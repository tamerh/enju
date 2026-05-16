package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// SbatchScriptName is the generated batch script, written beside
// .wrap-spec.json so a human can read exactly what was submitted.
const SbatchScriptName = ".wrap-sbatch.sh"

// SlurmExecutor submits the wrapper as a single sbatch job on
// the cluster this fat-client runs on (shared-FS assumption:
// submit host and compute nodes see the same WorkDir / snapshot
// / scratch / bigfiles / repo and the same enju binary path —
// no stage-in/out). One enju task = one sbatch job; enju's DAG
// owns dependencies, so no SLURM array/dependency jobs.
type SlurmExecutor struct{}

var _ Executor = SlurmExecutor{}

// Submit writes a batch script that, on the compute node, just
// execs the wrapper against the shared-FS paths, then `sbatch
// --parsable`s it. The node never touches git: DeferCommit lives
// in the serialized .wrap-spec.json the wrapper reads, so the
// node skips the commit from the spec, not from any launcher
// flag. The wrapper's existing container path runs inside the
// job unchanged (container is orthogonal to executor).
func (SlurmExecutor) Submit(ctx context.Context, specPath, outputPath string, env []string, r enjuYaml.Resources) (Handle, error) {
	self, err := os.Executable()
	if err != nil {
		return Handle{}, fmt.Errorf("locating enju binary: %w", err)
	}
	resultDir := resultDirOf(outputPath)
	jobName := "enju-" + strings.ReplaceAll(filepath.Base(specPath), ".wrap-spec.json", "")
	if jobName == "enju-" {
		jobName = "enju-task"
	}

	script := buildSbatchScript(self, specPath, outputPath, resultDir, jobName, env, r)
	scriptPath := filepath.Join(resultDir, SbatchScriptName)
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return Handle{}, fmt.Errorf("writing %s: %w", scriptPath, err)
	}

	out, err := exec.CommandContext(ctx, "sbatch", "--parsable", scriptPath).CombinedOutput()
	if err != nil {
		return Handle{}, fmt.Errorf("sbatch failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// --parsable prints "<jobid>" or "<jobid>;<cluster>".
	jobID := strings.TrimSpace(string(out))
	if i := strings.IndexByte(jobID, ';'); i >= 0 {
		jobID = jobID[:i]
	}
	if jobID == "" {
		return Handle{}, fmt.Errorf("sbatch --parsable returned no job id (output: %q)", strings.TrimSpace(string(out)))
	}

	h := Handle{
		Executor:    KindSlurm,
		JobID:       jobID,
		ResultDir:   resultDir,
		SubmittedAt: time.Now().UTC(),
	}
	if err := writeJobSidecar(h); err != nil {
		return h, fmt.Errorf("sbatch submitted job %s but persisting %s failed: %w", jobID, JobSidecarName, err)
	}
	return h, nil
}

// Poll maps `sacct` job state to launcher State. It never reads
// .wrap-result.json — exit interpretation is the reaper's. A
// missing row means the job vanished from accounting: StateLost,
// which the reaper resolves against .wrap-result.json (present →
// use it; absent → fail "slurm job lost").
func (SlurmExecutor) Poll(ctx context.Context, h Handle) (Status, error) {
	out, err := exec.CommandContext(ctx, "sacct", "-j", h.JobID, "-n", "-P", "-o", "JobID,State").Output()
	if err != nil {
		// sacct itself failed (slurmdbd down, not on a submit
		// host). Per the Executor.Poll contract the Status is
		// undefined when err != nil; the StateRunning here is a
		// benign default for any caller that (incorrectly)
		// ignores err — it degrades to "retry next sweep", never
		// a spurious terminal. The reaper checks err first.
		return Status{State: StateRunning}, fmt.Errorf("sacct -j %s: %w", h.JobID, err)
	}
	raw := sacctMainState(string(out), h.JobID)
	if raw == "" {
		return Status{State: StateLost, Reason: "slurm job " + h.JobID + " not found in sacct"}, nil
	}
	st, reason := MapSacctState(raw)
	return Status{State: st, Reason: reason}, nil
}

// Cancel scancels a queued/running job. scancel on an already-
// finished job is a harmless no-op, so Cancel is idempotent —
// safe for enju_terminate_run to call on every outstanding
// handle without first checking state.
func (SlurmExecutor) Cancel(ctx context.Context, h Handle) error {
	if h.JobID == "" {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "scancel", h.JobID).CombinedOutput(); err != nil {
		return fmt.Errorf("scancel %s: %w: %s", h.JobID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sacctMainState extracts the State of the main job row (JobID
// exactly == id, not id.batch / id.extern / id.0) from
// `sacct -n -P -o JobID,State` output. Returns "" when no such
// row exists (job aged out of accounting / never recorded).
func sacctMainState(out, id string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "|", 2)
		if len(f) != 2 {
			continue
		}
		if strings.TrimSpace(f[0]) == id {
			return strings.TrimSpace(f[1])
		}
	}
	return ""
}

// MapSacctState maps a raw sacct State token to a launcher State
// + an optional Reason. Exported so the reaper and the
// table-driven test share one source of truth for the
// sacct→enju matrix (§5 of the spec).
//
//   - PENDING/CONFIGURING/REQUEUED/RESIZING/SUSPENDED → Queued
//   - RUNNING/COMPLETING                              → Running
//   - COMPLETED/FAILED → Done, reason "" (defer to exit code in
//     .wrap-result.json — COMPLETED+exit0 ⇒ /result; otherwise
//     ⇒ /fail compute_error → failed_retryable)
//   - TIMEOUT/OUT_OF_MEMORY/PREEMPTED/NODE_FAIL/BOOT_FAIL/
//     DEADLINE → Done, reason = the SLURM state (transient infra;
//     composes with enju_retry_task from=snapshot — code was fine)
//   - CANCELLED[ by ...] → Done, reason "cancelled" (the
//     run-terminate / pause path; reaper does not race a /fail)
//   - anything else → Done, reason = raw (conservative: terminal,
//     surface the unknown state rather than hang)
//
// sacct sometimes suffixes a state with '+' (truncation) or
// " by <uid>" (CANCELLED) — both are normalized here.
func MapSacctState(raw string) (State, string) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "+")
	if i := strings.IndexByte(s, ' '); i >= 0 { // "CANCELLED by 1001"
		s = s[:i]
	}
	switch strings.ToUpper(s) {
	case "PENDING", "CONFIGURING", "REQUEUED", "RESIZING", "SUSPENDED":
		return StateQueued, ""
	case "RUNNING", "COMPLETING":
		return StateRunning, ""
	case "COMPLETED", "FAILED":
		return StateDone, ""
	case "TIMEOUT", "OUT_OF_MEMORY", "PREEMPTED", "NODE_FAIL", "BOOT_FAIL", "DEADLINE":
		return StateDone, strings.ToUpper(s)
	case "CANCELLED":
		return StateDone, "cancelled"
	default:
		return StateDone, strings.ToUpper(s)
	}
}

// buildSbatchScript renders the batch script. Zero-valued
// resource knobs are omitted so SLURM applies its site defaults;
// SbatchExtra lines are spliced verbatim as additional #SBATCH
// directives (the escape hatch for everything the five modeled
// knobs don't cover). env is re-exported explicitly so the node
// sees exactly the assembled ENJU_* + task env regardless of the
// cluster's sbatch --export policy.
func buildSbatchScript(self, specPath, outputPath, resultDir, jobName string, env []string, r enjuYaml.Resources) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	// #SBATCH directive values are parsed by sbatch, NOT the
	// shell — sbatch does no quote-stripping and treats %j as a
	// pattern token. So these are deliberately NOT
	// shellSingleQuote'd (unlike the env exports / exec line
	// below, which ARE shell-evaluated): quoting an #SBATCH path
	// would feed sbatch a literal-quote-wrapped filename and
	// break %j expansion. resultDir / jobName are Enju-controlled
	// (sanitized run slug + task id), so the unquoted form is
	// safe; the asymmetry with the shell body is intentional, not
	// an oversight.
	fmt.Fprintf(&b, "#SBATCH --job-name=%s\n", jobName)
	out := filepath.Join(resultDir, "slurm-%j.out")
	fmt.Fprintf(&b, "#SBATCH --output=%s\n", out)
	fmt.Fprintf(&b, "#SBATCH --error=%s\n", out)
	if r.Partition != "" {
		fmt.Fprintf(&b, "#SBATCH --partition=%s\n", r.Partition)
	}
	if r.Time != "" {
		fmt.Fprintf(&b, "#SBATCH --time=%s\n", r.Time)
	}
	if r.CPUs > 0 {
		fmt.Fprintf(&b, "#SBATCH --cpus-per-task=%d\n", r.CPUs)
	}
	if r.Mem != "" {
		fmt.Fprintf(&b, "#SBATCH --mem=%s\n", r.Mem)
	}
	if r.GPUs > 0 {
		fmt.Fprintf(&b, "#SBATCH --gpus=%d\n", r.GPUs)
	}
	for _, extra := range r.SbatchExtra {
		extra = strings.TrimSpace(extra)
		if extra != "" {
			fmt.Fprintf(&b, "#SBATCH %s\n", extra)
		}
	}
	b.WriteString("\n")
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", k, shellSingleQuote(v))
	}
	b.WriteString("\n")
	// exec: the wrapper replaces the shell so the job's exit code
	// is the wrapper's. DeferCommit=true lives in the spec the
	// wrapper reads — the node learns to skip the commit from the
	// spec, not from this script.
	fmt.Fprintf(&b, "exec %s wrap-task --spec %s --output %s\n",
		shellSingleQuote(self), shellSingleQuote(specPath), shellSingleQuote(outputPath))
	return b.String()
}

// shellSingleQuote wraps s in single quotes, escaping embedded
// single quotes via the '\'' idiom. Used for env values and
// paths spliced into the batch script so a value containing
// spaces / $ / ; can't break out.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
