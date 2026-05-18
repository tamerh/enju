package service

import (
	"strings"
	"testing"
)

func TestRunTemplateBranchGuard(t *testing.T) {
	// On the default branch → no error.
	if err := runTemplateBranchGuard("master", "master", "/repo"); err != nil {
		t.Errorf("on-default should pass, got: %v", err)
	}
	// Parked on a run branch → fail closed with actionable guidance.
	err := runTemplateBranchGuard("load-test-fan-out-1", "master", "/repo")
	if err == nil {
		t.Fatal("on a non-default branch must fail closed")
	}
	for _, want := range []string{"load-test-fan-out-1", "master", "git -C /repo checkout master"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("guidance missing %q in: %v", want, err)
		}
	}
	// Unset default (new project) → skip (EnsureBundleOnDefault handles it).
	if err := runTemplateBranchGuard("anything", "", "/repo"); err != nil {
		t.Errorf("empty default should skip, got: %v", err)
	}
	// Detached HEAD (current == "") → skip in v1 (not a false positive).
	if err := runTemplateBranchGuard("", "master", "/repo"); err != nil {
		t.Errorf("detached HEAD should skip in v1, got: %v", err)
	}
}

func TestSuspiciousAdoptedDefaultBranch(t *testing.T) {
	// Legitimate defaults → no warning.
	for _, ok := range []string{"", "main", "master", "develop", "trunk", "feature/auth"} {
		if w := suspiciousAdoptedDefaultBranch(ok); w != "" {
			t.Errorf("%q should not warn, got: %s", ok, w)
		}
	}
	// enju run/iter branch shapes → warn (the ones that mis-recorded
	// the default this session).
	for _, bad := range []string{
		"1-enju-feature-showcase",
		"load-test-fan-out-multi-stage-compute-pipeline-1",
		"enju-feature-showcase-2",
		"2-identity-model-probe/human_step/iter-1",
	} {
		w := suspiciousAdoptedDefaultBranch(bad)
		if w == "" {
			t.Errorf("%q should warn (enju run/iter shape)", bad)
			continue
		}
		if !strings.Contains(w, bad) || !strings.Contains(w, "enju_set_project_default_branch") {
			t.Errorf("%q warning not actionable: %s", bad, w)
		}
	}
}
