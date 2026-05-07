package enjugit

import (
	"sort"
	"strings"
)

// op_trace.go — shared infrastructure for "errors that pinpoint
// where they broke." Every multi-step Workflow verb threads a
// stepTrace through its sequence of git operations, recording
// each step's outcome as it executes. When the verb fails, the
// returned WorkflowOpError carries the full trace so callers
// see *which step* succeeded, *which were skipped*, *which
// failed and why* — without log archaeology.
//
// Why this exists at the workflow layer (not deeper):
//
//   - The git layer's typed errors (ErrRefNotFound, ErrPushNonFF,
//     etc.) tell you WHAT went wrong at a single git op.
//   - But Enju verbs sequence multiple git ops with fallbacks
//     ("checkout local; if missing, track origin; if missing,
//     fork from default"). When a verb fails, the question is
//     usually "which fallback step was tried last, and why did
//     it fail?". That's a verb-level question, not a git-op
//     question.
//   - Each verb's failure modes vary, but the diagnostic SHAPE
//     is uniform: a list of (step name, status, detail) records
//     plus the wrapped typed cause.
//
// Performance: appending to a small []Step (5-7 entries typical)
// per verb call costs ~150 bytes + nanoseconds of allocation,
// dominated by the git I/O the verb actually does. Not in any
// hot path. See task #380 commentary for why this is the right
// trade.

// Step is one stage of a multi-step Workflow verb. Status is
// one of:
//
//   - "ok"      — step ran to completion successfully
//   - "skipped" — step intentionally not run (precondition met,
//                 not applicable, etc.); detail explains why
//   - "failed"  — step ran but errored; detail is the error
//                 string for human reading (machine routing
//                 uses WorkflowOpError.Cause via errors.Is)
type Step struct {
	Name   string
	Status string
	Detail string
}

// WorkflowOpError is the structured failure type returned by
// every multi-step Workflow verb. Wraps the verb's typed
// sentinel cause (so `errors.Is(err, ErrCannotForkBranch)` etc.
// keeps working for routing) and carries the step trace for
// "which step broke?" debugging.
//
// Verb-specific context (branch names, task IDs, run seqs) goes
// into the Context map at trace start so the rendered error
// shows the operands without each verb defining its own struct.
type WorkflowOpError struct {
	// Op is the verb name (e.g. "SubmitTaskResult"). Goes into
	// the Error() string's first line.
	Op string

	// Cause is the typed sentinel error this op terminated with.
	// errors.Unwrap returns it so errors.Is routes correctly.
	// Always non-nil for a real failure.
	Cause error

	// Steps records every step's outcome in execution order.
	// Read top-to-bottom for the chain.
	Steps []Step

	// Context carries verb-specific operands surfaced into the
	// rendered error (branch names, task IDs, etc.). Sorted
	// alphabetically when rendered for stable output.
	Context map[string]string
}

func (e *WorkflowOpError) Error() string {
	var b strings.Builder
	b.WriteString("enjugit: ")
	b.WriteString(e.Op)
	b.WriteString(" failed")
	if len(e.Context) > 0 {
		// Sort keys for deterministic rendering — important for
		// log/test diffs.
		keys := make([]string, 0, len(e.Context))
		for k := range e.Context {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" (")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(e.Context[k])
		}
		b.WriteString(")")
	}
	for _, s := range e.Steps {
		b.WriteString("\n  ")
		b.WriteString(s.Name)
		b.WriteString(": ")
		b.WriteString(s.Status)
		if s.Detail != "" {
			b.WriteString(" — ")
			b.WriteString(s.Detail)
		}
	}
	return b.String()
}

// Unwrap exposes the typed Cause so errors.Is works for caller
// routing. e.g. errors.Is(returnedErr, ErrSubmitVerifyFailed).
func (e *WorkflowOpError) Unwrap() error { return e.Cause }

// stepTrace is the verb-side helper for recording step outcomes
// inline. Verbs construct one at the start of their work, call
// ok/skipped/fail as steps execute, and either return nil
// (success) or trace.fail(...) (failure with the typed cause).
//
// Lowercase because this is internal infra for Workflow verbs;
// callers see WorkflowOpError, not stepTrace.
type stepTrace struct {
	op      string
	steps   []Step
	context map[string]string
}

// startTrace begins a new step trace for the named verb.
// Verbs call this once at function entry; each step then
// appends to it.
func startTrace(op string) *stepTrace {
	return &stepTrace{
		op:      op,
		context: map[string]string{},
	}
}

// ctx adds verb-specific context that will be rendered into
// the error string. Branch names, task IDs, etc. — anything
// that helps a human reader identify which call this was.
func (t *stepTrace) ctx(key, val string) {
	if val != "" {
		t.context[key] = val
	}
}

// ok records that a step ran successfully without extra detail.
func (t *stepTrace) ok(name string) {
	t.steps = append(t.steps, Step{Name: name, Status: "ok"})
}

// okDetail records a successful step with a human-readable
// detail (e.g. "forked from main @ abc12345"). Useful when the
// step did meaningful work whose outcome should be visible
// even in the success case (auto-heal, fallback choice).
func (t *stepTrace) okDetail(name, detail string) {
	t.steps = append(t.steps, Step{Name: name, Status: "ok", Detail: detail})
}

// skipped records that a step was intentionally not run, with
// a reason. Steps that don't apply ("no remote configured",
// "branch already at expected SHA") use this so the trace
// reads as a complete narrative rather than a gap.
func (t *stepTrace) skipped(name, reason string) {
	t.steps = append(t.steps, Step{Name: name, Status: "skipped", Detail: reason})
}

// fail records a failed step and returns the WorkflowOpError
// the verb should return upstream. The returned error wraps
// `cause` so callers can `errors.Is(err, sentinel)` for
// routing AND `errors.As(err, &*WorkflowOpError)` for
// diagnostic field reads.
func (t *stepTrace) fail(name string, cause error) error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	t.steps = append(t.steps, Step{Name: name, Status: "failed", Detail: detail})
	return &WorkflowOpError{
		Op:      t.op,
		Cause:   cause,
		Steps:   t.steps,
		Context: t.context,
	}
}

// wrapTerminal is for the case where a verb hits a typed
// sentinel as its terminal failure but the failing step has
// already been recorded (or doesn't fit the step model).
// e.g. all fallbacks exhausted; return ErrXxx with the chain.
func (t *stepTrace) wrapTerminal(cause error) error {
	return &WorkflowOpError{
		Op:      t.op,
		Cause:   cause,
		Steps:   t.steps,
		Context: t.context,
	}
}
