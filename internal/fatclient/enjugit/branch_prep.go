package enjugit

import (
	"errors"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// prepareBranchForCommit ensures `branch` exists locally and is
// checked out, ready for a commit to land on. Used by every
// non-task verb that wants to commit to a specific branch:
// CommitArbitraryFiles, CommitTemplateBundle, future export
// helpers. Also called by SubmitTaskResult for the
// run-branch-fork fallback.
//
// Resolution order (top wins):
//
//  1. **fetch-origin** — best-effort fetch so origin tracking
//     refs reflect the latest server state. Skipped silently
//     when no remote is configured.
//  2. **validate-stale-ref** — when local `refs/heads/<branch>`
//     exists AND `preferredBase` is supplied, confirm the local
//     ref's history actually contains preferredBase's tip. If
//     not, the ref is stale from a prior bad fork and we reseat
//     it to preferredBase's tip BEFORE the next step takes it at
//     face value. Skipped when no preferredBase, or when the
//     ref doesn't exist yet, or when preferredBase can't be
//     resolved.
//  3. **checkout-local** — if `refs/heads/<branch>` exists
//     locally, check it out. Done. (Common steady state.)
//  4. **track-origin** — if `refs/remotes/origin/<branch>`
//     exists, create local from origin's tip + checkout. Done.
//     (First-time use of a branch the remote already knows.)
//  5. **fork-from-preferred-base** — neither local nor origin
//     has `<branch>`. When `preferredBase` is non-empty, fork
//     from its tip (used by review submits to fork the review
//     topic from the upstream developer's topic, so the review's
//     commit lands on top of the upstream content). Otherwise
//     falls through to step 6.
//  6. **fork-from-default** — neither local nor origin has
//     `<branch>`, and no preferred base was supplied. Create
//     from `defaultBranch` tip + checkout. (New run branch on
//     a project where main has commits.)
//
// Each step's outcome is recorded into a stepTrace; on failure
// the returned *WorkflowOpError carries the trace so a caller
// can read top-to-bottom which step finally failed. Wraps
// ErrCannotForkBranch as the typed cause for routing.
//
// Caller MUST hold w.git.WithLock — this verb is meant to be
// called from inside an atomic sequence.
func (w *Workflow) prepareBranchForCommit(g git.Ops, branch string, preferredBase string) error {
	if branch == "" {
		return fmt.Errorf("enjugit: prepareBranchForCommit: branch required")
	}
	defaultBranch := w.DefaultBranch()
	trace := startTrace("PrepareBranchForCommit")
	trace.ctx("branch", branch)
	trace.ctx("default_branch", defaultBranch)
	if preferredBase != "" {
		trace.ctx("preferred_base", preferredBase)
	}

	// Step 1: fetch-origin (best-effort).
	if ferr := g.Fetch(); ferr != nil {
		if errors.Is(ferr, git.ErrNoRemote) {
			trace.skipped("fetch-origin", "no remote configured")
		} else {
			// Don't fail the whole verb — offline / network
			// blips are recoverable. Recorded for debug.
			trace.steps = append(trace.steps, Step{
				Name: "fetch-origin", Status: "failed",
				Detail: ferr.Error(),
			})
		}
	} else {
		trace.ok("fetch-origin")
	}

	// Step 2: validate-stale-ref. When the local topic ref ALREADY
	// exists AND the caller supplied a preferredBase (review-iter
	// submit case), confirm the existing ref's history actually
	// contains preferredBase's tip. If not, the ref is stale (a
	// previous bad fork left it pointing at seed instead of
	// upstream's iter-N) and we reseat it to preferredBase's tip
	// before the next step takes it at face value.
	//
	// Skipped when no preferredBase is set, or the local ref
	// doesn't exist yet (the fork-from-* steps below will create
	// it correctly), or preferredBase can't be resolved (transient
	// — defer to the existing ref to avoid making things worse on
	// a lookup miss).
	//
	// This step is the "is the assumption I'm about to act on
	// still true?" check that prevents the silent-success class of
	// failure where checkout-local would happily switch to a stale
	// ref and the trace would say "ok" while the worktree ended up
	// at the wrong commit.
	if preferredBase != "" {
		localSHA, lerr := g.ResolveRef("refs/heads/" + branch)
		if lerr == nil {
			preferredSHA, perr := w.resolveBranchTip(g, preferredBase)
			if perr != nil {
				trace.skipped("validate-stale-ref",
					"preferred base "+preferredBase+" not yet resolvable")
			} else if isAnc, _ := g.IsAncestor(preferredSHA, localSHA); isAnc {
				trace.ok("validate-stale-ref")
			} else {
				if rerr := g.SetBranchTo(branch, preferredSHA); rerr != nil {
					return trace.fail("validate-stale-ref",
						translateGitError("reseat stale ref", rerr))
				}
				trace.okDetail("validate-stale-ref",
					"reseated "+shortSHA(localSHA)+" → "+
						preferredBase+"@"+shortSHA(preferredSHA))
			}
		} else if errors.Is(lerr, git.ErrRefNotFound) {
			trace.skipped("validate-stale-ref",
				"local refs/heads/"+branch+" doesn't exist yet")
		} else {
			return trace.fail("validate-stale-ref",
				translateGitError("resolve local ref", lerr))
		}
	} else {
		trace.skipped("validate-stale-ref", "no preferred base supplied")
	}

	// Step 3: checkout-local. Try local first — fastest path,
	// and the steady state for any branch the workspace has
	// already touched. (After validate-stale-ref above, the local
	// ref is either correct as-is or has been reseated.)
	if cerr := g.Checkout(branch); cerr == nil {
		trace.ok("checkout-local")
		return nil
	} else if !errors.Is(cerr, git.ErrRefNotFound) {
		// Real failure (not "branch missing") — surface it.
		// E.g. preserve dir collision, dirty worktree.
		return trace.fail("checkout-local", cerr)
	}
	trace.skipped("checkout-local", "no local refs/heads/"+branch)

	// Step 3: track-origin. Branch missing locally — see if
	// origin has it.
	originSHA, rerr := g.ResolveRef("refs/remotes/origin/" + branch)
	switch {
	case rerr == nil:
		if cerr := g.CreateBranchAt(branch, originSHA); cerr != nil {
			return trace.fail("track-origin", fmt.Errorf("create-branch: %w", cerr))
		}
		if cerr := g.Checkout(branch); cerr != nil {
			return trace.fail("track-origin", fmt.Errorf("checkout: %w", cerr))
		}
		trace.okDetail("track-origin", "forked from origin/"+branch+" @ "+shortSHA(originSHA))
		return nil
	case errors.Is(rerr, git.ErrRefNotFound):
		trace.skipped("track-origin", "no refs/remotes/origin/"+branch)
	default:
		return trace.fail("track-origin", rerr)
	}

	// Step 4: fork-from-preferred-base. When the caller
	// supplied a preferred fork ref (e.g. a review submit
	// passes the upstream's iteration branch so the review's
	// topic forks from the upstream's content), use that.
	if preferredBase != "" {
		preferredSHA, perr := w.resolveBranchTip(g, preferredBase)
		if perr != nil {
			trace.skipped("fork-from-preferred-base",
				"preferred base "+preferredBase+" not found")
		} else {
			if cerr := g.CreateBranchAt(branch, preferredSHA); cerr != nil {
				return trace.fail("fork-from-preferred-base",
					fmt.Errorf("create-branch: %w", cerr))
			}
			if cerr := g.Checkout(branch); cerr != nil {
				return trace.fail("fork-from-preferred-base",
					fmt.Errorf("checkout: %w", cerr))
			}
			trace.okDetail("fork-from-preferred-base",
				"forked from "+preferredBase+" @ "+shortSHA(preferredSHA))
			return nil
		}
	} else {
		trace.skipped("fork-from-preferred-base", "no preferred base supplied")
	}

	// Step 5: fork-from-default. Last resort — fork from the
	// project's default branch. Try local default first, then
	// origin/default.
	defaultSHA, defErr := w.resolveBranchTip(g, defaultBranch)
	if defErr != nil {
		// Record the failure WITH the typed cause so
		// errors.Is(err, ErrCannotForkBranch) routes correctly.
		trace.steps = append(trace.steps, Step{
			Name: "fork-from-default", Status: "failed",
			Detail: "default branch " + defaultBranch +
				" not found locally or on origin",
		})
		return trace.wrapTerminal(ErrCannotForkBranch)
	}
	if cerr := g.CreateBranchAt(branch, defaultSHA); cerr != nil {
		return trace.fail("fork-from-default", fmt.Errorf("create-branch: %w", cerr))
	}
	if cerr := g.Checkout(branch); cerr != nil {
		return trace.fail("fork-from-default", fmt.Errorf("checkout: %w", cerr))
	}
	trace.okDetail("fork-from-default", "forked from "+defaultBranch+" @ "+shortSHA(defaultSHA))
	return nil
}

// resolveBranchTip resolves a branch's tip SHA, preferring the
// local ref over origin's tracking ref. Used inside
// prepareBranchForCommit for the fork-from-default step.
func (w *Workflow) resolveBranchTip(g git.Ops, branch string) (string, error) {
	if sha, err := g.ResolveRef("refs/heads/" + branch); err == nil {
		return sha, nil
	}
	if sha, err := g.ResolveRef("refs/remotes/origin/" + branch); err == nil {
		return sha, nil
	}
	return "", fmt.Errorf("branch %q not found locally or on origin", branch)
}

// shortSHA returns the first 8 chars of a SHA for compact
// error / log messages. "" stays "".
func shortSHA(sha string) string {
	if len(sha) >= 8 {
		return sha[:8]
	}
	return sha
}
