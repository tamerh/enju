// StubHandler — canned responses for tests.
//
// The bot daemon's loop (claim → ProcessTask → submit) is the
// system under test for most bot integration coverage; the LLM
// invocation itself is incidental. StubHandler returns the
// canned text without spawning a subprocess so tests don't
// depend on `claude` being on PATH and don't introduce LLM
// non-determinism into the assertion shape.

package bots

import "context"

// StubHandler returns a fixed Response (or a fixed Err). Tests
// inject one per bot, sometimes one per test, and assert on what
// the daemon does with the response.
type StubHandler struct {
	// Response is what ProcessTask returns. Empty = a sentinel
	// "<stub: no canned response set>" so a test that forgot to
	// configure the handler fails loudly (rather than submitting
	// an empty string and confusing the coord).
	Response string

	// PrefillDecision (optional) drives the structured-output
	// path: when non-empty, the stub returns it on
	// HandlerOutput.Decision so the daemon's review-parsing
	// fallback is bypassed. Tests use this to exercise the
	// "custom Handler with its own response shape" code path
	// without writing a whole separate Handler implementation.
	PrefillDecision string

	// PrefillOption is the same idea for vote tasks.
	PrefillOption string

	// Err lets a test exercise the daemon's error-handling path
	// (rate-limit equivalents, model failures) without flakiness.
	Err error

	// Calls increments on every ProcessTask. Tests assert against
	// it to confirm the daemon reached the handler step.
	Calls int

	// Inputs records every brief seen by the handler. Tests
	// inspect it to confirm the daemon rendered the prompt /
	// system prompt / workspace correctly.
	Inputs []HandlerInput
}

func NewStubHandler() *StubHandler {
	return &StubHandler{}
}

// SkipClaimCWD opts out of the per-claim ephemeral CWD
// materialization (Phase 4c). Stub handlers don't read the
// project tree from their working directory — they return
// canned responses regardless. Materializing a fresh tree per
// claim for them would be pure overhead, especially across the
// many tests that spawn stub-handler bot daemons.
func (s *StubHandler) SkipClaimCWD() bool { return true }

func (s *StubHandler) ProcessTask(ctx context.Context, in HandlerInput) (HandlerOutput, error) {
	s.Calls++
	s.Inputs = append(s.Inputs, in)
	if s.Err != nil {
		return HandlerOutput{}, s.Err
	}
	resp := s.Response
	if resp == "" {
		resp = "<stub: no canned response set>"
	}
	return HandlerOutput{
		Response: resp,
		Decision: s.PrefillDecision,
		Option:   s.PrefillOption,
	}, nil
}
