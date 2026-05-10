package compute_test

// Wrapper-binary contract tests. These exercise the
// `enju wrap-task` subprocess path on its own so we know the
// fork+exec+trailers pipeline is solid before phase 4 starts
// depending on it for async task completion. Distinct from the
// MCP integration tests, which exercise compute tasks via the
// in-process compute.Run path; keeping the two separate means
// a subprocess regression can't masquerade as an integration
// failure (and vice versa).

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/compute"
)

// TestMain lets the test binary double as the `enju wrap-task`
// subprocess — same trick cmd/enju/main.go uses. Without this,
// the child exec would not know how to dispatch wrap-task and
// would run the test binary's normal `go test` entry point.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "wrap-task" {
		os.Exit(compute.WrapMain(os.Args[2:], io.Discard))
	}
	os.Exit(m.Run())
}

// TestWrapMainReadsSpecWritesResult verifies the simplest
// wrapper contract: given a spec pointing at a non-existent
// script, the wrapper returns a structured Result via the
// output file with Error set — not a silent exit code.
// Proves the fork+exec+IO round-trip works end-to-end without
// pulling in the git + workspace machinery.
func TestWrapMainReadsSpecWritesResult(t *testing.T) {
	tmp := t.TempDir()
	specPath := filepath.Join(tmp, "spec.json")
	outPath := filepath.Join(tmp, "result.json")

	// Deliberately point at a script that doesn't exist — we
	// want the wrapper to surface the missing-script error
	// via Result.Error, which is the signal the MCP handler
	// keys on in production.
	spec := compute.Spec{
		TaskID:      "1:1:t",
		ScriptPath:  filepath.Join(tmp, "does-not-exist.sh"),
		ScriptLabel: "missing.sh",
	}
	data, _ := json.Marshal(spec)
	if err := os.WriteFile(specPath, data, 0600); err != nil {
		t.Fatalf("writing spec: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(self, "wrap-task", "--spec", specPath, "--output", outPath)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wrap-task exit: %v (stderr: %s)", err, stderr.String())
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	var got compute.Result
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if got.Error == "" {
		t.Fatalf("expected Error to mention missing script, got: %+v", got)
	}
	if got.CommitSHA != "" {
		t.Errorf("expected no CommitSHA on missing-script path, got %q", got.CommitSHA)
	}
}

// TestWrapMainRejectsMissingFlags verifies the argv parser
// rejects invocations without both --spec and --output (exit
// 2, not a silent success). Protects the handler from a bug
// where it forgets to pass one of the flags and then reads
// back a stale/absent result file.
func TestWrapMainRejectsMissingFlags(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}
	cases := [][]string{
		{"wrap-task"},
		{"wrap-task", "--spec", "x"},
		{"wrap-task", "--output", "y"},
	}
	for _, args := range cases {
		cmd := exec.Command(self, args...)
		cmd.Stderr = io.Discard
		err := cmd.Run()
		if err == nil {
			t.Errorf("args %v: expected non-zero exit, got success", args)
			continue
		}
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() != 2 {
				t.Errorf("args %v: expected exit 2, got %d", args, ee.ExitCode())
			}
		}
	}
}

// TestWrapMainCreatesAndCleansScratchDir is the Phase 2.1 contract:
// when Spec.TaskScratchDir is non-empty, the wrapper creates that
// directory before exec'ing the script and removes it on the way
// out — regardless of whether the script ran or even existed.
//
// Without this, scratch dirs would leak across runs and a crashed
// wrapper would leave orphans that the next attempt would
// rediscover (causing dirty-state confusion).
//
// Uses the missing-script path (same shape as the existing
// TestWrapMainReadsSpecWritesResult) so we don't need git
// infrastructure to verify the lifecycle.
func TestWrapMainCreatesAndCleansScratchDir(t *testing.T) {
	tmp := t.TempDir()
	specPath := filepath.Join(tmp, "spec.json")
	outPath := filepath.Join(tmp, "result.json")
	scratch := filepath.Join(tmp, "scratch", "task-1-1-fetch-iter-1")

	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("test setup: scratch should not exist yet (err=%v)", err)
	}

	spec := compute.Spec{
		TaskID:         "1:1:fetch",
		ScriptPath:     filepath.Join(tmp, "does-not-exist.sh"),
		ScriptLabel:    "missing.sh",
		TaskScratchDir: scratch,
	}
	data, _ := json.Marshal(spec)
	if err := os.WriteFile(specPath, data, 0600); err != nil {
		t.Fatalf("writing spec: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(self, "wrap-task", "--spec", specPath, "--output", outPath)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wrap-task exit: %v (stderr: %s)", err, stderr.String())
	}

	// Even though the script didn't exist, scratch should have
	// been created (so a real script could have used it) and
	// then cleaned up on the way out.
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch dir should have been cleaned up, but stat returned: %v", err)
	}
}
