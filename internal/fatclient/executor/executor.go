// Package executor is the launch seam for compute-task wrappers.
//
// Everything around a compute task — snapshot materialization,
// env + context.json, scratch, bigfiles, the `enju wrap-task`
// wrapper (compute.Run / compute.WrapMain), compute.Spec /
// compute.Result, .wrap-result.json, the reconcile reaper, the
// failed_retryable / enju_retry_task machinery — is already
// executor-agnostic and stays byte-identical. An Executor owns
// exactly three things: WHERE the wrapper process runs, HOW it
// is launched, and HOW its completion is observed.
//
// The rest of the fat-client talks to this interface, never to
// os/exec or sbatch directly. LocalExecutor moves today's
// detached-fork path behind the seam (Option A — local goes
// through the interface on every ordinary run, which is what
// proves the seam); SlurmExecutor adds sbatch. K8s / AWS Batch
// / GCP Batch stay roadmap; a future one is a third
// implementation, nothing else changes.
//
// Import direction: this package lives under internal/fatclient
// and may import internal/common (yaml.Resources) but NOT
// internal/fatclient/service — service imports executor, so the
// reverse would be a cycle. It deliberately does not import
// enjugit either: launching a process needs no git.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// Executor kind strings. These are also the values of
// TaskDef.Executor / TaskMeta.Executor ("" is normalized to
// local by the caller before Pick).
const (
	KindLocal = "local"
	KindSlurm = "slurm"
)

// JobSidecarName is the persisted-Handle file, written at submit
// by every executor as a sibling of .wrap-spec.json /
// .wrap-result.json. It is what the reaper keys discovery off:
// a queued SLURM job has no .wrap-result.json yet, so the
// existing "walk for .wrap-result.json" cannot find it. Writing
// it for local too is what lets enju_terminate_run actually
// stop an in-flight local compute job (the PID is now persisted
// — Cancel has something to kill).
const JobSidecarName = ".wrap-job.json"

// Handle identifies a launched run for later Poll / Cancel. It
// is JSON-serializable: it IS the .wrap-job.json sidecar, so
// reaping and cancellation survive a fat-client restart.
type Handle struct {
	Executor    string    `json:"executor"`            // "local" | "slurm"
	PID         int       `json:"pid,omitempty"`       // local
	JobID       string    `json:"job_id,omitempty"`    // slurm
	ResultDir   string    `json:"result_dir"`          // where .wrap-result.json lands
	SubmittedAt time.Time `json:"submitted_at"`
}

// State is the launcher's coarse view of a run's progress. It
// deliberately does NOT encode script success/failure — that
// lives in .wrap-result.json and is the reaper's to interpret.
type State string

const (
	StateQueued  State = "queued"  // accepted by the launcher, not yet running (slurm PENDING)
	StateRunning State = "running" // process / job is live
	StateDone    State = "done"    // process/job terminated; inspect .wrap-result.json for exit
	StateLost    State = "lost"    // job vanished with no result — treat as failure
)

// Status is what Poll returns.
type Status struct {
	State State
	// Reason carries the executor's own terminal classification
	// when it differs from the script's exit (e.g. SLURM TIMEOUT
	// / OUT_OF_MEMORY / PREEMPTED / NODE_FAIL). Empty ⇒ defer to
	// .wrap-result.json.
	Reason string
}

// Executor launches a wrapper and observes it. Submit never
// waits; Poll never reads .wrap-result.json (that is the
// reaper's job — Poll only answers "is it still going?");
// Cancel stops a queued/running run.
//
// Poll contract: when the returned error is non-nil the Status
// is UNDEFINED and must not be acted on — the polling
// machinery (sacct, etc.) couldn't get a definitive answer, so
// callers treat err!=nil as "unknown, try again next sweep"
// and must check err before State. (SlurmExecutor returns
// {StateRunning}+err in that case purely so a caller that
// ignores err still degrades to a benign retry rather than a
// spurious terminal; the value itself carries no meaning when
// err!=nil.)
type Executor interface {
	Submit(ctx context.Context, specPath, outputPath string, env []string, r enjuYaml.Resources) (Handle, error)
	Poll(ctx context.Context, h Handle) (Status, error)
	Cancel(ctx context.Context, h Handle) error
}

// Pick returns the executor implementation for a task's
// (already param-resolved) executor string. "" and "local" →
// LocalExecutor; "slurm" → SlurmExecutor. Anything else is a
// programming error here: validateTaskExecutor already rejected
// unknown values at parse time, so reaching Pick with one means
// the wire carried something the validator should have stopped.
func Pick(kind string) (Executor, error) {
	switch kind {
	case "", KindLocal:
		return LocalExecutor{}, nil
	case KindSlurm:
		return SlurmExecutor{}, nil
	default:
		return nil, fmt.Errorf("unknown executor %q (validateTaskExecutor should have rejected this upstream)", kind)
	}
}

// writeJobSidecar persists h as JobSidecarName in h.ResultDir.
// Called by every Submit as its last step, so a returned Handle
// is always already on disk — Poll / Cancel / the reaper can
// then operate purely off the sidecar, even across a restart.
func writeJobSidecar(h Handle) error {
	if h.ResultDir == "" {
		return fmt.Errorf("handle has no ResultDir — cannot persist %s", JobSidecarName)
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding job sidecar: %w", err)
	}
	path := filepath.Join(h.ResultDir, JobSidecarName)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// ReadJobSidecar decodes a .wrap-job.json the reaper found while
// walking. Exported because the reaper (service package) drives
// discovery off these files.
func ReadJobSidecar(path string) (Handle, error) {
	var h Handle
	b, err := os.ReadFile(path)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, fmt.Errorf("decoding %s: %w", path, err)
	}
	return h, nil
}

// resultDirOf derives the result dir from the outputPath the
// caller passed to Submit (.wrap-result.json's directory). The
// sidecar, batch script, and wrapper log all live beside it.
func resultDirOf(outputPath string) string { return filepath.Dir(outputPath) }
