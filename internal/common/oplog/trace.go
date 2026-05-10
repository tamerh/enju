package oplog

// Step is one stage of a multi-step operation. Status is one of:
//
//   - "ok"      — step ran to completion successfully
//   - "skipped" — step intentionally not run (precondition met,
//                 not applicable, etc.); detail explains why
//   - "failed"  — step ran but errored; detail is the error
//                 string for human reading (machine routing
//                 uses OpError.Cause via errors.Is)
type Step struct {
	Name   string
	Status string
	Detail string
}

// Status values exposed for callers that want to grep / pattern-match
// on a known set rather than literal strings.
const (
	StatusOK      = "ok"
	StatusSkipped = "skipped"
	StatusFailed  = "failed"
)

// Trace records a multi-step operation's progress. Callers use Start
// at function entry, then OK/OKDetail/Skipped/Failed as steps run,
// and finally Emit (typically deferred) to surface the trace.
//
// Fields are exported so callers in the same module can inspect a
// trace mid-construction (e.g. counting failed steps before deciding
// what to return). Direct field mutation outside the package is
// discouraged but supported.
type Trace struct {
	Op      string
	Steps   []Step
	Context map[string]string
	// Terminal is true when the verb returned an error via this
	// Trace (Failed or WrapTerminal). Distinguishes a non-fatal
	// failed step (e.g. a recovered fetch-origin retry whose
	// failure is recorded via AppendStep but the verb keeps
	// running) from a step whose failure aborted the verb.
	// Emit uses this flag to choose ERROR vs WARN: a verb that
	// terminally failed is an actionable problem; one that
	// merely had a recoverable hiccup is informational.
	Terminal bool
}

// Start begins a new trace for the named operation.
func Start(op string) *Trace {
	return &Trace{
		Op:      op,
		Context: map[string]string{},
	}
}

// Ctx attaches operand context that renders into the error string
// and emit fields. Empty values are filtered (callers can pass
// optional values without guards).
func (t *Trace) Ctx(key, val string) {
	if t == nil || val == "" {
		return
	}
	t.Context[key] = val
}

// OK records a successful step with no extra detail.
func (t *Trace) OK(name string) {
	if t == nil {
		return
	}
	t.Steps = append(t.Steps, Step{Name: name, Status: StatusOK})
}

// OKDetail records a successful step with a human-readable detail
// (e.g. "forked from main @ abc12345"). Useful when the step did
// meaningful work whose outcome should be visible even on success.
func (t *Trace) OKDetail(name, detail string) {
	if t == nil {
		return
	}
	t.Steps = append(t.Steps, Step{Name: name, Status: StatusOK, Detail: detail})
}

// Skipped records that a step was intentionally not run. Reason
// goes into Detail so the trace reads as a complete narrative
// rather than a gap.
func (t *Trace) Skipped(name, reason string) {
	if t == nil {
		return
	}
	t.Steps = append(t.Steps, Step{Name: name, Status: StatusSkipped, Detail: reason})
}

// AppendStep is the escape hatch for callers that want full control
// over the Step record (e.g. recording a non-fatal failure that
// shouldn't terminate the trace). Most callers should prefer
// OK/OKDetail/Skipped/Failed.
func (t *Trace) AppendStep(s Step) {
	if t == nil {
		return
	}
	t.Steps = append(t.Steps, s)
}

// Failed records a failed step and returns an *OpError wrapping
// `cause`. Callers route on the typed cause via `errors.Is`; the
// trace travels with it for diagnostic display. Marks the trace
// Terminal so Emit logs at ERROR — a step recorded via Failed
// short-circuits the verb (the caller propagates the returned
// error), so the verb's outcome is "operation failed."
func (t *Trace) Failed(name string, cause error) error {
	if t == nil {
		return cause
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	t.Steps = append(t.Steps, Step{Name: name, Status: StatusFailed, Detail: detail})
	t.Terminal = true
	return &OpError{
		Op:      t.Op,
		Cause:   cause,
		Steps:   t.Steps,
		Context: t.Context,
	}
}

// WrapTerminal wraps `cause` in an OpError without adding a new
// failed step. Use when the failing step has already been recorded
// (or doesn't fit the step model — e.g. "all fallbacks exhausted").
// Marks the trace Terminal so Emit logs at ERROR.
func (t *Trace) WrapTerminal(cause error) error {
	if t == nil {
		return cause
	}
	t.Terminal = true
	return &OpError{
		Op:      t.Op,
		Cause:   cause,
		Steps:   t.Steps,
		Context: t.Context,
	}
}
