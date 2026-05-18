package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// validateRunBranchShape mirrors enjuRunBranchShape in
// internal/fatclient/service — kept local so the `validate` CLI
// doesn't couple to the service package for one heuristic. If you
// change one, change the other (run/iter branch shapes:
// "1-slug", ".../iter-N", "slug-N").
var validateRunBranchShape = regexp.MustCompile(`^[0-9]+-|/iter-|-[0-9]+$`)

// repoAdvisory returns non-fatal notes about the git state the
// workflow at yamlPath would actually run against. It is strictly
// best-effort: any failure (not a git repo, git absent, detached
// HEAD) yields no notes — `validate` stays a pure workflow check
// and never errors on non-repo / CI use. Never affects the
// validation verdict or exit code.
func repoAdvisory(yamlPath string) []string {
	abs, err := filepath.Abs(yamlPath)
	if err != nil {
		return nil
	}
	dir := filepath.Dir(abs)

	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil // not a repo / no git → silent skip
	}

	var notes []string
	if b, berr := exec.Command("git", "-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); berr == nil {
		if br := strings.TrimSpace(string(b)); br != "" && validateRunBranchShape.MatchString(br) {
			notes = append(notes, fmt.Sprintf(
				"on branch %q, which looks like an enju run/iter branch — "+
					"create_run pins the committed tree of the project's default "+
					"branch; switch to your default branch before running.", br))
		}
	}
	if s, serr := exec.Command("git", "-C", dir, "status", "--porcelain", "--", abs).Output(); serr == nil {
		if len(strings.TrimSpace(string(s))) > 0 {
			notes = append(notes, fmt.Sprintf(
				"%s has uncommitted changes — create_run pins the COMMITTED "+
					"version, so these edits won't take effect until committed.",
				filepath.Base(abs)))
		}
	}
	return notes
}
