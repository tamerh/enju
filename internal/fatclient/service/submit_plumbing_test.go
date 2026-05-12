package service

// Tests for the plumbing-mode submit path (UsePlumbing=true).
// Focus: the structural invariants that differ from the
// porcelain path, where regressions would silently break bot
// daemon submits.

import (
	"context"
	"strings"
	"testing"
)

// TestPrepareFatSubmit_PlumbingRequiresBranch pins the
// invariant: with UsePlumbing=true, Meta.Branch must be
// non-empty. SubmitComputeTaskResult uses it as the
// resolve-base fallback when the topic branch has no local
// ref yet — passing empty produces a deep failure mid-commit.
// The early validation fails loud at submit-entry instead.
func TestPrepareFatSubmit_PlumbingRequiresBranch(t *testing.T) {
	fc := &FatClient{}
	params := SubmitParams{
		TaskID: "1:1:t",
		Meta: &TaskMeta{
			ProjectID: 1,
			Action:    "answer",
			State:     "claimed",
			// Branch deliberately empty
		},
		UsePlumbing: true,
		Content:     "anything",
	}
	_, err := fc.prepareFatSubmit(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when UsePlumbing=true with empty Meta.Branch, got nil")
	}
	if !strings.Contains(err.Error(), "UsePlumbing=true requires meta.Branch") {
		t.Errorf("error should name the invariant; got: %v", err)
	}
}

// TestPrepareFatSubmit_PorcelainToleratesEmptyBranch confirms
// the validation is plumbing-specific. Porcelain (UsePlumbing
// =false) historically tolerated empty Branch via wider
// resolve-base fallback inside SubmitTaskResult; the early
// validation must NOT reject this case so MCP-side submits
// keep working.
//
// We don't drive a full submit here (that needs a workspace);
// we assert that the early validation alone doesn't reject.
// The validation is one of the first checks in
// prepareFatSubmit, so reaching OpenWorkflow (the next step,
// which will fail without a configured workspace) confirms
// we got past the new gate.
func TestPrepareFatSubmit_PorcelainToleratesEmptyBranch(t *testing.T) {
	fc := &FatClient{} // no workspace; OpenWorkflow will surface as a no-op
	params := SubmitParams{
		TaskID: "1:1:t",
		Meta: &TaskMeta{
			ProjectID: 1,
			Action:    "answer",
			State:     "claimed",
		},
		UsePlumbing: false,
		Content:     "anything",
	}
	_, err := fc.prepareFatSubmit(context.Background(), params)
	// Whatever error we get, it must NOT be the plumbing-branch
	// invariant. The OpenWorkflow step without a workspace
	// surfaces as a nil-workflow / nil-pointer condition; we
	// just care that we got past the early check.
	if err != nil && strings.Contains(err.Error(), "UsePlumbing=true requires") {
		t.Errorf("porcelain path should not trigger the plumbing-branch validation: %v", err)
	}
}

// TestPrepareFatSubmit_PlumbingAcceptsNonEmptyBranch confirms
// the validation passes when Branch is set, regardless of
// task action (vote/review/answer/compute all routed through
// the same path).
func TestPrepareFatSubmit_PlumbingAcceptsNonEmptyBranch(t *testing.T) {
	fc := &FatClient{}
	for _, action := range []string{"answer", "compute", "contribute"} {
		t.Run(action, func(t *testing.T) {
			params := SubmitParams{
				TaskID: "1:1:t",
				Meta: &TaskMeta{
					ProjectID: 1,
					Action:    action,
					State:     "claimed",
					Branch:    "run-1",
				},
				UsePlumbing: true,
				Content:     "anything",
			}
			_, err := fc.prepareFatSubmit(context.Background(), params)
			if err != nil && strings.Contains(err.Error(), "UsePlumbing=true requires") {
				t.Errorf("non-empty Branch should pass the plumbing-branch validation: %v", err)
			}
		})
	}
}
