package enjugit

import (
	"errors"
	"fmt"
	"strings"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

// SubmitTaskResult atomically commits the task's files on its
// topic branch and pushes with verify. The branch is composed
// from req fields via Conventions.BranchName.
//
// Authorship: req.Citizen. Trailers: built from req via
// buildSubmitTrailers.
//
// PRECONDITION: the daemon has already prepared the worktree by
// calling startIterationBranch / resumeIterationBranch /
// materializeUpstreamForReview as appropriate. SubmitTaskResult
// switches to the composed branch (creating it if missing); if
// creation is required AND the daemon didn't pre-prepare,
// SubmitTaskResult forks from the run branch (the safe default).
//
// Atomicity: WithLock holds the lock across the entire sequence
// so no concurrent goroutine can observe a partial state.
//
// Git operations performed (under one WithLock):
//   1. Compose branch name from req.
//   2. Switch to branch (create from run-branch if missing).
//   3. Compose commit message + trailers.
//   4. CommitFiles → returns local SHA.
//   5. PushWithVerify(branch, localSHA).
//
// Worktree state: Pre StateClean → Post StateClean (matches new commit tree).
//
// Errors:
//   - ErrCannotForkBranch: branch missing AND no fork base resolvable.
//   - ErrSubmitVerifyFailed: push went out but remote ref didn't update.
//   - ErrPushNonFF: another commit landed since we started.
//   - ErrMergeConflict (unreachable here; submits don't merge).
func (w *Workflow) SubmitTaskResult(req SubmitRequest) (*SubmitResult, error) {
	if req.TaskID == "" {
		return nil, fmt.Errorf("enjugit: SubmitTaskResult: TaskID required")
	}
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("enjugit: SubmitTaskResult: Files required")
	}
	// Branch resolution: caller-supplied BranchOverride wins
	// (vote/review tasks land on the run branch directly), else
	// compose the topic-branch name from the run + iter fields.
	branchName := req.BranchOverride
	if branchName == "" {
		branchName = w.convs.BranchName(req.RunSeq, req.RunSlug, req.TaskDef, req.InstanceKey, req.IterSeq)
	}

	subject := fmt.Sprintf("Task %s by @%s: %s",
		req.TaskID, sanitizeIdent(req.Citizen.Name), shortSubjectFromVerdict(req.Verdict))
	body := buildSubmitBody(req)
	trailers := buildSubmitTrailers(req)
	message := composeCommitMessage(w.convs, subject, body, trailers)

	authorName := req.Citizen.Name
	if authorName == "" {
		authorName = w.convs.SystemAuthor.Name
	}
	authorEmail := req.Citizen.Email
	if authorEmail == "" {
		authorEmail = w.convs.SystemAuthor.Email
	}

	stagePaths := make([]string, len(req.Files))
	for i, f := range req.Files {
		stagePaths[i] = f.RepoRelPath
	}

	trace := startTrace("SubmitTaskResult")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("task_id", req.TaskID)
	trace.ctx("branch", branchName)
	trace.ctx("iter_seq", fmt.Sprintf("%d", req.IterSeq))

	var result SubmitResult
	werr := w.git.WithLock(func(g git.Ops) error {
		// Step 1: prepare-branch — full multi-step fallback
		// (fetch + local + origin-tracking + fork-from-preferred
		// + fork-from-default) with its own structured trace.
		// Caller-supplied RunBranch acts as the preferred fork
		// base when the topic doesn't yet exist: review submits
		// pass the upstream's iteration branch here so the
		// review's commit lands on top of the upstream content
		// (FF-merge-preserves-upstream invariant).
		if err := w.prepareBranchForCommit(g, branchName, req.RunBranch); err != nil {
			// Inner WorkflowOpError carries its own steps;
			// surface as our verb's "prepare-branch" failure
			// so the trace tells the operator which outer step
			// (vs which inner sub-step) we're in.
			return trace.fail("prepare-branch", err)
		}
		trace.ok("prepare-branch")

		// Step 2: commit.
		commitRes, err := g.CommitFiles(git.CommitRequest{
			Files:       req.Files,
			StagePaths:  stagePaths,
			Message:     message,
			AuthorName:  authorName,
			AuthorEmail: authorEmail,
		})
		if err != nil {
			return trace.fail("commit", translateGitError("commit", err))
		}
		result.CommitSHA = commitRes.SHA
		result.NoOp = commitRes.NoOp
		if commitRes.NoOp {
			trace.okDetail("commit", "no-op (worktree already matched)")
		} else {
			trace.okDetail("commit", commitRes.SHA[:8])
		}

		// Step 3: push-verify. The verify step catches the
		// "commit reported but never landed" failure mode
		// (TP53 Bug 1 reproducer). Origin always exists (managed
		// bare for path-mode projects, real remote otherwise).
		if perr := g.PushWithVerify(branchName, commitRes.SHA); perr != nil {
			return trace.fail("push-verify", translateGitError("push verify", perr))
		}
		trace.ok("push-verify")
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	result.BranchName = branchName
	result.PushAttempts = 1 // single attempt; no rebase loop in v1
	// TODO(enjugit-retry-loop): the project package's old
	// SubmitTaskResult had a fetch+reset+re-apply+re-push retry
	// loop here that recovered from concurrent-push non-FF
	// rejections. Dropping it was a deliberate v1 simplification,
	// but it shifts the "race recovery" responsibility onto
	// callers (or leaves clients to lose work on naive retries).
	// Revisit: either honor SubmitRequest.MaxRetries here with a
	// proper rebase loop, OR document that the service layer
	// MUST handle non-FF + retry and audit all callers. Either
	// way, restore the scenario covered by
	// TestSubmitTaskResult_ConcurrentPushSurfacesNonFFIntegration
	// (which currently asserts the post-drop behavior).
	return &result, nil
}

// MergeAcceptedTopic merges a topic branch into a target
// branch. Tries fast-forward first; falls back to a no-FF merge
// commit when the target has advanced past the topic's fork point.
//
// Authorship per spec:
//   - author.AutoOrManual == "auto": SystemAuthor (Conventions.SystemAuthor)
//   - author.AutoOrManual == "manual": author.Citizen (the merge_resolve
//     task's claiming citizen)
//
// Trailers (when a merge commit is created): Enju-Merge,
// Enju-Triggered-By per buildMergeTrailers.
//
// Git operations performed (under one WithLock):
//   1. Resolve target + topic SHAs (Fetch first).
//   2. Try MergeFFOrFail.
//   3. On non-FF: compose merge message + trailers, MergeWithCommit.
//   4. Push the new target tip.
//
// Worktree state: Pre any → Post StateClean (matches new target tip).
//
// Errors:
//   - ErrMergeConflict (carries paths via *ErrConflict): real file conflict.
//     Caller (service) spawns a merge_resolve task in response.
//   - ErrCannotAutoMerge: anything else that prevents the merge.
func (w *Workflow) MergeAcceptedTopic(topic, target string, author MergeAuthor) (*MergeResult, error) {
	if topic == "" || target == "" {
		return nil, fmt.Errorf("enjugit: MergeAcceptedTopic: topic and target required")
	}
	authorName, authorEmail := w.mergeAuthorIdentity(author)
	trailers := buildMergeTrailers(author)
	subject := fmt.Sprintf("Merge %s into %s", topic, target)
	message := composeCommitMessage(w.convs, subject, "", trailers)

	trace := startTrace("MergeAcceptedTopic")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("topic", topic)
	trace.ctx("target", target)
	trace.ctx("trigger_task", author.TaskID)
	trace.ctx("merge_kind", author.AutoOrManual)

	// topicSHA + targetSHA captured before any merge attempt
	// so a conflict's ErrConflict carries them for the audit
	// pipeline to spawn merge_resolve with full context.
	var topicSHA, targetSHA string
	result := &MergeResult{}
	werr := w.git.WithLock(func(g git.Ops) error {
		// Pre-merge: capture branch tips for conflict-context.
		// Best-effort — a missing ref leaves SHAs empty and the
		// caller's report has minimal info; the merge attempt
		// itself will surface the real failure.
		if sha, err := g.ResolveRef(topic); err == nil {
			topicSHA = sha
		}
		if sha, err := g.ResolveRef(target); err == nil {
			targetSHA = sha
		}
		// Step 1: pre-merge fetch. Skipped when origin is unset —
		// the post-Phase-8 single-store layout has no remote to
		// sync from; the local clone IS the source of truth.
		// When origin is configured, treat failures as best-effort
		// (offline blips recoverable; record without aborting).
		if w.git.RemoteURL() == "" {
			trace.okDetail("fetch-origin", "skipped: no origin")
		} else if err := g.Fetch(); err != nil {
			trace.appendStep(Step{
				Name: "fetch-origin", Status: "failed",
				Detail: err.Error(),
			})
			w.logger.Warn("enjugit: pre-merge fetch failed; continuing",
				"topic", topic, "target", target, "error", err)
		} else {
			trace.ok("fetch-origin")
		}

		// Step 2: try fast-forward FIRST (ref-only operation —
		// doesn't touch worktree). On success, target's ref
		// moves to topic's tip; we'll checkout target AFTER so
		// the worktree reflects the new tip (not the old).
		// Doing it in this order avoids the trap where a
		// pre-merge Checkout(target) would reset the worktree
		// to target's OLD state and the subsequent FF (which
		// is ref-only) wouldn't refresh it.
		if tip, err := g.MergeFFOrFail(target, topic); err == nil {
			result.NewTip = tip
			result.FastForwarded = true
			trace.okDetail("merge-ff", shortSHA(tip))
			// Step 3a: checkout target so HEAD + worktree
			// land on target's new tip. Without this, HEAD
			// stays on the topic branch the caller came in on
			// — subsequent ops (next iteration's fork, user's
			// manual commit) would target the wrong branch.
			// This is the lost-commit regression guard.
			//
			// Skip when HEAD is ALREADY on target: the compute
			// path enters MergeAcceptedTopic with HEAD on target
			// (the run branch), and after a ref-only FF the ref
			// is already updated — re-running the preserve-
			// checkout-restore dance just risks losing untracked
			// files in the worktree (the bug the post-FF
			// Checkout was meant to defend against, inverted in
			// this entry shape).
			_, headBranch, herr := g.Head()
			if herr == nil && headBranch == target {
				trace.skipped("checkout-target", "HEAD already on target")
				// Skipping the Checkout means the index isn't
				// refreshed via the standard checkout path.
				// After the FF, the index still reflects the
				// pre-merge tree, which would make the next
				// checkout's preserve walk misclassify newly-
				// committed files as untracked. Force the index
				// in sync with HEAD's new tip explicitly.
				if rerr := g.SyncIndexToHead(); rerr != nil {
					return trace.fail("sync-index", translateGitError("sync index to head", rerr))
				}
				trace.ok("sync-index")
			} else {
				if cerr := g.Checkout(target); cerr != nil {
					return trace.fail("checkout-target", translateGitError("checkout merge target", cerr))
				}
				trace.ok("checkout-target")
			}
			// Step 4a: push the FF'd target. Skipped when origin
			// is unset — the local ref update IS the final state
			// in the single-store layout (post-Phase-8). Sharing
			// with other citizens is a separate concern handled
			// when origin points at a real remote.
			if w.git.RemoteURL() == "" {
				trace.okDetail("push", "skipped: no origin")
				return nil
			}
			if perr := g.Push(target); perr != nil {
				return trace.fail("push", translateGitError("push", perr))
			}
			trace.ok("push")
			return nil
		} else if !errors.Is(err, git.ErrPushNonFF) {
			// Real failure (not "non-FF"; non-FF is the
			// expected fall-through to merge-commit).
			return trace.fail("merge-ff", translateGitError("merge ff", err))
		}
		trace.skipped("merge-ff", "non-fast-forward; falling back to merge commit")

		// Step 3b: non-FF — real merge commit.
		tip, err := g.MergeWithCommit(target, topic, message, authorName, authorEmail)
		if err != nil {
			// Enrich the conflict with full audit context
			// before bubbling up. Service's reportMergeConflict
			// reads these fields directly to populate the
			// coordinator-side merge_conflict_detected event.
			translated := translateGitError("merge with commit", err)
			var conflict *ErrConflict
			if errors.As(translated, &conflict) {
				conflict.Branch = target
				conflict.TopicBranch = topic
				conflict.TopicCommit = topicSHA
				conflict.RunTipCommit = targetSHA
			}
			return trace.fail("merge-commit", translated)
		}
		result.NewTip = tip
		result.FastForwarded = false
		trace.okDetail("merge-commit", shortSHA(tip))

		// Step 4b: checkout target so HEAD + worktree land on
		// the new merge commit. Same rationale as the FF path's
		// Step 3a — without this, HEAD stays on the source
		// topic branch and the worktree reflects pre-merge state.
		if cerr := g.Checkout(target); cerr != nil {
			return trace.fail("checkout-target", translateGitError("checkout merge target", cerr))
		}
		trace.ok("checkout-target")

		// Step 5: push the new target tip. Skipped when origin
		// is unset — same rationale as Step 4a above.
		if w.git.RemoteURL() == "" {
			trace.okDetail("push", "skipped: no origin")
			return nil
		}
		if perr := g.Push(target); perr != nil {
			return trace.fail("push", translateGitError("push", perr))
		}
		trace.ok("push")
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	return result, nil
}

// CommitArbitraryFiles commits a set of files to a target
// branch. Used for non-task commits — diagram exports, event
// timeline JSONLs, README updates — anything that belongs in
// project history but isn't a task submission.
//
// Atomic: branch switch + commit + push happen under one
// WithLock. Empty req.Branch falls back to the workflow's
// default branch. Empty author falls back to SystemAuthor.
//
// Git operations performed (under one WithLock):
//  1. prepare-branch: resolve target + checkout (full multi-step
//     fallback via prepareBranchForCommit, no preferred base).
//  2. commit: stage + commit the files.
//  3. push: best-effort. Push failure is non-fatal (the local
//     commit is the source of truth; offline retry is the
//     caller's policy) — surfaced in the trace as a
//     non-blocking failed step.
//
// Worktree state: Pre any → Post StateClean.
//
// Errors (returned as *WorkflowOpError carrying the trace):
//   - ErrCannotForkBranch: branch missing AND no default branch.
//   - any commit-time git error translated via translateGitError.
//
// Push errors do NOT fail the verb (returned commit reflects the
// local landing); they appear in the trace as a "push: failed"
// step for the operator's audit pipeline.
func (w *Workflow) CommitArbitraryFiles(req CommitArbitraryFilesRequest) (*CommitArbitraryFilesResult, error) {
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("enjugit: CommitArbitraryFiles: Files required")
	}
	branch := req.Branch
	if branch == "" {
		branch = w.DefaultBranch()
	}
	authorName := req.AuthorName
	if authorName == "" {
		authorName = w.convs.SystemAuthor.Name
	}
	authorEmail := req.AuthorEmail
	if authorEmail == "" {
		authorEmail = w.convs.SystemAuthor.Email
	}
	subject := req.Subject
	if subject == "" {
		subject = "Workspace update"
	}

	trailers := map[string]string{}
	if req.ModelName != "" {
		trailers["AI-Model"] = req.ModelName
		if co := aiCoAuthorTrailer(req.ModelName); co != "" {
			trailers["Co-Authored-By"] = co
		}
	}
	for k, v := range req.CustomTrailers {
		trailers[k] = v
	}
	message := composeCommitMessage(w.convs, subject, req.Body, trailers)

	stagePaths := make([]string, len(req.Files))
	for i, f := range req.Files {
		stagePaths[i] = f.RepoRelPath
	}

	trace := startTrace("CommitArbitraryFiles")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("branch", branch)
	trace.ctx("subject", subject)

	var result CommitArbitraryFilesResult
	werr := w.git.WithLock(func(g git.Ops) error {
		// Step 1: prepare-branch — same multi-step fallback
		// SubmitTaskResult uses, no preferred fork base (this
		// verb is for export-class commits, not review topics).
		if err := w.prepareBranchForCommit(g, branch, ""); err != nil {
			return trace.fail("prepare-branch", err)
		}
		trace.ok("prepare-branch")

		// Step 2: commit.
		commitRes, err := g.CommitFiles(git.CommitRequest{
			Files:       req.Files,
			StagePaths:  stagePaths,
			Message:     message,
			AuthorName:  authorName,
			AuthorEmail: authorEmail,
		})
		if err != nil {
			return trace.fail("commit", translateGitError("commit", err))
		}
		result.CommitSHA = commitRes.SHA
		result.NoOp = commitRes.NoOp
		if commitRes.NoOp {
			trace.okDetail("commit", "no-op (worktree already matched)")
		} else {
			trace.okDetail("commit", shortSHA(commitRes.SHA))
		}

		// Step 3: push (best-effort). Offline blips are caller-
		// policy to retry; trace records the failure without
		// aborting the verb. Skipped when origin is unset —
		// local update-ref already committed into the single
		// store (post-Phase-8 layout).
		if w.git.RemoteURL() == "" {
			trace.okDetail("push", "skipped: no origin")
		} else if perr := g.Push(branch); perr != nil {
			trace.appendStep(Step{
				Name: "push", Status: "failed",
				Detail: perr.Error(),
			})
			w.logger.Warn("enjugit: arbitrary-files push failed; commit landed locally only",
				"branch", branch, "error", perr)
		} else {
			trace.ok("push")
		}
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	return &result, nil
}

// FetchAllRefs is a passthrough for git.Fetch. Wraps with
// Enju-typed errors. Used by daemon's pre-claim pull so
// cross-citizen refs are visible.
func (w *Workflow) FetchAllRefs() error {
	return translateGitError("fetch all refs", w.git.Fetch())
}

// ReadFileAtCommit reads a file's content at a specific commit,
// with lazy-fetch in the underlying git layer. Passthrough so
// service callers never touch git directly.
func (w *Workflow) ReadFileAtCommit(sha, path string) ([]byte, bool, error) {
	body, found, err := w.git.ReadFileAtCommit(sha, path)
	return body, found, translateGitError("read at commit", err)
}

// Head returns HEAD's commit SHA + branch name. Branch is "" when
// HEAD is detached. Passthrough so service callers can read the
// post-prepare HEAD without reaching into git.
func (w *Workflow) Head() (sha, branch string, err error) {
	sha, branch, err = w.git.Head()
	return sha, branch, translateGitError("head", err)
}

// LogFile returns commits that touched relPath in the local clone,
// newest-first. Native enjugit.CommitInfo (not a type alias) so the
// service-layer view of "commit history" doesn't depend on
// internal/git's struct shape — internal git changes can't ripple
// out without an explicit translation step here.
func (w *Workflow) LogFile(relPath string) ([]CommitInfo, error) {
	out, err := w.git.LogFile(relPath)
	if err != nil {
		return nil, translateGitError("log file", err)
	}
	res := make([]CommitInfo, len(out))
	for i, c := range out {
		res[i] = CommitInfo{
			Hash:    c.Hash,
			Message: c.Message,
			Author:  c.Author,
			Time:    c.Time,
		}
	}
	return res, nil
}

// mergeAuthorIdentity picks (name, email) for an auto-merge
// commit per spec: system for "auto", citizen for "manual".
func (w *Workflow) mergeAuthorIdentity(author MergeAuthor) (string, string) {
	if author.AutoOrManual == "manual" && author.Citizen.Name != "" {
		return author.Citizen.Name, author.Citizen.Email
	}
	return w.convs.SystemAuthor.Name, w.convs.SystemAuthor.Email
}

// shortSubjectFromVerdict returns a short subject suffix
// reflecting the verdict, or "result" when no verdict applies.
func shortSubjectFromVerdict(verdict string) string {
	switch verdict {
	case "approve":
		return "approve"
	case "reject":
		return "reject"
	case "request_changes":
		return "request changes"
	default:
		return "result"
	}
}

// buildSubmitBody composes the body portion of the commit
// message — currently just lists artifact paths when present.
// Future: include task type, run summary, etc.
func buildSubmitBody(req SubmitRequest) string {
	if len(req.ArtifactPaths) == 0 {
		return ""
	}
	return "Artifacts: " + strings.Join(req.ArtifactPaths, ", ")
}

// sanitizeIdent strips characters that would mangle commit-message
// formatting (newlines, control chars). For citizen names that
// might come from external sources.
func sanitizeIdent(name string) string {
	if name == "" {
		return "anonymous"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
}
