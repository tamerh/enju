package enjugit

// producing_plumbing.go — no-checkout submit path.
//
// SubmitComputeTaskResult is the parallel-safe sibling of
// SubmitTaskResult. It uses git.Clone.PlumbingCommit +
// UpdateRef instead of CommitFiles, so N goroutines on the same
// Workflow can each submit a compute task concurrently without
// fighting over HEAD, .git/index, or the working tree.
//
// Use this for compute tasks. Use SubmitTaskResult for LLM/bot
// tasks (which run in their own per-bot clone and need the
// porcelain checkout flow).

import (
	"fmt"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

// SubmitComputeTaskResult builds a commit on a per-task topic
// branch via plumbing primitives (no checkout, no HEAD move) and
// pushes the topic branch to origin. Returns the commit SHA +
// branch name so coord can record the submission and trigger
// auto-merge into the run branch.
//
// Branch policy:
//   - Topic branch name = w.convs.BranchName(req.RunSeq, req.RunSlug,
//     req.TaskDef, req.InstanceKey, req.IterSeq) by default; req.BranchOverride
//     wins when non-empty. The caller (compute.Run) typically passes
//     the run branch resolved from the task; this method composes
//     the topic name from the run-* + iter-* fields above.
//   - Base SHA = run branch's local tip. The commit forks from there
//     so the eventual auto-merge into the run branch is fast-forward
//     when no sibling has merged in between (cleanest case).
//
// Concurrency:
//   - PlumbingCommit does NOT acquire c.lock(). Multiple goroutines
//     can build commits in parallel; the object store handles writes
//     safely (content-addressed paths).
//   - UpdateRef DOES acquire c.lock() (briefly). Refs serialize.
//   - Push acquires c.lock() per branch. Concurrent pushes to
//     DIFFERENT branches serialize at the network layer but each
//     push targets its own ref so they don't compete on origin.
//
// Failure modes:
//   - Empty req.RunBranch → caller bug, returns error.
//   - Empty req.Files → no work to commit.
//   - Run branch ref doesn't exist locally → returns ErrRefNotFound.
//   - Push fails → topic branch ref is created locally but not on
//     origin. Operator can retry (PushTopicBranch is idempotent).
func (w *Workflow) SubmitComputeTaskResult(req SubmitRequest) (*SubmitResult, error) {
	if req.TaskID == "" {
		return nil, fmt.Errorf("enjugit: SubmitComputeTaskResult: TaskID required")
	}
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("enjugit: SubmitComputeTaskResult: Files required")
	}
	if req.RunBranch == "" {
		return nil, fmt.Errorf("enjugit: SubmitComputeTaskResult: RunBranch required " +
			"(plumbing path needs an explicit base; compute callers always know the run branch)")
	}

	branchName := req.BranchOverride
	if branchName == "" {
		branchName = w.convs.BranchName(req.RunSeq, req.RunSlug, req.TaskDef, req.InstanceKey, req.IterSeq)
	}

	// Compose commit message exactly like SubmitTaskResult so the
	// commit's trailers + subject are byte-identical between the
	// porcelain and plumbing paths. Audit tools downstream parse
	// trailers without caring which path produced the commit.
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

	// Convert []FileWrite to git.FileWrite slice (same shape, just
	// aliased in enjugit's namespace; PlumbingCommit takes the
	// git package's type).
	gitFiles := make([]git.FileWrite, len(req.Files))
	for i, f := range req.Files {
		gitFiles[i] = git.FileWrite(f)
	}

	trace := startTrace("SubmitComputeTaskResult")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("task_id", req.TaskID)
	trace.ctx("topic_branch", branchName)
	trace.ctx("run_branch", req.RunBranch)
	trace.ctx("iter_seq", fmt.Sprintf("%d", req.IterSeq))

	// Step 1: resolve base SHA. When the topic branch already
	// exists locally (re-iteration after request_changes — Phase
	// 6c keeps iter_seq stable so the topic branch name doesn't
	// change), fork the new commit from the EXISTING topic tip,
	// not the run branch. Otherwise the new commit's parent
	// chain has nothing in common with origin's topic ref → push
	// rejected as non-fast-forward.
	//
	// First iteration: no local topic ref, fall back to the run
	// branch as before.
	var baseSHA string
	var baseSource string
	if topicSHA, terr := w.git.LocalBranchHash(branchName); terr == nil && topicSHA != "" {
		baseSHA = topicSHA
		baseSource = "topic"
	} else {
		runSHA, err := w.git.LocalBranchHash(req.RunBranch)
		if err != nil {
			return nil, trace.fail("resolve-base", fmt.Errorf("read run branch %s: %w", req.RunBranch, err))
		}
		if runSHA == "" {
			return nil, trace.fail("resolve-base", fmt.Errorf("run branch %s has no local ref (caller must ensure run branch exists)", req.RunBranch))
		}
		baseSHA = runSHA
		baseSource = "run"
	}
	trace.okDetail("resolve-base", baseSource+":"+baseSHA[:8])

	// Step 2: build commit via plumbing (no checkout, no HEAD move).
	commitSHA, err := w.git.PlumbingCommit(git.PlumbingCommitRequest{
		BaseSHA:     baseSHA,
		Files:       gitFiles,
		Message:     message,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
	})
	if err != nil {
		return nil, trace.fail("plumbing-commit", translateGitError("plumbing commit", err))
	}
	trace.okDetail("plumbing-commit", commitSHA[:8])

	// Step 3: update topic-branch ref. expectedOldSHA = "" allows
	// either creating the ref (first iteration) or replacing it
	// (re-iteration after request_changes — caller passes
	// IterSeq=N+1 so branch name is distinct anyway, but we don't
	// CAS-guard because there's no concurrent writer for THIS
	// specific ref name in v1).
	if err := w.git.UpdateRef(branchName, commitSHA, ""); err != nil {
		return nil, trace.fail("update-ref", translateGitError("update topic ref", err))
	}
	trace.ok("update-ref")

	// Step 4: push topic branch with verify. Skipped when origin
	// is unset — the local update-ref above already committed the
	// branch into this clone's object store, which IS the single
	// store in the single-machine no-origin shape. Sharing with
	// other citizens is a separate concern (sync model) that only
	// applies when origin points at a real remote.
	//
	// Push acquires lock internally; concurrent pushes to
	// DIFFERENT branches are safe (each pushes its own ref).
	// Verify catches the silent-push-skip pattern that's bitten
	// compute submits before.
	if w.git.RemoteURL() == "" {
		trace.okDetail("push-verify", "skipped: no origin")
	} else {
		if err := w.git.PushWithVerify(branchName, commitSHA); err != nil {
			return nil, trace.fail("push-verify", translateGitError("push verify", err))
		}
		trace.ok("push-verify")
	}

	return &SubmitResult{
		CommitSHA:    commitSHA,
		BranchName:   branchName,
		PushAttempts: 1,
		NoOp:         false, // plumbing-commit always produces a commit (even if files match — the new commit just has a new SHA from the new timestamp)
	}, nil
}

