package enjugit

import (
	"errors"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// BatchResult bundles the per-entry outcomes of a SubmitBatch
// call plus shared post-batch state. Entries is 1:1 with the
// reqs slice passed in, in input order, so callers can correlate
// by index without juggling task-id maps.
type BatchResult struct {
	Entries []BatchEntryResult

	// Branches is the per-branch summary (final HEAD SHA, push
	// outcome) of every distinct branch the batch touched. The
	// caller needs this to advance per-branch scan cursors and
	// for any post-batch read of "where did this group end up?".
	// Populated only after a successful commit phase; missing
	// for branches that never reached push.
	Branches []BatchBranchResult

	// PushAttempts is the number of push round-trips made (one
	// per touched branch in v1; the v1 implementation does not
	// retry on non-FF).
	PushAttempts int
}

// BatchBranchResult is the per-branch summary for a SubmitBatch
// call. One per distinct branch in the batch.
type BatchBranchResult struct {
	// Name is the branch ref short name (e.g.
	// "1-probe/develop_thing/iter-1").
	Name string

	// PreBatchHead is the branch's tip BEFORE this batch ran;
	// captured for rollback. Empty when the branch was created
	// fresh inside this batch.
	PreBatchHead string

	// FinalHeadSHA is the post-push tip. Empty when push was
	// skipped (no remote) — in that case the local tip is
	// still authoritative; caller can read it from any
	// resolved entry's CommitSHA.
	FinalHeadSHA string

	// Pushed is true when an actual network push completed (or
	// would have, but was gracefully skipped on no-remote).
	// False when push was attempted and failed.
	Pushed bool
}

// BatchEntryResult captures one batch entry's outcome.
type BatchEntryResult struct {
	TaskID string

	// BranchName is the branch this entry's commit landed on
	// (or would have, if Attempted). Set up-front from
	// effectiveBranch(req) so even un-Attempted entries report
	// where they were headed.
	BranchName string

	// CommitSHA is the post-push, post-rebase-aware SHA — read
	// from the Enju-Task-Complete trailer on the actual pushed
	// commit. Stable across rebases. Empty when Attempted=false
	// or when the trailer scan couldn't find this entry's commit
	// (caller falls back to the branch's FinalHeadSHA).
	CommitSHA string

	// NoOp is true when the would-be commit matched the worktree
	// before staging — caller policy whether to surface as
	// success or skip.
	NoOp bool

	// Err is non-nil when this entry didn't make it through the
	// batch successfully:
	//   - mid-loop commit failure (the failing entry, plus every
	//     earlier-committed entry that was rolled back)
	//   - push failure for this entry's branch
	// Always a *WorkflowOpError carrying the step trace. nil
	// when the entry's commit landed AND its branch pushed.
	Err error

	// Attempted is true when this entry's commit step ran
	// (success or fail). False for entries past a mid-loop
	// failure that were never staged.
	Attempted bool
}

// SubmitBatch atomically commits N submissions across one or
// more branches under one workspace lock. Designed for
// bulk-submit flows (paper-scale evaluation: bulk reviews,
// labeling cohorts) where N individual SubmitTaskResult calls
// would inflate tool-call latency without buying anything.
//
// Multi-branch support: reqs may target different branches
// (e.g. parallel develop tasks each with their own iteration
// topic). SubmitBatch groups internally by effective branch
// and processes one group at a time inside the lock.
//
// Atomicity / failure semantics:
//   - One WithLock spans the entire commit-all → push-all →
//     trailer-scan sequence. No other goroutine sees a partial
//     batch.
//   - Mid-loop commit failure (Phase 1) → ROLLBACK every
//     touched branch to its pre-batch HEAD, mark the failed
//     entry's Err AND every Attempted entry's Err with the
//     same *WorkflowOpError (they all got rolled back),
//     un-Attempted entries left as-is.
//   - Push failure (Phase 2) → no rollback. Already-pushed
//     branches stay pushed. Failed-branch entries get the
//     push error; entries on already-pushed branches are
//     considered successful.
//   - Trailer scan (Phase 3) is best-effort — missing trailers
//     leave CommitSHA empty so the caller can fall back to
//     BranchHeads.
//
// Trace shape (one outer SubmitBatch op, sequential phases):
//
//	SubmitBatch (entries=4, branches=2) {
//	  prepare-branch:1-r/task_a/iter-1: ok
//	  snapshot-head:1-r/task_a/iter-1:  ok (abc12345)
//	  entry[0] commit:1-r/task_a/iter-1: ok (def1)
//	  entry[2] commit:1-r/task_a/iter-1: ok (def3)
//	  prepare-branch:1-r/task_b/iter-1: ok
//	  snapshot-head:1-r/task_b/iter-1:  ok (abc12345)
//	  entry[1] commit:1-r/task_b/iter-1: ok (def2)
//	  entry[3] commit:1-r/task_b/iter-1: ok (def4)
//	  push:1-r/task_a/iter-1: ok
//	  push:1-r/task_b/iter-1: ok
//	  scan:1-r/task_a/iter-1: ok (2/2)
//	  scan:1-r/task_b/iter-1: ok (2/2)
//	}
func (w *Workflow) SubmitBatch(reqs []SubmitRequest) (*BatchResult, error) {
	if len(reqs) == 0 {
		return nil, fmt.Errorf("enjugit: SubmitBatch: reqs required")
	}
	// Validate each req up-front + compute its effective branch
	// so a bad req fails before the lock.
	branches := make([]string, len(reqs))
	for i, r := range reqs {
		if r.TaskID == "" {
			return nil, fmt.Errorf("enjugit: SubmitBatch: reqs[%d]: TaskID required", i)
		}
		if len(r.Files) == 0 {
			return nil, fmt.Errorf("enjugit: SubmitBatch: reqs[%d]: Files required", i)
		}
		b := effectiveBranch(w.convs, r)
		if b == "" {
			return nil, fmt.Errorf("enjugit: SubmitBatch: reqs[%d]: cannot resolve branch (need BranchOverride or RunSeq+RunSlug+TaskDef+IterSeq)", i)
		}
		branches[i] = b
	}

	// Group entries by branch, preserving relative input order
	// within each group. The slice order of groups follows the
	// first occurrence of each branch.
	type batchGroup struct {
		branch        string
		entryIdxs     []int
		preferredBase string // from the FIRST entry's RunBranch (review topics)
	}
	var groups []*batchGroup
	byBranch := map[string]*batchGroup{}
	for i, b := range branches {
		grp, ok := byBranch[b]
		if !ok {
			grp = &batchGroup{branch: b, preferredBase: reqs[i].RunBranch}
			groups = append(groups, grp)
			byBranch[b] = grp
		}
		grp.entryIdxs = append(grp.entryIdxs, i)
	}

	result := &BatchResult{
		Entries: make([]BatchEntryResult, len(reqs)),
	}
	for i := range result.Entries {
		result.Entries[i].TaskID = reqs[i].TaskID
		result.Entries[i].BranchName = branches[i]
	}

	trace := startTrace("SubmitBatch")
	trace.ctx("entries", fmt.Sprintf("%d", len(reqs)))
	trace.ctx("branches", fmt.Sprintf("%d", len(groups)))

	// touched tracks branches whose HEAD we moved, so a mid-
	// loop failure can rebuild the rollback set in reverse.
	// Anonymous-struct shape matches rollbackBatchMulti +
	// lookupPreHead so they can pass it through directly
	// without an extra named type.
	var touched []struct{ name, preHead string }

	werr := w.git.WithLock(func(g git.Ops) error {
		// Phase 1: commit-all per branch group. No network ops
		// here — pure local commits, fully rollback-safe.
		for _, grp := range groups {
			if err := w.prepareBranchForCommit(g, grp.branch, grp.preferredBase); err != nil {
				return rollbackBatchMulti(g, trace, touched,
					"prepare-branch:"+grp.branch, err, result, grp.entryIdxs[0])
			}
			trace.ok("prepare-branch:" + grp.branch)

			preHead, _, headErr := g.Head()
			if headErr != nil {
				preHead = ""
				trace.skipped("snapshot-head:"+grp.branch, "no HEAD yet (fresh branch)")
			} else {
				trace.okDetail("snapshot-head:"+grp.branch, shortSHA(preHead))
			}
			touched = append(touched, struct{ name, preHead string }{name: grp.branch, preHead: preHead})

			for _, idx := range grp.entryIdxs {
				commitRes, err := commitOneBatchEntry(g, w.convs, reqs[idx])
				if err != nil {
					return rollbackBatchMulti(g, trace, touched,
						fmt.Sprintf("entry[%d] commit:%s", idx, grp.branch),
						translateGitError("commit", err), result, idx)
				}
				result.Entries[idx].Attempted = true
				result.Entries[idx].NoOp = commitRes.NoOp
				result.Entries[idx].CommitSHA = commitRes.SHA // pre-push best-known; refined post-push
				stepName := fmt.Sprintf("entry[%d] commit:%s", idx, grp.branch)
				if commitRes.NoOp {
					trace.okDetail(stepName, "no-op")
				} else {
					trace.okDetail(stepName, shortSHA(commitRes.SHA))
				}
			}
		}

		// Phase 2: push every touched branch. Sequential within
		// the lock — N TCP round-trips, but still amortizes the
		// lock acquisition (the part that serializes across
		// concurrent clients). PushWithVerify so a silent
		// server-side reject (auth, ref-update hook) surfaces
		// as ErrSubmitVerify with branch + local + remote SHAs
		// — same audit pipeline single-submit uses. expectedSHA
		// is the last-committed entry's SHA in the group: by
		// Phase-1 invariant the branch tip == that SHA.
		// On the first push failure we stop and surface the
		// error; already-pushed branches remain pushed (no
		// remote-side rollback in v1).
		for _, grp := range groups {
			expectedSHA := result.Entries[grp.entryIdxs[len(grp.entryIdxs)-1]].CommitSHA
			if perr := g.PushWithVerify(grp.branch, expectedSHA); perr != nil {
				if errors.Is(perr, git.ErrNoRemote) {
					trace.skipped("push:"+grp.branch, "no remote configured (solo project)")
					result.Branches = append(result.Branches, BatchBranchResult{
						Name:         grp.branch,
						PreBatchHead: lookupPreHead(touched, grp.branch),
						Pushed:       true, // local-only is the source of truth here
					})
					continue
				}
				wErr := trace.fail("push:"+grp.branch, translateGitError("push verify", perr))
				// Flag every entry in THIS branch's group with
				// the push error. Entries in already-pushed
				// branches stay clean — their commits did go
				// out. The caller sees a partial-success batch.
				for _, idx := range grp.entryIdxs {
					if result.Entries[idx].Attempted {
						result.Entries[idx].Err = wErr
					}
				}
				result.Branches = append(result.Branches, BatchBranchResult{
					Name:         grp.branch,
					PreBatchHead: lookupPreHead(touched, grp.branch),
					Pushed:       false,
				})
				return wErr
			}
			trace.ok("push:" + grp.branch)
			result.Branches = append(result.Branches, BatchBranchResult{
				Name:         grp.branch,
				PreBatchHead: lookupPreHead(touched, grp.branch),
				Pushed:       true,
			})
		}
		result.PushAttempts = len(groups)

		// Phase 3: per-branch trailer scan. Walk each group's
		// branch tip backward, resolve task_id → SHA via the
		// Enju-Task-Complete trailer (rebase-stable). Best-
		// effort: missing trailers leave CommitSHA at its
		// pre-push value, which the caller can override with
		// the branch's FinalHeadSHA.
		for grpIdx, grp := range groups {
			if cerr := g.Checkout(grp.branch); cerr != nil {
				trace.skipped("scan:"+grp.branch, "checkout failed: "+cerr.Error())
				continue
			}
			finalHead, _, _ := g.Head()
			result.Branches[grpIdx].FinalHeadSHA = finalHead

			wantSet := make(map[string]int, len(grp.entryIdxs))
			for _, idx := range grp.entryIdxs {
				wantSet[reqs[idx].TaskID] = idx
			}
			maxWalk := len(grp.entryIdxs)*2 + 16
			found := 0
			_ = g.WalkRecentCommits(maxWalk, func(sha, message string) bool {
				t := ParseEnjuTrailers(message)
				if t.TaskID == "" {
					return true
				}
				idx, ok := wantSet[t.TaskID]
				if !ok {
					return true
				}
				result.Entries[idx].CommitSHA = sha
				delete(wantSet, t.TaskID)
				found++
				return len(wantSet) > 0
			})
			trace.okDetail("scan:"+grp.branch, fmt.Sprintf("%d/%d", found, len(grp.entryIdxs)))
		}
		return nil
	})
	if werr != nil {
		return result, werr
	}
	return result, nil
}

// commitOneBatchEntry composes the commit message + identity for
// one batch entry and stages it via CommitFiles. Returns the
// CommitResult straight from git.Ops so the caller can record
// SHA + NoOp.
func commitOneBatchEntry(g git.Ops, convs Conventions, req SubmitRequest) (git.CommitResult, error) {
	subject := fmt.Sprintf("Task %s by @%s: %s",
		req.TaskID, sanitizeIdent(req.Citizen.Name), shortSubjectFromVerdict(req.Verdict))
	body := buildSubmitBody(req)
	trailers := buildSubmitTrailers(req)
	message := composeCommitMessage(convs, subject, body, trailers)

	authorName := req.Citizen.Name
	if authorName == "" {
		authorName = convs.SystemAuthor.Name
	}
	authorEmail := req.Citizen.Email
	if authorEmail == "" {
		authorEmail = convs.SystemAuthor.Email
	}

	stagePaths := make([]string, len(req.Files))
	for i, f := range req.Files {
		stagePaths[i] = f.RepoRelPath
	}
	return g.CommitFiles(git.CommitRequest{
		Files:       req.Files,
		StagePaths:  stagePaths,
		Message:     message,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
	})
}

// rollbackBatchMulti is the failure funnel for Phase-1 (commit-
// time) errors. Records the failing step, rolls every touched
// branch back to its pre-batch HEAD (in reverse order so newer
// branches reset first), records each rollback step in the
// trace, and tags every Attempted entry's Err with the
// resulting *WorkflowOpError so the caller can render a
// uniform "this batch was rolled back" signal across all
// affected entries.
//
// touched is the list of branches we've actually moved (in
// processing order). For each, we SetBranchTo preHead — this
// covers both "had a head before" and "branch existed before"
// cases. Branches with empty preHead (created fresh inside the
// batch) get a skipped step rather than an attempted reset.
func rollbackBatchMulti(g git.Ops, trace *stepTrace, touched []struct{ name, preHead string }, failStep string, cause error, result *BatchResult, failedIdx int) error {
	trace.steps = append(trace.steps, Step{
		Name: failStep, Status: "failed",
		Detail: cause.Error(),
	})
	result.Entries[failedIdx].Attempted = true

	// Roll back in reverse processing order — symmetric with
	// how we touched them. In a multi-branch batch this means
	// the most-recently-touched branch is reset first.
	for i := len(touched) - 1; i >= 0; i-- {
		t := touched[i]
		if t.preHead == "" {
			trace.skipped("rollback:"+t.name, "no pre-batch HEAD (fresh branch)")
			continue
		}
		if rerr := g.SetBranchTo(t.name, t.preHead); rerr != nil {
			trace.steps = append(trace.steps, Step{
				Name: "rollback:" + t.name, Status: "failed",
				Detail: fmt.Sprintf("set-branch-to %s: %v", shortSHA(t.preHead), rerr),
			})
			continue
		}
		trace.okDetail("rollback:"+t.name, "reset to "+shortSHA(t.preHead))
	}
	// Leave HEAD on the FIRST touched branch (consistent
	// post-rollback state — operator's worktree reflects the
	// project's home/run branch, not the last-attempted topic).
	if len(touched) > 0 {
		_ = g.Checkout(touched[0].name)
	}

	wErr := trace.wrapTerminal(cause)

	// Propagate the same wErr to every Attempted entry — they
	// were committed but rolled back, so from the caller's
	// perspective they all "failed in the same way" (the batch
	// as a whole). The trace tells the operator which step
	// actually broke; per-entry Err is a uniform signal.
	for i := range result.Entries {
		if result.Entries[i].Attempted {
			result.Entries[i].Err = wErr
		}
	}
	return wErr
}

// lookupPreHead returns the pre-batch HEAD for a named branch
// from the touched list. Used when populating BatchBranchResult
// in Phase 2 so per-branch summaries carry the rollback anchor.
func lookupPreHead(touched []struct{ name, preHead string }, branch string) string {
	for _, t := range touched {
		if t.name == branch {
			return t.preHead
		}
	}
	return ""
}

// effectiveBranch returns the branch a SubmitRequest will land
// on: BranchOverride wins (vote/review direct-to-run-branch),
// else Conventions.BranchName composes the topic name. Used
// up-front in SubmitBatch so reqs can be grouped by branch
// before any git work.
func effectiveBranch(convs Conventions, req SubmitRequest) string {
	if req.BranchOverride != "" {
		return req.BranchOverride
	}
	return convs.BranchName(req.RunSeq, req.RunSlug, req.TaskDef, req.InstanceKey, req.IterSeq)
}
