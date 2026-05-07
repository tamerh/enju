package enjugit

import (
	"errors"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// MaterializeUpstreamForReview puts the upstream task's tip on
// disk in detached HEAD state. NO local branch ref is created or
// modified. Used by review tasks before claude -p reads the
// developer's content.
//
// upstreamBranch is the name of the upstream task's topic branch
// (e.g. "1-build/develop_a/iter-1"). Resolved via origin's tip.
//
// Git operations performed:
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
func (w *Workflow) MaterializeUpstreamForReview(upstreamBranch string) error {
	if upstreamBranch == "" {
		return fmt.Errorf("enjugit: MaterializeUpstreamForReview: upstreamBranch is required")
	}
	trace := startTrace("MaterializeUpstreamForReview")
	trace.ctx("upstream_branch", upstreamBranch)

	// Step 1: fetch (best-effort).
	if err := w.git.Fetch(); err != nil {
		if errors.Is(err, git.ErrNoRemote) {
			trace.skipped("fetch-origin", "no remote configured")
		} else {
			trace.steps = append(trace.steps, Step{
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
	sha, err := w.git.ResolveRef(upstreamBranch)
	if err != nil {
		if errors.Is(err, git.ErrRefNotFound) {
			return trace.fail("resolve-upstream",
				fmt.Errorf("%w: %s", ErrUpstreamNotFound, upstreamBranch))
		}
		return trace.fail("resolve-upstream", translateGitError("resolve upstream", err))
	}
	trace.okDetail("resolve-upstream", shortSHA(sha))

	// Step 3: detached checkout onto upstream's tree.
	if err := w.git.CheckoutCommit(sha); err != nil {
		return trace.fail("checkout-detached", translateGitError("checkout upstream commit", err))
	}
	trace.ok("checkout-detached")
	return nil
}

// StartIterationBranch creates a fresh iter-N topic branch
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
func (w *Workflow) StartIterationBranch(
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
	trace := startTrace("StartIterationBranch")
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
				trace.steps = append(trace.steps, Step{
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

// ResumeIterationBranch switches to an existing iter-N topic
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
func (w *Workflow) ResumeIterationBranch(
	taskID string,
	iterSeq int,
	taskDef, instanceKey string,
	runSeq int,
	runSlug string,
) (string, error) {
	branchName := w.convs.BranchName(runSeq, runSlug, taskDef, instanceKey, iterSeq)
	trace := startTrace("ResumeIterationBranch")
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
				trace.steps = append(trace.steps, Step{
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

// WipeIterationWrites removes files matching the task's declared
// writes_artifacts patterns. Run between iterations so iter-N's
// commit carries iter-N's content only — no union of prior
// iterations' files (the LLM-non-determinism-in-filenames bug).
//
// All four declaration shapes (literal, glob, dir, templated)
// are handled by delegating to WriteArtifacts.ExpandAgainstWorkdir
// (the same expander used by post-handler validation).
//
// Git operations performed:
//   1. Expand writes against the worktree → list of paths.
//   2. RemoveFiles(paths).
//
// Worktree state: Pre StateClean → Post StateClean (paths gone).
// Idempotent: missing paths are silently skipped.
func (w *Workflow) WipeIterationWrites(writes WriteArtifacts) error {
	if len(writes) == 0 {
		return nil
	}
	expanded, _, err := writes.ExpandAgainstWorkdir(w.workDir())
	if err != nil {
		return fmt.Errorf("enjugit: expand writes: %w", err)
	}
	paths := make([]string, 0, len(expanded))
	for _, e := range expanded {
		paths = append(paths, e.Path)
	}
	if len(paths) == 0 {
		return nil
	}
	return translateGitError("remove iter writes", w.git.RemoveFiles(paths))
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

// workDir is a small helper so workflow methods can resolve the
// worktree directory without depending on *git.Clone directly
// (that would defeat the Ops-interface mocking story). We add a
// tiny extension method on git.Ops via a type assertion when the
// underlying impl supports it.
func (w *Workflow) workDir() string {
	type workDirer interface{ WorkDir() string }
	if wd, ok := w.git.(workDirer); ok {
		return wd.WorkDir()
	}
	return ""
}
