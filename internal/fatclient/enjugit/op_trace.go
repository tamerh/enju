package enjugit

// op_trace.go — thin local shim over internal/common/oplog so
// existing in-package usage (`startTrace`, `*WorkflowOpError`,
// `Step`, `t.ok` / `t.skipped` / `t.fail`, etc.) keeps compiling
// while the genuinely shared trace + file-sink machinery lives
// in oplog. Verbs `defer trace.emit(w.logger, w.traceFile)` to
// surface the trace to slog and the per-project log file.
//
// The shim exists so a Workflow change that wanted to add a new
// step type or alter the trace shape doesn't need to touch every
// verb call site — just the shim. When the shim's value drops
// (e.g. cross-package consumers of OpError grow), it can collapse
// into direct oplog references.

import (
	"log/slog"
	"os"

	"github.com/enju-ai/enju/internal/common/oplog"
)

// Step is the per-stage record. Aliased to oplog.Step so external
// callers (mostly tests) can keep referring to enjugit.Step.
type Step = oplog.Step

// WorkflowOpError is the structured failure type returned by every
// multi-step Workflow verb. Aliased to oplog.OpError so existing
// `errors.As(err, &*WorkflowOpError)` calls and the package
// docstrings continue to work.
type WorkflowOpError = oplog.OpError

// stepTrace is the verb-side handle. Wraps *oplog.Trace so verbs
// keep using the lowercase ok/skipped/fail vocabulary, and so the
// `emit(logger, file)` shape stays internal to enjugit.
type stepTrace struct {
	t *oplog.Trace
}

// startTrace begins a new trace for the named verb.
func startTrace(op string) *stepTrace {
	return &stepTrace{t: oplog.Start(op)}
}

func (s *stepTrace) ctx(key, val string)              { s.t.Ctx(key, val) }
func (s *stepTrace) ok(name string)                   { s.t.OK(name) }
func (s *stepTrace) okDetail(name, detail string)     { s.t.OKDetail(name, detail) }
func (s *stepTrace) skipped(name, reason string)      { s.t.Skipped(name, reason) }
func (s *stepTrace) fail(name string, cause error) error {
	return s.t.Failed(name, cause)
}
func (s *stepTrace) wrapTerminal(cause error) error { return s.t.WrapTerminal(cause) }

// steps is a backward-compat accessor used by the few sites that
// manually `append(trace.steps, Step{...})` for non-fatal failed
// steps (fetch-origin retries). Returns the underlying slice; the
// caller mutates it in place. Keep this until those sites move
// to a proper "non-fatal failed step" helper.
func (s *stepTrace) appendStep(step Step) { s.t.AppendStep(step) }

// emit forwards to oplog.Trace.Emit. Verbs use:
//
//	trace := startTrace("MyVerb")
//	defer trace.emit(w.logger, w.traceFile)
//
// traceFile is the per-process append-only log opened by
// Workspace at <projectRoot>/enju/logs/trace-<pid>.log.
func (s *stepTrace) emit(logger *slog.Logger, traceFile *os.File) {
	s.t.Emit(logger, traceFile)
}

// stepsView is a read-only view of recorded steps. Used by tests
// that assert on trace shape without poking the internal slice.
func (s *stepTrace) stepsView() []Step {
	return s.t.Steps
}
