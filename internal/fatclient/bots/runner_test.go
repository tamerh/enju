package bots

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCoord drives the Runner deterministically. Records every
// call so tests can assert what the runner did + when.
type fakeCoord struct {
	mu sync.Mutex

	// readyQueue is the FIFO of ready-task batches the runner
	// will see on successive ListReadyForBot calls. Empty
	// batch surfaces ErrNoWork to the runner.
	readyQueue [][]TaskInfo

	// claimErrors maps task IDs to the error to return when
	// Claim is called for that ID. Useful for ErrClaimRace
	// scenarios.
	claimErrors map[string]error

	// submitError, when non-nil, fails every Submit call.
	submitError error

	// Recordings — tests assert against these.
	listCalls    int
	claimCalls   []string // task IDs, in call order
	submitCalls  []submitRecord
	releaseCalls []releaseRecord

	// blockSubmit, when non-nil, holds the Submit call until
	// the channel is closed. Lets tests pause an iteration
	// mid-flight to exercise the shutdown-during-LLM path.
	blockSubmit chan struct{}
}

type submitRecord struct {
	TaskID   string
	Response string
	Model    string
}

// releaseRecord captures Release calls for shutdown-path tests.
type releaseRecord struct {
	TaskID      string
	BotUsername string
}

func (f *fakeCoord) ListReadyForBot(ctx context.Context, projectID int64, botUsername string) ([]TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if len(f.readyQueue) == 0 {
		return nil, nil
	}
	batch := f.readyQueue[0]
	f.readyQueue = f.readyQueue[1:]
	return batch, nil
}

func (f *fakeCoord) Claim(ctx context.Context, taskID, botUsername, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls = append(f.claimCalls, taskID)
	if err, ok := f.claimErrors[taskID]; ok {
		return err
	}
	return nil
}

func (f *fakeCoord) Submit(ctx context.Context, task TaskInfo, response, model string) error {
	f.mu.Lock()
	if f.blockSubmit != nil {
		ch := f.blockSubmit
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
	}
	defer f.mu.Unlock()
	f.submitCalls = append(f.submitCalls, submitRecord{TaskID: task.ID, Response: response, Model: model})
	return f.submitError
}

func (f *fakeCoord) Release(ctx context.Context, taskID, botUsername string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, releaseRecord{TaskID: taskID, BotUsername: botUsername})
	return nil
}

func newRunner(coord *fakeCoord, llm LLMBackend) *Runner {
	return &Runner{
		Bot:          &Bot{Name: "test-bot", Model: "test-model"},
		ProjectID:    7,
		Coord:        coord,
		LLM:          llm,
		SystemPrompt: "you are a tester",
		PollInterval: 10 * time.Millisecond,
		BackoffMax:   50 * time.Millisecond,
	}
}

func TestRunOnce_HappyPath(t *testing.T) {
	coord := &fakeCoord{
		readyQueue: [][]TaskInfo{
			{{ID: "1:1:review", Action: "review", Prompt: "review the draft"}},
		},
	}
	llm := &StubBackend{RespondWith: "approve — looks good"}
	r := newRunner(coord, llm)

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(coord.claimCalls) != 1 || coord.claimCalls[0] != "1:1:review" {
		t.Errorf("claim: got %v, want [1:1:review]", coord.claimCalls)
	}
	if len(coord.submitCalls) != 1 {
		t.Fatalf("submit: got %d calls, want 1", len(coord.submitCalls))
	}
	got := coord.submitCalls[0]
	if got.TaskID != "1:1:review" || got.Response != "approve — looks good" || got.Model != "test-model" {
		t.Errorf("submit payload: %+v", got)
	}
	if llm.Calls != 1 {
		t.Errorf("LLM Calls: got %d, want 1", llm.Calls)
	}
}

func TestRunOnce_NoWork(t *testing.T) {
	coord := &fakeCoord{readyQueue: [][]TaskInfo{nil}}
	r := newRunner(coord, &StubBackend{RespondWith: "x"})
	err := r.RunOnce(context.Background())
	if !errors.Is(err, ErrNoWork) {
		t.Errorf("err: got %v, want ErrNoWork", err)
	}
	if len(coord.claimCalls) != 0 {
		t.Errorf("should not claim when nothing ready, got %v", coord.claimCalls)
	}
}

