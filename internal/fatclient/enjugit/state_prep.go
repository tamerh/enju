package enjugit

import (
	"errors"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// materializeUpstreamForReview puts the upstream task's tip on
// disk in detached HEAD state. NO local branch ref is created or
// modified. Used by review tasks before claude -p reads the
// developer's content.
//
// upstreamBranch is the name of the upstream task's topic branch
// (e.g. "1-build/develop_a/iter-1"). Resolved via origin's tip.
//
// Git operations performed (under one WithLock):
//   1. Fetch origin (so origin/<upstreamBranch> is current).
//   2. ResolveRef(<upstreamBranch>) — finds origin/<upstreamBranch>'s SHA.
//   3. CheckoutCommit(SHA) — detached HEAD on that commit.
//
// Worktree state: Pre any → Post StateDetached, files match the
// upstream's tree exactly.
//
// Errors:
//   - ErrUpstreamNotFound: upstreamBranch doesn't exist on origin.
//   - any git error translated via translateGitError.
func (w *Workflow) materializeUpstreamForReview(upstreamBranch string) error {
	if upstreamBranch == "" {
		return fmt.Errorf("enjugit: materializeUpstreamForReview: upstreamBranch is required")
	}
	trace := startTrace("materializeUpstreamForReview")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("upstream_branch", upstreamBranch)

	werr := w.git.WithLock(func(g git.Ops) error {
		// Step 1: fetch (best-effort).
		if err := g.Fetch(); err != nil {
			if errors.Is(err, git.ErrNoRemote) {
				trace.skipped("fetch-origin", "no remote configured")
			} else {
				trace.appendStep(Step{
					Name: "fetch-origin", Status: "failed",
					Detail: err.Error(),
				})
				w.logger.Warn("enjugit: pre-materialize fetch failed; continuing with cached refs",
					"branch", upstreamBranch, "error", err)
			}
		} else {
			trace.ok("fetch-origin")
		}

		// Step 2: resolve upstream tip.
		sha, err := g.ResolveRef(upstreamBranch)
		if err != nil {
			if errors.Is(err, git.ErrRefNotFound) {
				return trace.fail("resolve-upstream",
					fmt.Errorf("%w: %s", ErrUpstreamNotFound, upstreamBranch))
			}
			return trace.fail("resolve-upstream", translateGitError("resolve upstream", err))
		}
		trace.okDetail("resolve-upstream", shortSHA(sha))

		// Step 3: detached checkout onto upstream's tree.
		if err := g.CheckoutCommit(sha); err != nil {
			return trace.fail("checkout-detached", translateGitError("checkout upstream commit", err))
		}
		trace.ok("checkout-detached")
		return nil
	})
	return werr
}

// startIterationBranch creates a fresh iter-N topic branch
// according to ForkPoint policy. Branch name composed from
// Conventions.BranchName.
//
// Inputs:
//   - taskID: full task ID (for log lines, not the branch name)
//   - iterSeq: 1-based attempt number; goes into the branch name
//   - fork: where the new branch forks from (run branch / upstream
//     topic / prior iteration)
//   - taskDef, instanceKey, runSeq, runSlug: drive Conventions.BranchName
//   - runBranch, upstreamBranch: fork-base sources (used per ForkPoint)
//
// Git operations performed (under one WithLock):
//   1. Compose branch name via Conventions.BranchName.
//   2. Resolve fork-base SHA per ForkPoint (Fetch first if needed).
//   3. CreateBranchAt(name, forkSHA) — errors if branch exists.
//   4. Checkout(name) — switches HEAD + worktree.
//
// Worktree state: Pre any → Post StateClean (matches new branch tip).
//
// Errors:
//   - ErrInvalidForkPoint: fork is ForkUnknown or unrecognized.
//   - ErrIterationBranchExists: branch already exists locally.
//   - ErrForkBaseNotFound: fork-base ref couldn't be resolved.
//   - any git error translated.
func (w *Workflow) startIterationBranch(
	taskID string,
	iterSeq int,
	fork ForkPoint,
	taskDef, instanceKey string,
	runSeq int,
	runSlug, runBranch, upstreamBranch string,
) (string, error) {
	if fork == ForkUnknown {
		return "", fmt.Errorf("%w: caller must specify ForkPoint", ErrInvalidForkPoint)
	}
	branchName := w.convs.BranchName(runSeq, runSlug, taskDef, instanceKey, iterSeq)
	trace := startTrace("startIterationBranch")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("task_id", taskID)
	trace.ctx("branch", branchName)
	trace.ctx("fork_point", fork.String())
	trace.ctx("iter_seq", fmt.Sprintf("%d", iterSeq))

	werr := w.git.WithLock(func(g git.Ops) error {
		// Step 1: pick fork-ref per ForkPoint.
		var forkRef string
		switch fork {
		case ForkFromRunBranch:
			forkRef = runBranch
		case ForkFromUpstreamTopic:
			forkRef = upstreamBranch
		case ForkFromPriorIteration:
			if iterSeq < 2 {
				return trace.fail("pick-fork-ref",
					fmt.Errorf("%w: ForkFromPriorIteration requires iterSeq>=2", ErrInvalidForkPoint))
			}
			forkRef = w.convs.BranchName(runSeq, runSlug, taskDef, instanceKey, iterSeq-1)
		default:
			return trace.fail("pick-fork-ref",
				fmt.Errorf("%w: %s", ErrInvalidForkPoint, fork))
		}
		if forkRef == "" {
			return trace.fail("pick-fork-ref",
				fmt.Errorf("%w: fork=%s", ErrForkBaseNotFound, fork))
		}
		trace.okDetail("pick-fork-ref", forkRef)

		// Step 2: fetch (best-effort) so cross-citizen refs are visible.
		if ferr := g.Fetch(); ferr != nil {
			if errors.Is(ferr, git.ErrNoRemote) {
				trace.skipped("fetch-origin", "no remote configured")
			} else {
				trace.appendStep(Step{
					Name: "fetch-origin", Status: "failed",
					Detail: ferr.Error(),
				})
				w.logger.Warn("enjugit: pre-fork fetch failed; continuing",
					"task_id", taskID, "fork_ref", forkRef, "error", ferr)
			}
		} else {
			trace.ok("fetch-origin")
		}

		// Step 3: resolve fork-base SHA.
		forkSHA, err := g.ResolveRef(forkRef)
		if err != nil {
			return trace.fail("resolve-fork-base",
				fmt.Errorf("%w: ref=%s: %v", ErrForkBaseNotFound, forkRef, err))
		}
		trace.okDetail("resolve-fork-base", shortSHA(forkSHA))

		// Step 4: create the new branch.
		if err := g.CreateBranchAt(branchName, forkSHA); err != nil {
			if errors.Is(err, git.ErrBranchExists) {
				return trace.fail("create-branch",
					fmt.Errorf("%w: %s", ErrIterationBranchExists, branchName))
			}
			return trace.fail("create-branch", translateGitError("create iter branch", err))
		}
		trace.ok("create-branch")

		// Step 5: switch HEAD + worktree onto it.
		if err := g.Checkout(branchName); err != nil {
			return trace.fail("checkout", translateGitError("checkout iter branch", err))
		}
		trace.ok("checkout")
		return nil
	})
	if werr != nil {
		return "", werr
	}
	return branchName, nil
}

// resumeIterationBranch switches to an existing iter-N topic
// branch (request_changes revision case where iter_seq stays the
// same). Errors if the branch doesn't exist locally.
//
// Auto-heals when the local ref disagrees with origin/<branch>:
// resets to origin's tip and switches. This is the stale-ref
// auto-heal we added in the project package, lifted into a
// proper workflow contract.
//
// Git operations performed:
//   1. Compose branch name via Conventions.BranchName.
//   2. Fetch (so origin tip is current).
//   3. Compare local ref vs origin: if disagree, SetBranchTo origin.
//   4. Checkout(name) — switches HEAD + worktree.
//
// Worktree state: Pre any → Post StateClean (matches branch tip).
//
// Errors:
//   - ErrIterationBranchMissing: no local ref for the iter branch.
func (w *Workflow) resumeIterationBranch(
	taskID string,
	iterSeq int,
	taskDef, instanceKey string,
	runSeq int,
	runSlug string,
) (string, error) {
	branchName := w.convs.BranchName(runSeq, runSlug, taskDef, instanceKey, iterSeq)
	trace := startTrace("resumeIterationBranch")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("task_id", taskID)
	trace.ctx("branch", branchName)
	trace.ctx("iter_seq", fmt.Sprintf("%d", iterSeq))

	werr := w.git.WithLock(func(g git.Ops) error {
		// Step 1: verify local ref exists. Resume requires the
		// branch to already exist locally — that's its contract.
		localSHA, err := g.ResolveRef("refs/heads/" + branchName)
		if err != nil {
			return trace.fail("resolve-local",
				fmt.Errorf("%w: %s", ErrIterationBranchMissing, branchName))
		}
		trace.okDetail("resolve-local", shortSHA(localSHA))

		// Step 2: fetch (best-effort) so we can compare with origin.
		if ferr := g.Fetch(); ferr != nil {
			if errors.Is(ferr, git.ErrNoRemote) {
				trace.skipped("fetch-origin", "no remote configured")
			} else {
				trace.appendStep(Step{
					Name: "fetch-origin", Status: "failed",
					Detail: ferr.Error(),
				})
				w.logger.Warn("enjugit: pre-resume fetch failed; continuing",
					"task_id", taskID, "branch", branchName, "error", ferr)
			}
		} else {
			trace.ok("fetch-origin")
		}

		// Step 3: auto-heal stale local ref. If origin disagrees,
		// reset local to origin's SHA. Surfaces in the trace so a
		// citizen who sees "wait, my local moved" can read what
		// happened.
		if originSHA, err := g.ResolveRef("refs/remotes/origin/" + branchName); err == nil {
			if originSHA != localSHA {
				w.logger.Warn("enjugit: local iter branch disagrees with origin; resetting to origin tip",
					"task_id", taskID, "branch", branchName,
					"local", localSHA, "origin", originSHA)
				if err := g.SetBranchTo(branchName, originSHA); err != nil {
					return trace.fail("auto-heal-stale-local",
						translateGitError("auto-heal SetBranchTo", err))
				}
				trace.okDetail("auto-heal-stale-local",
					"local "+shortSHA(localSHA)+" → origin "+shortSHA(originSHA))
			} else {
				trace.skipped("auto-heal-stale-local", "local matches origin")
			}
		} else {
			trace.skipped("auto-heal-stale-local", "no refs/remotes/origin/"+branchName)
		}

		// Step 4: checkout.
		if err := g.Checkout(branchName); err != nil {
			return trace.fail("checkout", translateGitError("checkout existing iter branch", err))
		}
		trace.ok("checkout")
		return nil
	})
	if werr != nil {
		return "", werr
	}
	return branchName, nil
}

// ResetCleanWorktree drops uncommitted modifications + untracked
// files. After this, State() == StateClean.
//
// Git operations performed: ResetClean.
//
// Worktree state: Pre any → Post StateClean.
func (w *Workflow) ResetCleanWorktree() error {
	return translateGitError("reset clean", w.git.ResetClean())
}

// EnsureRunBranch makes the run branch exist locally and on origin.
// Idempotent: a branch that already exists at either location is
// left alone. Called from the create_run path so subsequent
// claim/submit verbs can rely on the run branch being a real ref
// (not a coordinator-only string), which lets MergeFFOrFail stay
// strict instead of special-casing missing targets.
//
// Three cases:
//
//  1. branch already exists locally → no-op (covers `branch=<existing>`).
//  2. branch exists only on origin (another machine pushed it first)
//     → create local tracking ref pointing at origin's tip.
//  3. branch exists nowhere → fork from defaultBranch, push to
//     origin (best-effort on no-remote projects).
//
// Inputs:
//   - branch: the run branch name (e.g. "run-on-auto-branch-1").
//   - defaultBranch: where to fork a brand-new branch from
//     (e.g. "main"). Only consulted in case 3.
//
// Git operations performed (under one WithLock):
//
//  1. Fetch (best-effort — pulls remote refs into refs/remotes/origin/*).
//  2. ResolveRef("refs/heads/<branch>") — local check.
//  3. ResolveRef("refs/remotes/origin/<branch>") — origin check
//     (only when local missed).
//  4. ResolveRef(defaultBranch) — fork-base lookup
//     (only when both local and origin missed).
//  5. CreateBranchAt(branch, baseSHA) — points the local ref.
//  6. Push(branch) — establishes the ref on origin
//     (only in case 3; skipped on no-remote).
//
// Worktree state: unchanged (this is a ref-only operation).
//
// Errors:
//   - ErrForkBaseNotFound: defaultBranch can't be resolved when
//     branch is brand-new. Caller's project has no commits yet —
//     should seed (`enju_create_project` does this) before
//     creating runs.
//   - any git error translated.
func (w *Workflow) EnsureRunBranch(branch, defaultBranch string) error {
	if branch == "" {
		return fmt.Errorf("enjugit: EnsureRunBranch: branch is required")
	}
	trace := startTrace("EnsureRunBranch")
	defer trace.emit(w.logger, w.traceFile)
	trace.ctx("branch", branch)
	trace.ctx("default_branch", defaultBranch)

	werr := w.git.WithLock(func(g git.Ops) error {
		// Step 1: refresh origin refs so the local-vs-origin
		// distinction below reflects current reality.
		if err := g.Fetch(); err != nil {
			if errors.Is(err, git.ErrNoRemote) {
				trace.skipped("fetch-origin", "no remote configured")
			} else {
				trace.appendStep(Step{
					Name: "fetch-origin", Status: "failed",
					Detail: err.Error(),
				})
				w.logger.Warn("enjugit: pre-ensure fetch failed; continuing",
					"branch", branch, "error", err)
			}
		} else {
			trace.ok("fetch-origin")
		}

		// Step 2: local check. Pass the full ref path so
		// ResolveRef does an exact lookup instead of falling
		// through to origin's tracking ref.
		if sha, err := g.ResolveRef("refs/heads/" + branch); err == nil {
			trace.okDetail("exists-local", shortSHA(sha))
			return nil
		}
		trace.skipped("exists-local", "branch not present locally")

		// Step 3: origin check. When another machine has pushed
		// the branch but our clone hasn't materialized it as a
		// local ref yet, planting the local ref keeps subsequent
		// fork/checkout verbs strict.
		if sha, err := g.ResolveRef("refs/remotes/origin/" + branch); err == nil {
			trace.okDetail("exists-origin", shortSHA(sha))
			if err := g.CreateBranchAt(branch, sha); err != nil {
				return trace.fail("create-from-origin",
					translateGitError("create local from origin tip", err))
			}
			trace.ok("create-from-origin")
			return nil
		}
		trace.skipped("exists-origin", "branch not present on origin")

		// Step 4: brand-new branch — fork from defaultBranch.
		if defaultBranch == "" {
			return trace.fail("pick-fork-base",
				fmt.Errorf("%w: defaultBranch required for brand-new run branch", ErrForkBaseNotFound))
		}
		forkSHA, err := g.ResolveRef(defaultBranch)
		if err != nil {
			return trace.fail("resolve-default-branch",
				fmt.Errorf("%w: %s: %v", ErrForkBaseNotFound, defaultBranch, err))
		}
		trace.okDetail("resolve-default-branch", shortSHA(forkSHA))

		// Step 5: create the local ref at the fork point.
		if err := g.CreateBranchAt(branch, forkSHA); err != nil {
			return trace.fail("create-branch",
				translateGitError("create run branch", err))
		}
		trace.ok("create-branch")

		// Step 6: push so the ref exists on origin too. Without
		// this, follow-up verbs on a different machine couldn't
		// see the branch and would either err or duplicate-create.
		if err := g.Push(branch); err != nil {
			if errors.Is(err, git.ErrNoRemote) {
				trace.skipped("push", "no remote configured")
				return nil
			}
			return trace.fail("push",
				translateGitError("push run branch", err))
		}
		trace.ok("push")
		return nil
	})
	return werr
}
