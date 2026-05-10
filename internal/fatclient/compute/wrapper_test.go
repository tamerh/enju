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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestMaterializeReads pins the Phase 2.2 contract: given a list
// of declared input paths and a read-from-commit function, the
// helper writes each input under the scratch dir at its declared
// path, creating intermediate directories as needed; missing
// inputs surface in the returned slice (caller soft-warns) but
// don't abort the rest.
func TestMaterializeReads(t *testing.T) {
	scratch := t.TempDir()

	contents := map[string][]byte{
		"data/raw_a.txt":      []byte("alpha\n"),
		"data/raw_b.txt":      []byte("beta\n"),
		"src/config/conf.yml": []byte("port: 8080\n"),
	}
	read := func(sha, path string) ([]byte, bool, error) {
		if sha != "deadbeef" {
			return nil, false, nil
		}
		body, ok := contents[path]
		return body, ok, nil
	}

	paths := []string{
		"data/raw_a.txt",
		"data/raw_b.txt",
		"src/config/conf.yml",
		"data/raw_c.txt", // intentionally missing
	}
	missing, err := compute.MaterializeReads(scratch, "deadbeef", paths, read)
	if err != nil {
		t.Fatalf("MaterializeReads returned error: %v", err)
	}
	if len(missing) != 1 || missing[0] != "data/raw_c.txt" {
		t.Errorf("expected missing=[data/raw_c.txt], got %v", missing)
	}
	for path, want := range contents {
		got, err := os.ReadFile(filepath.Join(scratch, path))
		if err != nil {
			t.Errorf("reading materialized %s: %v", path, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("content mismatch at %s: got %q, want %q", path, got, want)
		}
	}
	// Ensure the missing one didn't leave an empty file behind.
	if _, err := os.Stat(filepath.Join(scratch, "data/raw_c.txt")); !os.IsNotExist(err) {
		t.Errorf("missing path should not have been written, stat err: %v", err)
	}
}

// TestMaterializeReads_ReadError surfaces underlying read
// failures verbatim (not as missing) so the caller can fail the
// task loudly rather than silently treating an IO error as
// "input absent."
func TestMaterializeReads_ReadError(t *testing.T) {
	scratch := t.TempDir()
	read := func(sha, path string) ([]byte, bool, error) {
		return nil, false, fmt.Errorf("disk on fire")
	}
	_, err := compute.MaterializeReads(scratch, "deadbeef",
		[]string{"data/x.txt"}, read)
	if err == nil {
		t.Fatal("expected error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error should carry underlying cause, got: %v", err)
	}
}

// TestScriptCwdFor pins the Phase 2.5 picker: scratch wins
// whenever it's set, regardless of execution mode. Container
// mode used to fall back to workDir (Phase 2.3 deferred container
// support); 2.5 added the docker bind-mount + path translation
// so container scripts also see scratch as their CWD via
// ContainerScratchDir. The host-side path returned here is what
// the wrapper uses to read writes_artifacts after exit and to
// materialize reads_artifacts before start — same on both modes.
func TestScriptCwdFor(t *testing.T) {
	cases := []struct {
		name string
		spec compute.Spec
		want string
	}{
		{
			name: "direct-exec with scratch ─► scratch",
			spec: compute.Spec{TaskScratchDir: "/scratch/abc"},
			want: "/scratch/abc",
		},
		{
			name: "direct-exec without scratch ─► workDir",
			spec: compute.Spec{},
			want: "/work",
		},
		{
			name: "container with scratch ─► scratch (bind-mounted at /scratch in container)",
			spec: compute.Spec{TaskScratchDir: "/scratch/abc", Container: "alpine:3"},
			want: "/scratch/abc",
		},
		{
			name: "container without scratch ─► workDir",
			spec: compute.Spec{Container: "alpine:3"},
			want: "/work",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compute.ScriptCwdFor(c.spec, "/work"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestSweepStaleScratch covers the Phase 2.4 crash-recovery
// helper: at bot startup, any scratch dirs left over from a
// previous wrapper's crash get nuked. Empty and nonexistent
// trees are no-ops.
func TestSweepStaleScratch(t *testing.T) {
	tmp := t.TempDir()
	scratch := filepath.Join(tmp, "scratch")

	// Empty scratch root: no-op.
	if n, err := compute.SweepStaleScratch(tmp); err != nil || n != 0 {
		t.Fatalf("empty: got n=%d err=%v, want 0/nil", n, err)
	}

	// Populated: simulate two crashed wrappers' leftovers.
	for _, sub := range []string{"task-a-iter-1", "task-b-iter-3/data"} {
		if err := os.MkdirAll(filepath.Join(scratch, sub), 0o755); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(scratch, "task-b-iter-3/data/x.txt"),
		[]byte("hi"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	n, err := compute.SweepStaleScratch(tmp)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("removed count: got %d, want 2", n)
	}
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Errorf("scratch root should be empty after sweep, has: %v", entries)
	}

	// Nonexistent scratch root: no-op.
	if n, err := compute.SweepStaleScratch(filepath.Join(tmp, "no-such-dir")); err != nil || n != 0 {
		t.Errorf("nonexistent: got n=%d err=%v, want 0/nil", n, err)
	}

	// Empty workspaceRoot: no-op.
	if n, err := compute.SweepStaleScratch(""); err != nil || n != 0 {
		t.Errorf("empty workspaceRoot: got n=%d err=%v, want 0/nil", n, err)
	}
}
