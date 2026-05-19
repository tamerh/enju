package service

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/compute"
)

// classifyComputeResult is the post-compute.Run truth table. The
// A1 livelock shipped because one arm was missing: a script that
// exited 0 but didn't produce its declared writes set res.Error,
// and the old inline code returned a raw error WITHOUT POSTing
// /fail — so the task stranded in RUNNING, never failed_retryable,
// enju_retry_task refused it, and the reaper re-ran it forever.
// This pins every arm; the wrapper-abort arm is the regression.
func TestClassifyComputeResult(t *testing.T) {
	const (
		taskID  = "9:1:c"
		script  = "scripts/x.sh"
		branch  = "run-9"
		scratch = "/tmp/scratch/9-1-c"
	)

	t.Run("success → continue (nil Outcome)", func(t *testing.T) {
		cls := classifyComputeResult(compute.Result{ExitCode: 0, CommitSHA: "abc"}, taskID, script, branch, scratch)
		if cls.Outcome != nil {
			t.Fatalf("clean exit-0 must continue (nil Outcome); got %+v", cls.Outcome)
		}
		if cls.PostFail {
			t.Error("success must not POST /fail")
		}
	})

	t.Run("script non-zero exit → failed + POST /fail", func(t *testing.T) {
		cls := classifyComputeResult(
			compute.Result{ExitCode: 1, Stderr: "boom\nINTENTIONAL"},
			taskID, script, branch, scratch)
		if cls.Outcome == nil || cls.Outcome.Status != "failed" {
			t.Fatalf("exit!=0 must be Status=failed; got %+v", cls.Outcome)
		}
		if !cls.PostFail {
			t.Error("exit!=0 must POST /fail kind=compute_error (failed_retryable)")
		}
		if !strings.Contains(cls.Reason, "exited with code 1") || !strings.Contains(cls.Reason, "INTENTIONAL") {
			t.Errorf("reason should name exit code + stderr tail; got %q", cls.Reason)
		}
		if cls.Outcome.ScratchDir != scratch {
			t.Errorf("failed outcome must surface scratch dir; got %q", cls.Outcome.ScratchDir)
		}
	})

	// THE A1 REGRESSION ARM. Wrapper-level abort: script exited 0
	// but a required declared write was not produced → compute.Run
	// sets res.Error with ExitCode==0. This MUST behave exactly
	// like a non-zero exit: Status=failed AND PostFail=true so the
	// coordinator parks it failed_retryable. The bug was returning
	// a raw error here with no /fail → stranded RUNNING forever.
	t.Run("wrapper abort (exit 0, res.Error) → failed + POST /fail [A1]", func(t *testing.T) {
		cls := classifyComputeResult(
			compute.Result{
				ExitCode: 0,
				Error:    "required writes_artifacts not produced: [out/declared_but_missing.txt]",
			},
			taskID, script, branch, scratch)
		if cls.Outcome == nil {
			t.Fatal("A1: a wrapper-abort must NOT be a continue/raw-error — it must yield a failed Outcome")
		}
		if cls.Outcome.Status != "failed" {
			t.Errorf("A1: Status = %q, want failed", cls.Outcome.Status)
		}
		if !cls.PostFail {
			t.Fatal("A1 REGRESSION: wrapper abort must POST /fail kind=compute_error so it parks failed_retryable (not stranded RUNNING)")
		}
		if cls.Reason != "required writes_artifacts not produced: [out/declared_but_missing.txt]" {
			t.Errorf("A1: reason should carry the wrapper message verbatim; got %q", cls.Reason)
		}
		if cls.Outcome.ExitCode != 0 {
			t.Errorf("A1: a wrapper abort is exit 0; got ExitCode=%d", cls.Outcome.ExitCode)
		}
	})

	t.Run("git failure via res.GitError → git_failed, no /fail", func(t *testing.T) {
		cls := classifyComputeResult(
			compute.Result{ExitCode: 0, CommitSHA: "", GitError: "push rejected; rebase failed"},
			taskID, script, branch, scratch)
		if cls.Outcome == nil || cls.Outcome.Status != "git_failed" {
			t.Fatalf("GitError must be Status=git_failed; got %+v", cls.Outcome)
		}
		if cls.PostFail {
			t.Error("git_failed recovery is fix-the-git-state, NOT failed_retryable — must not POST /fail")
		}
		if cls.Outcome.ErrorMessage != "push rejected; rebase failed" {
			t.Errorf("git_failed must carry the git error; got %q", cls.Outcome.ErrorMessage)
		}
	})

	// git_failed must win over the generic res.Error arm — proving
	// it is checked FIRST (the old code's `if res.Error!="" return`
	// made this fallback permanently dead).
	t.Run("git failure via res.Error prefix fallback → git_failed (checked first)", func(t *testing.T) {
		cls := classifyComputeResult(
			compute.Result{ExitCode: 0, Error: compute.GitSubmitFailedPrefix + " object not found"},
			taskID, script, branch, scratch)
		if cls.Outcome == nil || cls.Outcome.Status != "git_failed" {
			t.Fatalf("res.Error with GitSubmitFailedPrefix must classify git_failed (not generic failed); got %+v", cls.Outcome)
		}
		if cls.PostFail {
			t.Error("git_failed must not POST /fail even via the prefix fallback")
		}
	})
}
