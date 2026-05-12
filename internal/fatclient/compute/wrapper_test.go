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
	"time"

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

// TestSweepStaleScratchAtStartup covers the Phase 2.4/2.5
// crash-recovery helper: at bot startup, scratch dirs left over
// from a previous wrapper of THIS bot get nuked once they're
// past the age threshold (Phase 5 — TP53 Bug 2). Replica
// isolation: a second bot's scratch in the same workspace is
// untouched (Phase 2.5 — scoping by botUsername).
//
// The age filter is exercised by chtimes-aging the seeded dirs
// past compute.StaleScratchAgeThreshold so the public-API test
// stays runnable without sleeping. A separate test covers the
// fresh-dir-survives case.
func TestSweepStaleScratchAtStartup(t *testing.T) {
	tmp := t.TempDir()
	bot := "alice-1"
	otherBot := "alice-2"
	myScratch := filepath.Join(tmp, "scratch", bot)
	otherScratch := filepath.Join(tmp, "scratch", otherBot)

	// Empty scratch root: no-op.
	if n, err := compute.SweepStaleScratchAtStartup(tmp, bot); err != nil || n != 0 {
		t.Fatalf("empty: got n=%d err=%v, want 0/nil", n, err)
	}

	// Populated: simulate this bot's crashed wrappers + a sibling
	// replica's LIVE scratch that must NOT be nuked.
	for _, sub := range []string{"task-a-iter-1", "task-b-iter-3/data"} {
		if err := os.MkdirAll(filepath.Join(myScratch, sub), 0o755); err != nil {
			t.Fatalf("seed mine: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(myScratch, "task-b-iter-3/data/x.txt"),
		[]byte("hi"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	// Age the seeded top-level dirs past the threshold so the
	// real public-API sweep treats them as eligible. Chtimes on
	// the dir itself is what info.ModTime() reads.
	old := time.Now().Add(-25 * time.Hour)
	for _, sub := range []string{"task-a-iter-1", "task-b-iter-3"} {
		if err := os.Chtimes(filepath.Join(myScratch, sub), old, old); err != nil {
			t.Fatalf("age %s: %v", sub, err)
		}
	}
	// Sibling replica's live work — must survive.
	if err := os.MkdirAll(filepath.Join(otherScratch, "task-c-iter-1"), 0o755); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	n, err := compute.SweepStaleScratchAtStartup(tmp, bot)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("removed count: got %d, want 2", n)
	}
	if entries, _ := os.ReadDir(myScratch); len(entries) != 0 {
		t.Errorf("my scratch should be empty, has: %v", entries)
	}
	// Sibling replica's scratch must be untouched.
	if _, err := os.Stat(filepath.Join(otherScratch, "task-c-iter-1")); err != nil {
		t.Errorf("sibling replica's live scratch was clobbered: %v", err)
	}

	// Nonexistent scratch root: no-op.
	if n, err := compute.SweepStaleScratchAtStartup(filepath.Join(tmp, "no-such-dir"), bot); err != nil || n != 0 {
		t.Errorf("nonexistent: got n=%d err=%v, want 0/nil", n, err)
	}

	// Empty workspaceRoot: no-op.
	if n, err := compute.SweepStaleScratchAtStartup("", bot); err != nil || n != 0 {
		t.Errorf("empty workspaceRoot: got n=%d err=%v, want 0/nil", n, err)
	}

	// Empty botUsername: no-op (test-fixture path).
	if n, err := compute.SweepStaleScratchAtStartup(tmp, ""); err != nil || n != 0 {
		t.Errorf("empty botUsername: got n=%d err=%v, want 0/nil", n, err)
	}
}

// TestSweepStaleScratchAtStartup_HonorsEnvOverride pins R5:
// ENJU_SCRATCH_PRESERVE_HOURS lets operators extend (or shorten)
// the retry window without recompiling. A 1-hour override makes
// a 2-hour-old dir eligible for sweep that the 24h default
// would have kept. Invalid values fall back to the default
// silently so a typo can't disable the safety net.
func TestSweepStaleScratchAtStartup_HonorsEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	bot := "alice-1"
	myScratch := filepath.Join(tmp, "scratch", bot)

	// Seed a dir aged 2 hours. Under the 24h default it survives;
	// under a 1h override it's eligible.
	twoHourOld := filepath.Join(myScratch, "task-2h-iter-1")
	if err := os.MkdirAll(twoHourOld, 0o755); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(twoHourOld, t2, t2); err != nil {
		t.Fatal(err)
	}

	// Sanity: default keeps it.
	if n, err := compute.SweepStaleScratchAtStartup(tmp, bot); err != nil {
		t.Fatalf("default sweep: %v", err)
	} else if n != 0 {
		t.Errorf("default 24h: want 0 removed for a 2h-old dir, got %d", n)
	}
	if _, err := os.Stat(twoHourOld); err != nil {
		t.Fatalf("default sweep wrongly removed fresh dir: %v", err)
	}

	// Override to 1h → 2h-old dir is eligible.
	t.Setenv("ENJU_SCRATCH_PRESERVE_HOURS", "1")
	n, err := compute.SweepStaleScratchAtStartup(tmp, bot)
	if err != nil {
		t.Fatalf("override sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("override 1h: want 1 removed, got %d", n)
	}
	if _, err := os.Stat(twoHourOld); !os.IsNotExist(err) {
		t.Errorf("dir should be gone after override sweep, stat=%v", err)
	}
}

// TestSweepStaleScratchAtStartup_InvalidEnvFallsBackToDefault
// pins the safety-net behavior: a typo in
// ENJU_SCRATCH_PRESERVE_HOURS (non-numeric, negative, zero)
// silently falls back to the 24h default rather than disabling
// the sweep entirely.
func TestSweepStaleScratchAtStartup_InvalidEnvFallsBackToDefault(t *testing.T) {
	tmp := t.TempDir()
	bot := "alice-1"
	myScratch := filepath.Join(tmp, "scratch", bot)

	// Aged 2 hours.
	fresh := filepath.Join(myScratch, "fresh-iter-1")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(fresh, t2, t2)

	// 25 hours — eligible under default.
	aged := filepath.Join(myScratch, "aged-iter-1")
	if err := os.MkdirAll(aged, 0o755); err != nil {
		t.Fatal(err)
	}
	t25 := time.Now().Add(-25 * time.Hour)
	_ = os.Chtimes(aged, t25, t25)

	for _, bad := range []string{"oops", "-3", "0", ""} {
		t.Run("bad-"+bad, func(t *testing.T) {
			t.Setenv("ENJU_SCRATCH_PRESERVE_HOURS", bad)
			// Re-seed (previous subtest may have removed aged).
			_ = os.MkdirAll(aged, 0o755)
			_ = os.Chtimes(aged, t25, t25)
			n, err := compute.SweepStaleScratchAtStartup(tmp, bot)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			// Default behavior: aged removed, fresh kept.
			if n < 1 {
				t.Errorf("expected aged dir removed under fallback default, got n=%d", n)
			}
			if _, err := os.Stat(fresh); err != nil {
				t.Errorf("fresh dir was wrongly removed: %v", err)
			}
		})
	}
}

// TestSweepStaleScratchAtStartup_RespectsAgeFilter pins TP53
// Bug 2's preservation invariant against the startup sweep:
// fresh scratch dirs (younger than StaleScratchAgeThreshold)
// MUST survive a sweep so that a submit-failed retry can pick
// up the script's outputs from disk after a daemon restart.
//
// Without this filter, the wrapper's "preserve on submit-fail"
// behavior is meaningless — the next daemon start would wipe
// the dir before the operator's retry runs.
func TestSweepStaleScratchAtStartup_RespectsAgeFilter(t *testing.T) {
	tmp := t.TempDir()
	bot := "alice-1"
	myScratch := filepath.Join(tmp, "scratch", bot)

	// Fresh dir — simulates a submit-failed wrapper from the
	// previous daemon run that left outputs behind for retry.
	freshTask := filepath.Join(myScratch, "fresh-task-iter-1")
	if err := os.MkdirAll(freshTask, 0o755); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(freshTask, "output.md"), []byte("retry me"), 0o644); err != nil {
		t.Fatalf("seed output: %v", err)
	}
	// Aged dir — same shape but past the retry window.
	agedTask := filepath.Join(myScratch, "aged-task-iter-1")
	if err := os.MkdirAll(agedTask, 0o755); err != nil {
		t.Fatalf("seed aged: %v", err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(agedTask, old, old); err != nil {
		t.Fatalf("age aged: %v", err)
	}

	n, err := compute.SweepStaleScratchAtStartup(tmp, bot)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("removed count: got %d, want 1 (only aged)", n)
	}
	// Fresh dir + its output must survive.
	if _, err := os.Stat(freshTask); err != nil {
		t.Errorf("fresh scratch was wiped — retry would lose outputs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(freshTask, "output.md")); err != nil {
		t.Errorf("fresh output.md was wiped: %v", err)
	}
	// Aged dir is gone.
	if _, err := os.Stat(agedTask); !os.IsNotExist(err) {
		t.Errorf("aged scratch should have been removed, got err=%v", err)
	}
}
