package service

import (
	"fmt"
	"regexp"
)

// Worktree/branch guards for the create_* entry points. Both
// failures share one root: the operator's worktree is parked on an
// enju run/iter branch, so create_run pins from the wrong index
// and create_project adopt records the wrong default. One catches
// it fail-closed (create_run), the other advises (adopt).

// enjuRunBranchShape matches the unmistakable enju run/iter branch
// shapes: seq-prefixed runs ("1-enju-feature-showcase"), iter
// branches (".../iter-N"), and auto-named runs ending "-<N>"
// ("load-test-...-1", "enju-feature-showcase-2"). Conventional
// defaults — main/master/develop/trunk/feature/x — never match.
// A rare human branch like "release-2024" would; acceptable here
// because it only drives the NON-FATAL adopt advisory (one
// ignorable sentence) while the failure it prevents is a silent
// wrong-default. (Mirrored, intentionally decoupled, in
// cmd/enju's validate advisory — change both if you change one.)
var enjuRunBranchShape = regexp.MustCompile(`^[0-9]+-|/iter-|-[0-9]+$`)

// runTemplateBranchGuard fails closed, with actionable guidance,
// when the worktree is parked on a branch other than the project
// default. create_run pins the template by enumerating the
// CHECKED-OUT branch's index (git ls-files --cached); a run/iter
// branch carries committed task output that makes that enumeration
// trip with a cryptic "paths ignored by .gitignore". Catching it
// here — before the coordinator persists anything — turns that
// into a clear instruction and leaves no ghost run.
//
// v1 scope: only the explicit branch mismatch. Detached HEAD
// (current == "") and dirty-but-on-default are intentionally NOT
// fired on — precise to the known failure, no false positives;
// those are refinements, not v1.
func runTemplateBranchGuard(current, defaultBranch, repoDir string) error {
	if defaultBranch == "" || current == "" || current == defaultBranch {
		return nil
	}
	return fmt.Errorf(
		"worktree is on branch %q, not the project default %q — create_run "+
			"pins the template from the checked-out branch, and a run branch "+
			"carries committed run output that trips this. "+
			"Run: git -C %s checkout %s   then retry",
		current, defaultBranch, repoDir, defaultBranch)
}

// suspiciousAdoptedDefaultBranch returns a non-fatal warning when
// create_project auto-detected the project's default branch from a
// HEAD that looks like an enju run/iter branch — i.e. the worktree
// was parked on a run branch at adopt time, so the recorded
// default is almost certainly wrong and downstream runs would
// silently target it. "" when the branch looks like a legitimate
// default.
func suspiciousAdoptedDefaultBranch(branch string) string {
	if branch == "" || !enjuRunBranchShape.MatchString(branch) {
		return ""
	}
	return fmt.Sprintf("recorded default branch %q looks like an enju run/iter "+
		"branch — the worktree was likely parked there when the project was "+
		"adopted, so the project default is probably wrong. Set the intended "+
		"default with enju_set_project_default_branch (e.g. branch=main).", branch)
}
