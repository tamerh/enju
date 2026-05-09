package enjugit

import (
	"errors"
	"fmt"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitv6"
)

// Sentinel errors. Verbs document which they return; callers use
// errors.Is. String matching is forbidden.
//
// Translation rule: workflow methods catch git.Err* and translate
// to enjugit.Err* via errors.Is + a fresh Errorf("%w: ...", ...).
// Callers (service) NEVER see git.Err* — that's the layer
// boundary. A lint check enforces this on the service side.
var (
	// Workspace
	ErrCloneNotFound = errors.New("enjugit: no clone for project")
	ErrNoCloneSource = errors.New("enjugit: no remote_url and no adopted path")

	// Workflow state-prep
	ErrUpstreamNotFound       = errors.New("enjugit: upstream branch not found on origin")
	ErrIterationBranchExists  = errors.New("enjugit: iteration branch already exists")
	ErrIterationBranchMissing = errors.New("enjugit: iteration branch does not exist")
	ErrForkBaseNotFound       = errors.New("enjugit: fork base could not be resolved")
	ErrInvalidForkPoint       = errors.New("enjugit: invalid ForkPoint")

	// Submit
	ErrSubmitVerifyFailed = errors.New("enjugit: submit pushed but remote ref didn't update")
	ErrCannotForkBranch   = errors.New("enjugit: cannot create iteration branch")
	ErrPushNonFF          = errors.New("enjugit: push rejected (non-fast-forward)")

	// Merge
	ErrMergeConflict   = errors.New("enjugit: merge conflict (paths in error.Paths)")
	ErrCannotAutoMerge = errors.New("enjugit: auto-merge failed (non-conflict)")

	// Templates / resolve
	ErrTemplateNotFound    = errors.New("enjugit: template bundle not found")
	ErrUnresolvedReference = errors.New("enjugit: template reference could not be resolved")

	// Scanner
	ErrScanCursorMismatch = errors.New("enjugit: scan cursor doesn't match expected SHA")

	// Read
	ErrCommitNotFound = errors.New("enjugit: commit not found")
	ErrRefNotFound    = errors.New("enjugit: ref not found")
)

// ErrConflict carries everything the audit pipeline needs to
// report a merge conflict to the coordinator: the paths that
// conflicted, the topic and target branches, and their tip
// SHAs. Returned wrapped around ErrMergeConflict so callers
// can both errors.Is(err, ErrMergeConflict) and errors.As(err,
// &*ErrConflict).
//
// Service uses these fields directly when posting a
// merge_conflict_detected report so the audit timeline carries
// the full picture (which task triggered, which branches, which
// commits, which files) — sufficient to spawn a merge_resolve
// task without re-deriving any of it.
type ErrConflict struct {
	// Paths are the worktree-relative file paths that hit a
	// conflict marker. Always populated when known.
	Paths []string

	// Branch is the merge target (typically the run branch).
	Branch string
	// TopicBranch is the source the merge tried to fold in.
	TopicBranch string
	// TopicCommit is the topic branch's tip SHA at merge time.
	TopicCommit string
	// RunTipCommit is the target branch's tip SHA at merge time.
	RunTipCommit string
}

func (e *ErrConflict) Error() string {
	if e.Branch != "" && e.TopicBranch != "" {
		return "enjugit: merge conflict on " + e.Branch +
			": topic " + shortSHA(e.TopicCommit) +
			" and run-tip " + shortSHA(e.RunTipCommit) +
			" do not merge cleanly (conflicts in " + joinShort(e.Paths) + ")"
	}
	return "enjugit: merge conflict in " + joinShort(e.Paths)
}
func (e *ErrConflict) Is(target error) bool { return target == ErrMergeConflict }

// (PrepareBranch's structured failure type lives in op_trace.go
// as the shared *WorkflowOpError. Callers route via
// errors.Is(err, ErrCannotForkBranch) for "fork failed at the
// last step" and errors.As(err, &*WorkflowOpError) for the
// step-by-step trace.)

