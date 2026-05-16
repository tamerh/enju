package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// TestMapSacctState pins the sacct→enju matrix (spec §5). This
// is the one place the SLURM-state classification lives; the
// reaper and any future caller read through it, so the table is
// the contract.
func TestMapSacctState(t *testing.T) {
	cases := []struct {
		raw       string
		wantState State
		wantReason string
	}{
		{"PENDING", StateQueued, ""},
		{"CONFIGURING", StateQueued, ""},
		{"REQUEUED", StateQueued, ""},
		{"SUSPENDED", StateQueued, ""},
		{"RUNNING", StateRunning, ""},
		{"COMPLETING", StateRunning, ""},
		{"COMPLETED", StateDone, ""},      // exit code in .wrap-result.json decides
		{"FAILED", StateDone, ""},         // ditto
		{"TIMEOUT", StateDone, "TIMEOUT"}, // transient infra → reason carries it
		{"OUT_OF_MEMORY", StateDone, "OUT_OF_MEMORY"},
		{"PREEMPTED", StateDone, "PREEMPTED"},
		{"NODE_FAIL", StateDone, "NODE_FAIL"},
		{"BOOT_FAIL", StateDone, "BOOT_FAIL"},
		{"DEADLINE", StateDone, "DEADLINE"},
		{"CANCELLED", StateDone, "cancelled"},
		{"CANCELLED by 1001", StateDone, "cancelled"}, // " by <uid>" normalized
		{"COMPLETED+", StateDone, ""},                  // '+' truncation normalized
		{"running", StateRunning, ""},                  // case-insensitive
		{"WEIRD_NEW_STATE", StateDone, "WEIRD_NEW_STATE"}, // unknown → terminal, surfaced
	}
	for _, c := range cases {
		gotState, gotReason := MapSacctState(c.raw)
		if gotState != c.wantState || gotReason != c.wantReason {
			t.Errorf("MapSacctState(%q) = (%s,%q), want (%s,%q)",
				c.raw, gotState, gotReason, c.wantState, c.wantReason)
		}
	}
}

// TestSacctMainState extracts the main job row and ignores the
// .batch / .extern / step sub-rows sacct emits.
func TestSacctMainState(t *testing.T) {
	out := strings.Join([]string{
		"12345|COMPLETED",
		"12345.batch|COMPLETED",
		"12345.extern|COMPLETED",
		"12345.0|FAILED",
	}, "\n")
	if got := sacctMainState(out, "12345"); got != "COMPLETED" {
		t.Errorf("sacctMainState main row: got %q, want COMPLETED", got)
	}
	if got := sacctMainState(out, "99999"); got != "" {
		t.Errorf("sacctMainState missing job: got %q, want \"\"", got)
	}
	if got := sacctMainState("", "12345"); got != "" {
		t.Errorf("sacctMainState empty: got %q, want \"\"", got)
	}
}

// TestPick maps the executor string to an implementation; ""
// and "local" collapse to LocalExecutor, "slurm" to
// SlurmExecutor, anything else errors (the validator should
// have stopped it upstream).
func TestPick(t *testing.T) {
	for _, k := range []string{"", "local"} {
		ex, err := Pick(k)
		if err != nil {
			t.Fatalf("Pick(%q): %v", k, err)
		}
		if _, ok := ex.(LocalExecutor); !ok {
			t.Errorf("Pick(%q) = %T, want LocalExecutor", k, ex)
		}
	}
	ex, err := Pick("slurm")
	if err != nil {
		t.Fatalf("Pick(slurm): %v", err)
	}
	if _, ok := ex.(SlurmExecutor); !ok {
		t.Errorf("Pick(slurm) = %T, want SlurmExecutor", ex)
	}
	if _, err := Pick("k8s"); err == nil {
		t.Error("Pick(k8s): expected error, got nil")
	}
}