func TestRunOnce_ClaimRaceSkipsToNext(t *testing.T) {
	// First task is raced away; runner should try the second
	// without bailing on the iteration.
	coord := &fakeCoord{
		readyQueue: [][]TaskInfo{
			{
				{ID: "1:1:a", Action: "review", Prompt: "p1"},
				{ID: "1:1:b", Action: "review", Prompt: "p2"},
			},
		},
		claimErrors: map[string]error{"1:1:a": ErrClaimRace},
	}
	llm := &StubBackend{RespondWith: "approve"}
	r := newRunner(coord, llm)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(coord.claimCalls) != 2 || coord.claimCalls[1] != "1:1:b" {
		t.Errorf("claim path: got %v, want [1:1:a, 1:1:b]", coord.claimCalls)
	}
	if len(coord.submitCalls) != 1 || coord.submitCalls[0].TaskID != "1:1:b" {
		t.Errorf("submit: got %+v, want one for 1:1:b", coord.submitCalls)
	}
}

func TestRunOnce_AllRacedReportsNoWork(t *testing.T) {
	// Every task in the batch is raced away — return ErrNoWork
	// so the Run loop applies backoff (don't burn CPU re-listing
	// in a tight loop).
	coord := &fakeCoord{
		readyQueue: [][]TaskInfo{
			{{ID: "x", Action: "review"}},
		},
		claimErrors: map[string]error{"x": ErrClaimRace},
	}
	r := newRunner(coord, &StubBackend{RespondWith: "x"})
	err := r.RunOnce(context.Background())
	if !errors.Is(err, ErrNoWork) {
		t.Errorf("err: got %v, want ErrNoWork", err)
	}
}

func TestRunOnce_LLMErrorDoesNotSubmit(t *testing.T) {
	// LLM failure: claim is open, but submitting a stub error
	// message would be worse than leaving the claim alone.
	coord := &fakeCoord{
		readyQueue: [][]TaskInfo{
			{{ID: "1:1:x", Action: "review", Prompt: "p"}},
		},
	}
	llm := &StubBackend{Err: errors.New("rate limited")}
	r := newRunner(coord, llm)
	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err: got %v, want rate-limit error", err)
	}
	if len(coord.submitCalls) != 0 {
		t.Errorf("should not submit on LLM failure, got %v", coord.submitCalls)
	}
}

func TestRun_LoopProcessesAndExitsOnContext(t *testing.T) {
	// Two tasks queued, then nothing — Run should process both
	// and idle until the context cancels.
	coord := &fakeCoord{
		readyQueue: [][]TaskInfo{
			{{ID: "1:1:a", Action: "review", Prompt: "p1"}},
			{{ID: "1:1:b", Action: "vote", Prompt: "p2"}},
			nil, // backoff round
			nil,
		},
	}
	llm := &StubBackend{RespondWith: "ok"}
	r := newRunner(coord, llm)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := r.Run(ctx)
	// Run exits on ctx — returns ctx.Err() (DeadlineExceeded
	// for our timeout).
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run exit: got %v, want context.DeadlineExceeded", err)
	}
	if len(coord.submitCalls) != 2 {
		t.Errorf("submitted %d, want 2", len(coord.submitCalls))
	}
}

