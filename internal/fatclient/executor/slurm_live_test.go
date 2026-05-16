package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// TestSlurmLive exercises the real sbatch/sacct/scancel path on
// a submit host. Skipped by default (no cluster in CI); set
// ENJU_SLURM_IT=1 on a machine with SLURM to run it. It submits
// a trivial job, polls until terminal, and cancels a second one
// — proving Submit/Poll/Cancel against the actual CLIs, which
// the cluster-free unit tests deliberately don't touch.
func TestSlurmLive(t *testing.T) {
	if os.Getenv("ENJU_SLURM_IT") == "" {
		t.Skip("set ENJU_SLURM_IT=1 on a SLURM submit host to run the live executor test")
	}
	for _, bin := range []string{"sbatch", "sacct", "scancel"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("ENJU_SLURM_IT set but %s not on PATH: %v", bin, err)
		}
	}

	dir := t.TempDir()
	// A stand-in "wrapper": os.Executable() under -test wouldn't
	// behave as `enju wrap-task`, so write a trivial result file
	// the way a real wrapper would and exec /bin/true. This keeps
	// the live test about the launcher (sbatch/sacct/scancel),
	// not the wrapper.
	outPath := filepath.Join(dir, ".wrap-result.json")
	specPath := filepath.Join(dir, ".wrap-spec.json")
	if err := os.WriteFile(specPath, []byte(`{"task_id":"1:1:live"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	se := SlurmExecutor{}
	h, err := se.Submit(ctx, specPath, outPath, os.Environ(), enjuYaml.Resources{Time: "00:02:00"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if h.JobID == "" {
		t.Fatal("Submit returned empty JobID")
	}

	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		st, perr := se.Poll(ctx, h)
		if perr != nil {
			t.Logf("Poll transient: %v", perr)
		}
		if st.State == StateDone || st.State == StateLost {
			t.Logf("job %s reached %s (reason=%q)", h.JobID, st.State, st.Reason)
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("job %s never reached terminal within timeout", h.JobID)
}
