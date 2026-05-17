package bots

import (
	"os"
)

// Phase classifies a bot daemon's lifecycle stage so the
// supervisor can decide when create_run's auto_agents wait should
// unblock. Three states:
//
//   PhaseStarting    — process is up; flags parsed; pre-flight
//                      checks (git on PATH, workflow YAML, bot
//                      manifest lookup) are running. The daemon
//                      can't claim tasks in this phase.
//   PhaseSelfHealing — credentials file was missing; the daemon
//                      is registering itself against the
//                      coordinator using the operator's owner
//                      credentials. May involve network calls
//                      that take a few seconds.
//   PhaseReady       — startup recovery is done, the poll loop
//                      has entered. Claim attempts are firing.
//                      This is the post-condition create_run
//                      auto_agents is waiting on.
//
// The signal is best-effort: phase writes are appended to a
// file named by ENJU_BOT_PHASE_FILE (set by the supervisor when
// it spawns the daemon). When the env var is empty (the daemon
// was started directly by an operator via `enju bot run`, not
// through the supervisor), WritePhase is a no-op. Phase reads
// on a missing file return PhaseUnknown — semantically "we
// can't see the bot yet."
type Phase string

const (
	PhaseUnknown     Phase = ""
	PhaseStarting    Phase = "starting"
	PhaseSelfHealing Phase = "self_healing"
	PhaseReady       Phase = "ready"
)

// PhaseFileEnv names the environment variable the supervisor
// uses to tell each daemon where to drop its phase markers.
// One file per bot; the supervisor controls the path so tests
// (which use a t.TempDir-rooted Supervisor) can stub it.
const PhaseFileEnv = "ENJU_BOT_PHASE_FILE"

// WritePhase persists p to ENJU_BOT_PHASE_FILE atomically. Used
// by the daemon at lifecycle transitions. Best-effort: any
// error is returned for logging but does not abort the daemon —
// the phase signal is supervisor-side coordination, not daemon
// correctness. Empty env var = no-op (daemon was started
// outside the supervisor flow).
//
// The 0600 mode mirrors the pid-file's privacy posture; the
// file lives in the supervisor's PIDDir alongside <bot>.json.
func WritePhase(p Phase) error {
	path := os.Getenv(PhaseFileEnv)
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(p), 0600)
}