func TestRun_BackoffDoublesOnEmptyRounds(t *testing.T) {
	// Pin the schedule: each empty round must wait at least
	// PollInterval, and successive empties extend up to BackoffMax.
	// We thread a counter into Coord's ListReadyForBot to record
	// when each call lands; gaps between calls reveal the schedule.
	var (
		mu        sync.Mutex
		callTimes []time.Duration
	)
	start := time.Now()
	coord := &fakeCoordWithTiming{
		onList: func() {
			mu.Lock()
			callTimes = append(callTimes, time.Since(start))
			mu.Unlock()
		},
	}
	r := &Runner{
		Bot:          &Bot{Name: "test-bot", Model: "m"},
		Coord:        coord,
		LLM:          &StubBackend{RespondWith: "x"},
		PollInterval: 20 * time.Millisecond,
		BackoffMax:   200 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	mu.Lock()
	gaps := make([]time.Duration, 0, len(callTimes))
	for i := 1; i < len(callTimes); i++ {
		gaps = append(gaps, callTimes[i]-callTimes[i-1])
	}
	mu.Unlock()

	if len(gaps) < 3 {
		t.Fatalf("need at least 3 gaps to verify backoff; got %d (calls: %v)", len(gaps), callTimes)
	}
	// Schedule should be: gap0 ≈ PollInterval (20ms),
	// gap1 ≈ 2*PollInterval (40ms), gap2 ≈ 4*PollInterval (80ms)…
	// up to BackoffMax (200ms). Loose bounds — scheduling
	// noise is real.
	if gaps[0] < 15*time.Millisecond || gaps[0] > 60*time.Millisecond {
		t.Errorf("gap[0] (first backoff): %v, want ≈ 20ms", gaps[0])
	}
	if gaps[1] <= gaps[0] {
		t.Errorf("gap[1] (%v) should be larger than gap[0] (%v) — backoff not doubling", gaps[1], gaps[0])
	}
	if gaps[2] <= gaps[1] && gaps[1] < 150*time.Millisecond {
		t.Errorf("gap[2] (%v) should keep growing or be capped — got gap[1]=%v", gaps[2], gaps[1])
	}
}

// fakeCoordWithTiming is a bare-bones CoordClient that records
// call times for the backoff test. Always returns nothing
// (drives ErrNoWork forever).
type fakeCoordWithTiming struct {
	onList func()
}

func (f *fakeCoordWithTiming) ListReadyForBot(ctx context.Context, projectID int64, botUsername string) ([]TaskInfo, error) {
	f.onList()
	return nil, nil
}

func (f *fakeCoordWithTiming) Claim(ctx context.Context, taskID, botUsername, model string) error {
	return nil
}

func (f *fakeCoordWithTiming) Submit(ctx context.Context, task TaskInfo, response, model string) error {
	return nil
}

func (f *fakeCoordWithTiming) Release(ctx context.Context, taskID, botUsername string) error {
	return nil
}

func TestRun_ReleasesActiveClaimOnShutdown(t *testing.T) {
	// One task ready, but Submit blocks indefinitely. Cancel
	// the runner's ctx mid-submit; the deferred ReleaseActiveClaim
	// should release the in-flight claim before Run returns.
	block := make(chan struct{})
	coord := &fakeCoord{
		readyQueue:  [][]TaskInfo{{{ID: "1:1:r", Action: "review", Prompt: "p"}}},
		blockSubmit: block,
	}
	r := newRunner(coord, &StubBackend{RespondWith: "approve"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait until Submit is blocked (claim has been recorded).
	for {
		coord.mu.Lock()
		claimed := len(coord.claimCalls) > 0
		coord.mu.Unlock()
		if claimed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Trigger shutdown; unblock Submit so it returns ctx.Err.
	cancel()
	close(block)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't exit after ctx cancel")
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if len(coord.releaseCalls) != 1 || coord.releaseCalls[0].TaskID != "1:1:r" {
		t.Errorf("expected one Release for 1:1:r, got %v", coord.releaseCalls)
	}
}

func TestRun_NoReleaseAfterSuccessfulSubmit(t *testing.T) {
	// Submit succeeds, then ctx cancels with no claim active.
	// ReleaseActiveClaim should be a no-op (nothing to release).
	coord := &fakeCoord{
		readyQueue: [][]TaskInfo{
			{{ID: "1:1:a", Action: "review", Prompt: "p"}},
		},
	}
	r := newRunner(coord, &StubBackend{RespondWith: "approve"})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if len(coord.submitCalls) != 1 {
		t.Errorf("submit count: got %d, want 1", len(coord.submitCalls))
	}
	if len(coord.releaseCalls) != 0 {
		t.Errorf("expected no Release after successful submit, got %v", coord.releaseCalls)
	}
}

func TestRun_ReleasesAfterClaimButNoSubmit(t *testing.T) {
	// LLM fails — RunOnce returns an error mid-iteration with
	// the claim still tracked. Then ctx cancels. The deferred
	// release should fire even though the LLM step failed.
	coord := &fakeCoord{
		readyQueue: [][]TaskInfo{
			{{ID: "1:1:b", Action: "review", Prompt: "p"}},
		},
	}
	llm := &StubBackend{Err: errors.New("simulated LLM failure")}
	r := newRunner(coord, llm)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	coord.mu.Lock()
	defer coord.mu.Unlock()
	// At least one release should have fired for the claimed
	// task. The runner may have looped a couple times before
	// ctx expired (each iteration claims, LLM fails, no
	// release until shutdown — only the final iteration's
	// claim should be tracked when ctx hits).
	found := false
	for _, rel := range coord.releaseCalls {
		if rel.TaskID == "1:1:b" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a release for 1:1:b after LLM-error iteration, got %v", coord.releaseCalls)
	}
}

func TestComposeTaskPrompt_IncludesUserPrompt(t *testing.T) {
	task := &TaskInfo{
		ID:         "1:1:t",
		Action:     "answer",
		Prompt:     "main task prompt",
		UserPrompt: "extra hint from operator",
	}
	got := composeTaskPrompt(task)
	for _, want := range []string{"action: answer", "main task prompt", "extra hint from operator", "1:1:t"} {
		if !strings.Contains(got, want) {
			t.Errorf("composed prompt missing %q:\n%s", want, got)
		}
	}
}