// TestJobSidecarRoundTrip writes a Handle the way Submit does
// and reads it back the way the reaper does.
func TestJobSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := Handle{
		Executor:    KindSlurm,
		JobID:       "98765",
		ResultDir:   dir,
		SubmittedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := writeJobSidecar(h); err != nil {
		t.Fatalf("writeJobSidecar: %v", err)
	}
	got, err := ReadJobSidecar(filepath.Join(dir, JobSidecarName))
	if err != nil {
		t.Fatalf("ReadJobSidecar: %v", err)
	}
	if got.Executor != h.Executor || got.JobID != h.JobID || got.ResultDir != h.ResultDir || !got.SubmittedAt.Equal(h.SubmittedAt) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, h)
	}
	// writeJobSidecar must reject a handle with no ResultDir —
	// otherwise the reaper/Cancel have nothing to key off.
	if err := writeJobSidecar(Handle{Executor: KindLocal, PID: 1}); err == nil {
		t.Error("writeJobSidecar with empty ResultDir: expected error, got nil")
	}
}

// TestBuildSbatchScript checks the generated batch script:
// modeled knobs become #SBATCH lines, zero knobs are omitted
// (SLURM site defaults), sbatch_extra is spliced verbatim, env
// is single-quoted, and the wrapper is exec'd (so the job's
// exit code is the wrapper's).
func TestBuildSbatchScript(t *testing.T) {
	r := enjuYaml.Resources{
		Partition:   "gpu",
		Time:        "02:00:00",
		CPUs:        8,
		Mem:         "32G",
		GPUs:        1,
		SbatchExtra: []string{"--account=lab123", "--constraint=a100"},
	}
	env := []string{"ENJU_TASK_ID=1:2:align", "ENJU_PARAM_x=a b'c", "MALFORMED"}
	s := buildSbatchScript("/opt/enju", "/r/.wrap-spec.json", "/r/.wrap-result.json", "/r", "enju-1-2-align", env, r)

	must := []string{
		"#!/bin/bash",
		"#SBATCH --job-name=enju-1-2-align",
		"#SBATCH --output=/r/slurm-%j.out",
		"#SBATCH --error=/r/slurm-%j.out",
		"#SBATCH --partition=gpu",
		"#SBATCH --time=02:00:00",
		"#SBATCH --cpus-per-task=8",
		"#SBATCH --mem=32G",
		"#SBATCH --gpus=1",
		"#SBATCH --account=lab123",
		"#SBATCH --constraint=a100",
		"export ENJU_TASK_ID='1:2:align'",
		`export ENJU_PARAM_x='a b'\''c'`, // single-quote escaped
		"exec '/opt/enju' wrap-task --spec '/r/.wrap-spec.json' --output '/r/.wrap-result.json'",
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("script missing %q\n--- script ---\n%s", m, s)
		}
	}
	if strings.Contains(s, "MALFORMED") {
		t.Errorf("malformed env entry (no '=') should be skipped, got it in:\n%s", s)
	}

	// Zero knobs omitted entirely so SLURM applies site defaults.
	z := buildSbatchScript("/e", "/r/.wrap-spec.json", "/r/o.json", "/r", "enju-z", nil, enjuYaml.Resources{})
	for _, banned := range []string{"--partition", "--time", "--cpus-per-task", "--mem", "--gpus"} {
		if strings.Contains(z, banned) {
			t.Errorf("zero Resources should omit %q, got:\n%s", banned, z)
		}
	}
}

func TestShellSingleQuote(t *testing.T) {
	cases := map[string]string{
		"plain":    "'plain'",
		"a b":      "'a b'",
		"a'b":      `'a'\''b'`,
		"$(rm -rf)": "'$(rm -rf)'",
		"":         "''",
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReadJobSidecarMalformed — a corrupt sidecar must error,
// not panic, so the reaper can skip it.
func TestReadJobSidecarMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), JobSidecarName)
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJobSidecar(p); err == nil {
		t.Error("expected error decoding malformed sidecar, got nil")
	}
	// Sanity: a well-formed Handle still decodes.
	good, _ := json.Marshal(Handle{Executor: KindLocal, PID: 7, ResultDir: "/x"})
	_ = os.WriteFile(p, good, 0o600)
	if h, err := ReadJobSidecar(p); err != nil || h.PID != 7 {
		t.Errorf("good sidecar: h=%+v err=%v", h, err)
	}
}