// CommitArbitraryFilesPlumbing is the parallel-safe sibling of
// CommitArbitraryFiles. Writes a commit to req.Branch using
// PlumbingCommit + UpdateRef + Push — no checkout, no HEAD move,
// no worktree mutation. N goroutines on the same Workflow can
// each call this concurrently for DIFFERENT branches without
// the worktree-race that bit concurrent create_run snapshots
// (each create_run wanted its own run-branch checked out
// simultaneously; only the last one's files survived on disk).
//
// Same input shape and message-composition as the porcelain
// CommitArbitraryFiles — caller code switching from one to the
// other doesn't have to translate trailers or recompose the
// subject. Only the on-disk side effect differs: porcelain
// leaves the worktree on req.Branch; this method leaves the
// worktree exactly as it found it.
//
// Branch resolution:
//   - req.Branch is required (no default-branch fallback — the
//     caller MUST know which branch this writes to, since the
//     point of this verb is concurrent writes to distinct branches).
//   - Branch must already exist (locally or as origin tracking).
//     create_run's flow always creates the run branch on the
//     coord side BEFORE calling this; the run branch is then
//     visible via origin tracking after fetch.
//
// Returns the new commit SHA. NoOp is never true here — the
// plumbing path always produces a commit object (no
// content-equal short-circuit; commit timestamps differ even
// for identical trees).
//
// Failure modes:
//   - Empty req.Files → error.
//   - Empty req.Branch → error.
//   - Branch not resolvable locally or on origin → error.
//   - Push failure → returns error WITH the local commit having
//     landed (operator's retry policy applies). The trace
//     records "push: failed" alongside the local commit SHA.
func (w *Workflow) CommitArbitraryFilesPlumbing(req CommitArbitraryFilesRequest) (*CommitArbitraryFilesResult, error) {
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("enjugit: CommitArbitraryFilesPlumbing: Files required")
	}
	if req.Branch == "" {
		return nil, fmt.Errorf("enjugit: CommitArbitraryFilesPlumbing: Branch required (plumbing path won't pick a default)")
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

	// Convert []FileWrite → git.FileWrite (same shape, just
	// re-typed for the gitcli package).
	gitFiles := make([]git.FileWrite, len(req.Files))
	for i, f := range req.Files {
		gitFiles[i] = git.FileWrite(f)
	}

	trace := startTrace("CommitArbitraryFilesPlumbing")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("branch", req.Branch)
	trace.ctx("subject", subject)

	// Step 1: resolve base SHA for the new commit. Prefer the
	// LOCAL branch tip; fall back to refs/remotes/origin/<branch>
	// for the cross-citizen case where the run branch was created
	// by another caller and lives only on origin. LocalBranchHash
	// already implements this preference order.
	baseSHA, err := w.git.LocalBranchHash(req.Branch)
	if err != nil {
		return nil, trace.fail("resolve-base", translateGitError("resolve branch tip", err))
	}
	if baseSHA == "" {
		return nil, trace.fail("resolve-base",
			fmt.Errorf("branch %q has no local or origin-tracking ref (caller must ensure branch exists before plumbing-commit)", req.Branch))
	}
	trace.okDetail("resolve-base", baseSHA[:8])

	// Step 2: build commit via plumbing. No HEAD/index/worktree
	// mutation. Concurrent goroutines on the same Clone can do
	// this in parallel — git's object store is content-addressed
	// and the temp-index-file path (GIT_INDEX_FILE) keeps the
	// real index untouched.
	commitSHA, err := w.git.PlumbingCommit(git.PlumbingCommitRequest{
		BaseSHA:     baseSHA,
		Files:       gitFiles,
		Message:     message,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
	})
	if err != nil {
		return nil, trace.fail("plumbing-commit", translateGitError("plumbing commit", err))
	}
	trace.okDetail("plumbing-commit", commitSHA[:8])

	// Step 3: advance the branch ref atomically. CAS guards
	// against a sibling goroutine racing to update the SAME
	// branch — that case is rare (each create_run writes to a
	// distinct run branch) but the CAS makes the race
	// observable rather than silent.
	if err := w.git.UpdateRef(req.Branch, commitSHA, baseSHA); err != nil {
		return nil, trace.fail("update-ref", translateGitError("advance branch", err))
	}
	trace.ok("update-ref")

	// Step 4: push. Skipped when origin is unset — local update-ref
	// already committed into this clone's store. Returns the push
	// error so the caller knows when the local commit landed but
	// origin didn't — same shape as SubmitComputeTaskResult.
	if w.git.RemoteURL() == "" {
		trace.appendStep(Step{
			Name: "push", Status: "ok", Detail: "skipped: no origin",
		})
		return &CommitArbitraryFilesResult{CommitSHA: commitSHA}, nil
	}
	if perr := w.git.Push(req.Branch); perr != nil {
		trace.appendStep(Step{
			Name: "push", Status: "failed",
			Detail: perr.Error(),
		})
		w.logger.Warn("enjugit: plumbing arbitrary-files push failed; commit landed locally only",
			"branch", req.Branch, "error", perr)
		// Return the result anyway (commit is on disk locally);
		// the caller decides retry policy.
		return &CommitArbitraryFilesResult{CommitSHA: commitSHA}, nil
	}
	trace.ok("push")

	return &CommitArbitraryFilesResult{CommitSHA: commitSHA}, nil
}
