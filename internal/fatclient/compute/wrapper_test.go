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
// pulling in the git + mcpgit machinery.
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