// ErrSubmitVerify carries the local + remote SHAs when a submit
// push verified to a different SHA than expected.
type ErrSubmitVerify struct {
	Branch    string
	LocalSHA  string
	RemoteSHA string
}

func (e *ErrSubmitVerify) Error() string {
	return "enjugit: submit verify failed: branch=" + e.Branch +
		" local=" + e.LocalSHA + " remote=" + e.RemoteSHA
}
func (e *ErrSubmitVerify) Is(target error) bool { return target == ErrSubmitVerifyFailed }

// gitOpError wraps a translated git error with the project-level
// context the operator needs to diagnose without a debugger:
// which op, which branch (if branch-scoped), which on-disk
// clone, which origin URL. Surfaces in error messages as a
// compact `op{branch=…, workdir=…, origin=…}: <cause>` line.
//
// Sentinel checks (errors.Is / errors.As) walk through Unwrap,
// so a caller doing `errors.Is(err, ErrPushNonFF)` still works
// when the wrap is in the chain.
//
// Construct via Workflow.wrapGitError so the workdir + remote
// fields are filled in consistently. Plain translateGitError
// remains for callers that don't have a Workflow handle (low-
// level tests + a few internal helpers).
type gitOpError struct {
	Op        string // verb name, e.g. "push", "fetch branch"
	Branch    string // empty for non-branch-scoped ops
	WorkDir   string
	RemoteURL string
	Cause     error
}

func (e *gitOpError) Error() string {
	parts := make([]string, 0, 3)
	if e.Branch != "" {
		parts = append(parts, "branch="+e.Branch)
	}
	if e.WorkDir != "" {
		parts = append(parts, "workdir="+e.WorkDir)
	}
	if e.RemoteURL != "" {
		parts = append(parts, "origin="+e.RemoteURL)
	}
	ctx := ""
	if len(parts) > 0 {
		ctx = "{" + joinComma(parts) + "} "
	}
	return e.Op + " " + ctx + ": " + e.Cause.Error()
}

func (e *gitOpError) Unwrap() error { return e.Cause }

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// translateGitError maps a git.Err* to its enjugit.Err*
// counterpart. Used by every Workflow method that catches a git
// error before returning. Unknown git errors are wrapped
// generically.
func translateGitError(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, git.ErrMergeConflict):
		var c *git.ErrConflict
		if errors.As(err, &c) {
			return &ErrConflict{Paths: c.Paths}
		}
		return ErrMergeConflict
	case errors.Is(err, git.ErrPushVerifyFailed):
		var v *git.ErrVerifyFailed
		if errors.As(err, &v) {
			return &ErrSubmitVerify{
				Branch:    v.Branch,
				LocalSHA:  v.LocalSHA,
				RemoteSHA: v.RemoteSHA,
			}
		}
		return ErrSubmitVerifyFailed
	case errors.Is(err, git.ErrPushNonFF):
		return ErrPushNonFF
	case errors.Is(err, git.ErrCommitNotFound):
		return ErrCommitNotFound
	case errors.Is(err, git.ErrCloneNotFound):
		return ErrCloneNotFound
	case errors.Is(err, git.ErrRefNotFound):
		// Generic ref-not-found from git surfaces as enjugit's
		// own ErrRefNotFound. Callers that mean "upstream not
		// found on origin" specifically (e.g. state_prep's
		// resolve-upstream step) wrap with ErrUpstreamNotFound
		// at the call site — see materializeUpstreamForReview.
		// Don't blanket-translate to ErrUpstreamNotFound here:
		// MergeFFOrFail's target-not-found case isn't an
		// "upstream" question and the resulting message lies
		// about which branch is missing where.
		return fmt.Errorf("%w: %s", ErrRefNotFound, err.Error())
	default:
		return err
	}
}

// joinShort produces a compact comma-joined list for error
// messages. Truncates after 5 entries so the error stays readable.
func joinShort(paths []string) string {
	if len(paths) == 0 {
		return "0 file(s)"
	}
	const max = 5
	out := ""
	for i, p := range paths {
		if i >= max {
			out += " ..."
			break
		}
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
