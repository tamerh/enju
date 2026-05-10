package bots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// fakeFC is the test double for the daemon's fatClient interface.
// Every method records its calls so tests can assert what the
// daemon did. Fields are public so each test composes the shape
// it needs without builder methods.
type fakeFC struct {
	mu sync.Mutex

	username string

	// Catalogues the daemon polls.
	projects []wire.Project
	runs     map[int64][]wire.Run
	ready    map[string][]map[string]interface{} // key: "pid:rid"

	// Recorded actions.
	claims    []service.ClaimParams
	submits   []service.SubmitParams
	releases  []string
	metaByID  map[string]*service.TaskMeta
	claimErr  error
	submitErr string

	// Inputs JSON returned by ClaimTask. Same key as ready map.
	claimInputs map[string][]byte

	// listReadyHook fires on every ListReadyTasks call so tests
	// can pin which (projectID, runID) pairs the daemon
	// requested. Optional — most tests don't care.
	listReadyHook func(pid, rid int64)

	// workspacePath is the path returned by
	// ResolveProjectWorkspace. Optional; default is a synthetic
	// path. Tests pinning the cwd-threading contract set it
	// explicitly.
	workspacePath string
	workspaceErr  error

	// lastResolveBotUsername captures the username arg the
	// daemon passed on its most recent ResolveBotWorkspace
	// call. Tests asserting "the daemon passes its own
	// citizen identity to the workspace resolver" check this
	// directly rather than reaching for log strings.
	lastResolveBotUsername string

	// resetCalls counts ResetBotCloneToCleanState invocations
	// keyed by projectID. Tests assert on this to pin "the
	// daemon resets the clone between iterations." resetErr,
	// when non-nil, is returned from every call — daemon
	// should log + continue rather than abort.
	resetCalls map[int64]int
	resetErr   error

	// checkoutTopicCalls captures the (projectID, branch) pairs
	// the daemon asked CheckoutTopicBranchTip for. Tests pin
	// the iter-2-revision flow by asserting this fires only on
	// re-claim.
	checkoutTopicCalls []checkoutTopicCall
	checkoutTopicErr   error

	// wipeWritesCalls captures the (projectID, paths) pairs the
	// daemon asked WipeDeclaredWrites for. Tests pin the
	// "iter-2 starts from a clean canvas in declared output
	// paths" contract by asserting this fires only on re-claim.
	wipeWritesCalls []wipeWritesCall
	wipeWritesErr   error

	// reviewFeedback, when set, is returned in
	// ClaimResult.ReviewFeedback. Tests pinning the iter-2
	// feedback-into-prompt contract set this.
	reviewFeedback []byte

	// fetchAllRefsCalls records FetchAllRefsForBot invocations
	// keyed by projectID. Tests pin the "daemon fetches before
	// claim" contract by asserting this fires every iteration.
	fetchAllRefsCalls []int64
	fetchAllRefsErr   error

	// markStartedCalls records MarkTaskStarted invocations.
	// Phase 8.2 contract: the daemon posts /started for every
	// claimed task right before the LLM call. Tests assert this
	// list mirrors the claimed taskIDs in order.
	markStartedCalls []string
	markStartedErr   error
}

type checkoutTopicCall struct {
	projectID int64
	branch    string
}

type wipeWritesCall struct {
	projectID int64
	paths     []string
}

func (f *fakeFC) Username() string { return f.username }
func (f *fakeFC) CommitAuthor(ctx context.Context) (string, string) {
	return f.username, f.username + "@enju.local"
}

func (f *fakeFC) ListProjects(ctx context.Context) ([]wire.Project, error) {
	return f.projects, nil
}

func (f *fakeFC) ListRuns(ctx context.Context, pid int64) ([]wire.Run, error) {
	return f.runs[pid], nil
}

func (f *fakeFC) ListReadyTasks(ctx context.Context, pid, rid int64) ([]map[string]interface{}, error) {
	if f.listReadyHook != nil {
		f.listReadyHook(pid, rid)
	}
	key := keyOf(pid, rid)
	return f.ready[key], nil
}

func (f *fakeFC) ClaimTask(ctx context.Context, p service.ClaimParams) (*service.ClaimResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, p)
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	return &service.ClaimResult{
		Inputs:         f.claimInputs[p.TaskID],
		ReviewFeedback: f.reviewFeedback,
	}, nil
}

func (f *fakeFC) ReleaseTask(ctx context.Context, taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, taskID)
	return nil
}

func (f *fakeFC) ReleaseAllMyOpenClaims(ctx context.Context) (*service.ReleaseAllMyOpenClaimsResponse, error) {
	return &service.ReleaseAllMyOpenClaimsResponse{ReleasedTaskIDs: []string{}, Count: 0}, nil
}

func (f *fakeFC) SweepStaleScratchAtStartup() (int, error) { return 0, nil }

func (f *fakeFC) FetchTaskMeta(ctx context.Context, taskID string) (*service.TaskMeta, error) {
	if m, ok := f.metaByID[taskID]; ok {
		return m, nil
	}
	return nil, errors.New("no meta for " + taskID)
}

// resetCalls counts ResetBotCloneToCleanState invocations
// per (caller-supplied) projectID. Tests that pin the
// "between iterations" call site assert against this.
func (f *fakeFC) ResetBotCloneToCleanState(ctx context.Context, projectID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resetCalls == nil {
		f.resetCalls = map[int64]int{}
	}
	f.resetCalls[projectID]++
	return f.resetErr
}

func (f *fakeFC) CheckoutTopicBranchTip(ctx context.Context, projectID int64, branch, baseBranch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkoutTopicCalls = append(f.checkoutTopicCalls, checkoutTopicCall{projectID, branch})
	_ = baseBranch
	return f.checkoutTopicErr
}

func (f *fakeFC) FetchAllRefsForBot(ctx context.Context, projectID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchAllRefsCalls = append(f.fetchAllRefsCalls, projectID)
	return f.fetchAllRefsErr
}

func (f *fakeFC) MarkTaskStarted(ctx context.Context, taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markStartedCalls = append(f.markStartedCalls, taskID)
	return f.markStartedErr
}

func (f *fakeFC) WipeDeclaredWrites(ctx context.Context, projectID int64, writes enjuYaml.WriteArtifacts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, 0, len(writes))
	for _, w := range writes {
		paths = append(paths, w.Path)
	}
	f.wipeWritesCalls = append(f.wipeWritesCalls, wipeWritesCall{projectID, paths})
	return f.wipeWritesErr
}

func (f *fakeFC) ResolveBotWorkspace(ctx context.Context, projectID int64, botUsername string) (string, error) {
	f.mu.Lock()
	f.lastResolveBotUsername = botUsername
	f.mu.Unlock()
	if f.workspaceErr != nil {
		return "", f.workspaceErr
	}
	if f.workspacePath != "" {
		return f.workspacePath, nil
	}
	// Default to a synthetic path that includes the bot
	// username so tests asserting per-bot isolation can spot
	// distinct resolves at a glance.
	return "/tmp/fake-workspace/project-" + itoa(projectID) + "-bot-" + botUsername, nil
}

func (f *fakeFC) SubmitTaskResult(ctx context.Context, p service.SubmitParams) *service.SubmitResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits = append(f.submits, p)
	if f.submitErr != "" {
		return &service.SubmitResult{ErrorMessage: f.submitErr}
	}
	return &service.SubmitResult{}
}

func keyOf(pid, rid int64) string {
	return "p" + itoa(pid) + ":r" + itoa(rid)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// scenarioBot returns a manifest entry that satisfies New's
// validation. Daemon tests don't exercise manifest validation;
// anything past New is what we're testing.
func scenarioBot() *Bot {
	return &Bot{Name: "test-bot", Model: "stub", Handler: "stub"}
}

// readyTask builds a wire-shape ready task entry. assignedTo
// drives whether the daemon thinks it owns the task.
func readyTask(id string, assignedTo []string) map[string]interface{} {
	out := map[string]interface{}{"id": id}
	if assignedTo != nil {
		ass := make([]interface{}, len(assignedTo))
		for i, s := range assignedTo {
			ass[i] = s
		}
		out["assign_to"] = ass
	}
	return out
}

// Pin the wire.Run.Seq vs wire.Run.ID confusion that produced
// the production "bot scoped to project 3 reached project 1"
// symptom. The coord's /tasks/ready treats ?run_id=N as the
// per-project RunSeq; passing wire.Run.ID (global int64) would
// miss the lookup coord-side and (pre-fix) fall through to the
// "all projects" fallback. This test fails loudly if a future
// refactor swaps the field accidentally.
func TestDaemon_FindWork_PassesRunSeqNotGlobalID(t *testing.T) {
	var seenRunIDs []int64
	fc := &fakeFC{
		username: "bot1",
		runs: map[int64][]wire.Run{
			1: {{ID: 99, Seq: 7}}, // ID and Seq deliberately different
		},
		ready: map[string][]map[string]interface{}{
			keyOf(1, 7): {readyTask("1:7:t", []string{"bot1"})},
		},
		metaByID: map[string]*service.TaskMeta{
			"1:7:t": {ID: "1:7:t", Action: "answer"},
		},
		listReadyHook: func(pid, rid int64) {
			seenRunIDs = append(seenRunIDs, rid)
		},
	}
	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "x"}, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seenRunIDs) != 1 || seenRunIDs[0] != 7 {
		t.Errorf("ListReadyTasks should be called with Seq=7, got %v (passing the global ID 99 was the production bug)", seenRunIDs)
	}
}

// Pin the production bug: pre-fix the daemon never threaded a
// workspace path to the handler, so claude -p inherited the
// daemon's cwd. The LLM then read/wrote the operator's
// filesystem instead of the bot's project clone, leaking
// the source tree (Bug 3 in the report) and clobbering the
// operator's checkout (Bug 4). Post-fix: the daemon resolves
// the project clone via FatClient and passes it as
// HandlerInput.Workspace; ClaudeHandler sets cmd.Dir from
// there.
func TestDaemon_ResolvesAndThreadsWorkspaceToHandler(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "")
	fc.workspacePath = "/home/test/projects/myproject/enju/bots/bot1/clone"
	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(stub.Inputs) != 1 {
		t.Fatalf("expected 1 handler invocation, got %d", len(stub.Inputs))
	}
	if got := stub.Inputs[0].Workspace; got != "/home/test/projects/myproject/enju/bots/bot1/clone" {
		t.Errorf("Workspace not threaded to handler: got %q (claude -p would inherit daemon cwd)", got)
	}
}

// TestDaemon_RunOnce_PassesBotUsernameToWorkspaceResolver pins
// the per-bot-clone contract: the daemon must thread its own
// citizen identity (Username from the FatClient) into
// ResolveBotWorkspace so the resolver can scope the clone to
// `<project>/enju/bots/<botUsername>/clone/`. Pre-fix the call
// took only projectID, all bots collided on a single shared
// clone, and two daemons on the same project on the same
// machine couldn't run in parallel.
func TestDaemon_RunOnce_PassesBotUsernameToWorkspaceResolver(t *testing.T) {
	fc := newFCWithTask("alice-bot", "answer", "")
	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fc.lastResolveBotUsername; got != "alice-bot" {
		t.Errorf("daemon should pass its own citizen username to ResolveBotWorkspace; got %q, want %q", got, "alice-bot")
	}
}

func TestDaemon_RunOnce_FailsWhenWorkspaceUnresolvable(t *testing.T) {
	// If the project clone can't be resolved (no remote_url,
	// network failure, etc.) the daemon must FAIL the iteration
	// rather than letting the handler inherit cwd. Pre-fix this
	// silently leaked the operator's filesystem to the LLM.
	fc := newFCWithTask("bot1", "answer", "")
	fc.workspaceErr = errors.New("project has no remote_url")
	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected workspace-resolution error to surface")
	}
	if !strings.Contains(err.Error(), "no remote_url") {
		t.Errorf("error should carry root cause: %v", err)
	}
	// Critical: handler must NOT have been invoked. Pre-fix,
	// the daemon would call the handler with empty Workspace
	// and claude -p would happily run with cwd=daemon.
	if stub.Calls != 0 {
		t.Errorf("handler should not run when workspace is unresolvable, got %d calls", stub.Calls)
	}
	if len(fc.submits) != 0 {
		t.Errorf("submit should not fire on workspace failure, got %d", len(fc.submits))
	}
}

// TestDaemon_Run_ExitsOnPermanentConfigError pins the
// permanent-vs-transient error split: if ResolveBotWorkspace
// returns service.ErrNoCloneSource (project has no remote_url
// AND no registered adopted path), the Run loop must surface
// the error and exit, not retry forever. Without this, a
// misconfigured project would spam log lines every poll cycle
// indefinitely. Transient errors still get the retry-with-
// backoff treatment — that's covered by other tests.
func TestDaemon_Run_ExitsOnPermanentConfigError(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "")
	// Wrap the sentinel exactly like service.ResolveBotWorkspace
	// does, so errors.Is in Run matches end-to-end.
	fc.workspaceErr = fmt.Errorf("%w: project 1", service.ErrNoCloneSource)
	stub := &StubHandler{Response: "ok"}
	d, err := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1, PollFloor: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	// 100ms ceiling — Run should exit immediately on the first
	// permanent-error iteration. If the loop keeps retrying,
	// this test hangs and fails the timeout, which is the right
	// signal.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runErr := d.Run(ctx)
	if runErr == nil {
		t.Fatal("Run should return an error on permanent config failure")
	}
	if !errors.Is(runErr, service.ErrNoCloneSource) {
		t.Errorf("Run should propagate ErrNoCloneSource so the operator sees the cause; got %v", runErr)
	}
	if ctx.Err() != nil {
		t.Errorf("Run should have exited BEFORE the context deadline (it kept retrying instead of bailing)")
	}
}

func TestDaemon_RunOnce_NoWork(t *testing.T) {
	fc := &fakeFC{username: "bot1"}
	d, err := New(Config{FC: fc, Handler: NewStubHandler(), Bot: scenarioBot(), ProjectID: 1})
	if err != nil {
		t.Fatal(err)
	}
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if worked {
		t.Error("expected worked=false on empty poll")
	}
	if len(fc.claims) != 0 {
		t.Errorf("expected no claims, got %d", len(fc.claims))
	}
}

func TestDaemon_RunOnce_AnswerAction(t *testing.T) {
	// Pin the architectural fix: a commit-bearing action
	// (answer) flows through the daemon end-to-end. Pre-Phase-7
	// code only handled review/vote because it reimplemented
	// the submit path; this test is the regression contract
	// against backsliding.
	fc := newFCWithTask("bot1", "answer", "the answer body")
	stub := &StubHandler{Response: "the answer body"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !worked {
		t.Fatal("expected worked=true on successful submit")
	}
	if len(fc.submits) != 1 {
		t.Fatalf("expected 1 submit, got %d", len(fc.submits))
	}
	got := fc.submits[0]
	if got.Content != "the answer body" {
		t.Errorf("Content: got %q, want %q", got.Content, "the answer body")
	}
	if got.Decision != "" || got.Option != "" {
		t.Errorf("answer should not set Decision/Option: %+v", got)
	}
}

// TestDaemon_RunOnce_MarksTaskStartedBeforeHandler pins Phase
// 8.2's CLAIMED → RUNNING signal: the daemon must POST
// /tasks/:id/started for every claimed task before invoking
// the handler. Without this, a bot stuck in pre-LLM setup
// (workspace resolve, claim race, etc.) is indistinguishable
// from one mid-LLM-call. The fake records each call so we can
// also assert it's the same task we claimed.
func TestDaemon_RunOnce_MarksTaskStartedBeforeHandler(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "ok")
	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(fc.markStartedCalls) != 1 {
		t.Fatalf("expected 1 MarkTaskStarted call, got %d", len(fc.markStartedCalls))
	}
	if got, want := fc.markStartedCalls[0], "1:1:t"; got != want {
		t.Errorf("MarkTaskStarted called for %q, want %q", got, want)
	}
	// One submit confirms the iteration completed past the
	// MarkTaskStarted gate. A swallowed error in the handler
	// path would have left submits empty here.
	if len(fc.submits) != 1 {
		t.Fatalf("expected 1 submit, got %d", len(fc.submits))
	}
}

// TestDaemon_RunOnce_MarkStartedFailureDoesNotBlockHandler
// pins the best-effort contract: a /started POST that errors
// (e.g. coord transient 5xx, or a state-guard rejection on a
// retry resume) MUST NOT abort the iteration. The daemon logs
// and proceeds — the work itself is independent of this
// observability hook.
func TestDaemon_RunOnce_MarkStartedFailureDoesNotBlockHandler(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "ok")
	fc.markStartedErr = errors.New("coord 503 transient")
	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !worked {
		t.Fatal("expected worked=true; mark-started error must not abort")
	}
	if len(fc.submits) != 1 {
		t.Errorf("expected 1 submit despite mark-started error, got %d", len(fc.submits))
	}
}

// Pin the data-loss fix: when the task declares writes_artifacts,
// the daemon must read each tracked path off disk and stage it
// into params.Artifacts, and stat each untracked path into
// params.UntrackedArtifacts. Without the read step, the bot
// reports success but the file silently drops out of the commit.
func TestDaemon_RunOnce_PopulatesArtifactsFromWritesArtifacts(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "out", "tracked.md"), []byte("tracked body"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.bam"), []byte("untracked body"), 0644); err != nil {
		t.Fatal(err)
	}

	fc := newFCWithTask("bot1", "answer", "")
	fc.workspacePath = ws
	fc.metaByID["1:1:t"].WritesArtifacts = enjuYaml.WriteArtifacts{
		{Path: "out/tracked.md", Track: true},
		{Path: "big.bam", Track: false},
	}

	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "done"}, Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !worked {
		t.Fatal("expected worked=true")
	}
	if len(fc.submits) != 1 {
		t.Fatalf("expected 1 submit, got %d", len(fc.submits))
	}
	got := fc.submits[0]
	if got.Artifacts["out/tracked.md"] != "tracked body" {
		t.Errorf("Artifacts[out/tracked.md] = %q, want %q", got.Artifacts["out/tracked.md"], "tracked body")
	}
	if _, ok := got.Artifacts["big.bam"]; ok {
		t.Errorf("untracked path big.bam must NOT appear in Artifacts (would be committed): got %+v", got.Artifacts)
	}
	if len(got.UntrackedArtifacts) != 1 || got.UntrackedArtifacts[0] != "big.bam" {
		t.Errorf("UntrackedArtifacts = %v, want [big.bam]", got.UntrackedArtifacts)
	}
}

// TestDaemon_RunOnce_ResetsBotCloneBeforeHandler pins the
// fix for the cross-iteration residue bug: the daemon must
// call ResetBotCloneToCleanState between ClaimTask and the
// handler invocation, so leftover untracked files / dirty
// tracked-file edits from a previous task can't trip the
// next task's CheckoutBranchFrom. The single-iteration
// failure mode this guards against is task N's residue
// disturbing task N+1; per-iteration the call shows up as
// exactly one reset on the project's id.
func TestDaemon_RunOnce_ResetsBotCloneBeforeHandler(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "")
	stub := &StubHandler{Response: "done"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := fc.resetCalls[1]; got != 1 {
		t.Errorf("expected exactly one reset for project 1; got %d", got)
	}
}

// TestDaemon_RunOnce_ContinuesWhenResetFails pins the
// best-effort policy: a reset failure logs but doesn't
// abort the iteration. The handler still runs (against
// possibly-dirty state) and any actual collision surfaces
// at submit time where it's already handled.
func TestDaemon_RunOnce_ContinuesWhenResetFails(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "")
	fc.resetErr = errors.New("disk full")
	stub := &StubHandler{Response: "done"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce should swallow reset errors: %v", err)
	}
	if !worked {
		t.Error("worked should be true — handler still ran despite reset failure")
	}
	if len(fc.submits) != 1 {
		t.Errorf("submit should still happen: got %d", len(fc.submits))
	}
}

// Pin pattern expansion: when the manifest declares
// `src/api/` (directory) and the bot writes 3 files inside,
// all 3 should land in params.Artifacts as tracked content.
// Bots that decompose a "build the api package" task into
// many free-form filenames need this — without directory
// declarations every output filename has to be predicted up
// front.
func TestDaemon_RunOnce_DirectoryDeclarationCoversAllFilesInside(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src", "api"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server.go", "middleware.go", "handlers/users.go"} {
		full := filepath.Join(ws, "src", "api", name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("// "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fc := newFCWithTask("bot1", "answer", "")
	fc.workspacePath = ws
	fc.metaByID["1:1:t"].WritesArtifacts = enjuYaml.WriteArtifacts{
		{Path: "src/api/", Track: true},
	}

	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "done"}, Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !worked {
		t.Fatal("expected worked=true")
	}
	got := fc.submits[0].Artifacts
	want := map[string]string{
		"src/api/server.go":          "// server.go",
		"src/api/middleware.go":      "// middleware.go",
		"src/api/handlers/users.go":  "// handlers/users.go",
	}
	if len(got) != len(want) {
		t.Fatalf("Artifacts size: got %d, want %d (%v)", len(got), len(want), got)
	}
	for k, wantV := range want {
		if got[k] != wantV {
			t.Errorf("Artifacts[%q] = %q, want %q", k, got[k], wantV)
		}
	}
}

// Pin glob expansion: `src/*.go` covers every shallow .go file
// the bot writes. Combined with TestDaemon_RunOnce_DirectoryDecl
// this exercises both pattern forms at the daemon level.
func TestDaemon_RunOnce_GlobDeclarationCoversMatchingFiles(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.go", "b.go", "c.go", "notes.md"} {
		if err := os.WriteFile(filepath.Join(ws, "src", name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fc := newFCWithTask("bot1", "answer", "")
	fc.workspacePath = ws
	fc.metaByID["1:1:t"].WritesArtifacts = enjuYaml.WriteArtifacts{
		{Path: "src/*.go", Track: true},
	}
	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "done"}, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := fc.submits[0].Artifacts
	if _, ok := got["src/a.go"]; !ok {
		t.Errorf("src/a.go missing from Artifacts: %v", got)
	}
	if _, ok := got["src/notes.md"]; ok {
		t.Errorf("src/notes.md should NOT match *.go glob: %v", got)
	}
}

// Pin optional behavior: a declared optional path that the bot
// didn't write doesn't fail the iteration. The submit goes
// through with whatever WAS written.
func TestDaemon_RunOnce_OptionalMissingArtifactSucceeds(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "out.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	fc := newFCWithTask("bot1", "answer", "")
	fc.workspacePath = ws
	fc.metaByID["1:1:t"].WritesArtifacts = enjuYaml.WriteArtifacts{
		{Path: "out.txt", Track: true},
		{Path: "go.sum", Track: true, Optional: true},
	}
	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "done"}, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should not fail with optional missing: %v", err)
	}
	if len(fc.submits) != 1 {
		t.Fatalf("expected 1 submit, got %d", len(fc.submits))
	}
	if _, ok := fc.submits[0].Artifacts["go.sum"]; ok {
		t.Error("optional missing path should not appear in Artifacts")
	}
	if _, ok := fc.submits[0].Artifacts["out.txt"]; !ok {
		t.Error("required existing path should appear in Artifacts")
	}
}

// Pin the fail-loud contract: a declared artifact missing on disk
// fails the iteration so the bot's "I'm done" doesn't silently
// land. Pre-fix this scenario submitted result.md only and the
// task transitioned to ACCEPTED with the missing file unrecorded.
func TestDaemon_RunOnce_FailsWhenDeclaredArtifactMissing(t *testing.T) {
	ws := t.TempDir() // intentionally empty — no out/missing.md

	fc := newFCWithTask("bot1", "answer", "")
	fc.workspacePath = ws
	fc.metaByID["1:1:t"].WritesArtifacts = enjuYaml.WriteArtifacts{
		{Path: "out/missing.md", Track: true},
	}

	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "done"}, Bot: scenarioBot(), ProjectID: 1})
	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error from missing artifact, got nil")
	}
	if !strings.Contains(err.Error(), "out/missing.md") {
		t.Errorf("error should name the missing path; got %q", err.Error())
	}
	// The submit MUST NOT happen — if it did, the bug would be back:
	// task accepted with phantom artifact_written list, file gone.
	if len(fc.submits) != 0 {
		t.Errorf("submit must NOT be called when artifacts are missing; got %d submits", len(fc.submits))
	}
}

func TestDaemon_RunOnce_ReviewAction_SplitsDecisionFromBody(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	stub := &StubHandler{Response: "APPROVE\nLooks good. Tests pass."}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !worked {
		t.Fatal("expected worked=true")
	}
	got := fc.submits[0]
	// Decision is normalized to canonical lowercase form
	// (matches types.ReviewDecisionApprove). The coord rejects
	// any other casing.
	if got.Decision != "approve" {
		t.Errorf("Decision: got %q, want approve", got.Decision)
	}
	if !strings.Contains(got.Content, "Looks good") {
		t.Errorf("Content should carry the rationale, got %q", got.Content)
	}
}

// Regression for the production false-negative report: a real
// reviewer-bot wrote a thorough review that ended with both an
// inline "approve" line AND a final "DECISION: approve" marker.
// Pre-fix the parser checked only the first non-empty line
// ("Reviewing the breakdown against the spec.") and fell back
// to request_changes despite the LLM clearly intending approve.
// Post-fix: DECISION: marker (bottom-up) wins over first-line.
func TestDaemon_RunOnce_ReviewAction_DecisionMarkerWinsOverPreamble(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	body := "Reviewing the breakdown against the spec.\n" +
		"\n" +
		"No-peeking — only spec files cited. Module names are\n" +
		"spec-derived, not imported terminology.\n" +
		"\n" +
		"approve\n" +
		"\n" +
		"The breakdown maps every spec entity to a clearly-owned module.\n" +
		"\n" +
		"DECISION: approve"
	stub := &StubHandler{Response: body}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := fc.submits[0]
	if got.Decision != "approve" {
		t.Errorf("Decision: got %q, want approve (DECISION: marker should win, not the first-line preamble)", got.Decision)
	}
	if !strings.Contains(got.Content, "Reviewing the breakdown") {
		t.Errorf("rationale should carry the full response: %q", got.Content)
	}
}

// Last-line bare keyword: LLM concludes with "I conclude X.\n\napprove".
func TestDaemon_RunOnce_ReviewAction_LastLineBareKeyword(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	body := "After reviewing, the implementation matches the spec.\n" +
		"Tests cover the happy path. Nothing else to flag.\n" +
		"\n" +
		"approve"
	stub := &StubHandler{Response: body}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fc.submits[0].Decision; got != "approve" {
		t.Errorf("Decision: got %q, want approve (last-line bare keyword)", got)
	}
}

// DECISION: marker beats a stale earlier "DECISION: reject" the
// LLM mentioned while deliberating. Bottom-up scan = final word
// wins.
func TestDaemon_RunOnce_ReviewAction_DecisionMarkerBottomUp(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	body := "I considered DECISION: reject early on but...\n" +
		"after re-reading the spec the design is sound.\n" +
		"\n" +
		"DECISION: approve"
	stub := &StubHandler{Response: body}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fc.submits[0].Decision; got != "approve" {
		t.Errorf("bottom-up scan should pick latest DECISION:; got %q", got)
	}
}

// True-fallback case: NO recognizable verdict anywhere. LLM was
// genuinely confused. Daemon must fall back to request_changes
// rather than ship garbage to the coord.
func TestDaemon_RunOnce_ReviewAction_NoVerdictFallsBack(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	body := "I've now read goal.md, all of spec/layer1 (00-05).\n" +
		"Evaluating systematically — but I'm not sure what to make of this."
	stub := &StubHandler{Response: body}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fc.submits[0]
	if got.Decision != "request_changes" {
		t.Errorf("Decision: got %q, want request_changes (safe fallback for response with no verdict anywhere)", got.Decision)
	}
	if !strings.Contains(got.Content, "I've now read goal.md") {
		t.Errorf("rationale should carry the LLM's full response: %q", got.Content)
	}
}

// Structured-output path: a Handler that pre-fills
// HandlerOutput.Decision skips daemon-side text parsing entirely.
// This is the architectural escape hatch for users plugging in
// their own Handler with a custom response shape (JSON-mode
// LLMs, rule-based handlers, prompt conventions other than
// DECISION:). The daemon trusts the handler's structured
// output, only normalizing casing.
func TestDaemon_RunOnce_ReviewAction_StructuredHandlerSkipsTextParse(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	// Response text is deliberately unparseable by daemon
	// heuristics — no DECISION:, no bare keyword on first or
	// last line. Without the structured-output path, this
	// would hit the request_changes fallback. With it, the
	// handler's pre-filled Decision wins.
	stub := &StubHandler{
		Response: "Here are my thoughts on the design, in narrative form. " +
			"The module decomposition feels right and the tests look thorough.",
	}
	stub.PrefillDecision = "APPROVE" // weird casing on purpose
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fc.submits[0]
	if got.Decision != "approve" {
		t.Errorf("Decision: got %q, want approve (structured output normalized)", got.Decision)
	}
	if !strings.Contains(got.Content, "narrative form") {
		t.Errorf("rationale should be the full Response: %q", got.Content)
	}
}

func TestDaemon_RunOnce_ReviewAction_StructuredButInvalidFallsThroughToTextParse(t *testing.T) {
	// Defense in depth: if a custom handler sets Decision to
	// something that isn't a known verdict (typo, bug, lying
	// to itself), the daemon falls through to text parsing
	// rather than shipping garbage to the coord. The text
	// here HAS a valid DECISION: marker so we can prove the
	// fallthrough happened.
	fc := newFCWithTask("bot1", "review", "")
	stub := &StubHandler{Response: "DECISION: approve"}
	stub.PrefillDecision = "definitely-not-a-verdict"
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fc.submits[0].Decision; got != "approve" {
		t.Errorf("Decision: got %q, want approve (text parse should have rescued the bad structured output)", got)
	}
}

// Same idea for vote: structured Option wins over text.
func TestDaemon_RunOnce_VoteAction_StructuredHandlerSkipsTextParse(t *testing.T) {
	fc := newFCWithTask("bot1", "vote", "")
	fc.metaByID["1:1:t"].VoteOptionsJSON = `["yes","no","abstain"]`
	stub := &StubHandler{Response: "I weighed the options and chose carefully."}
	stub.PrefillOption = "abstain"
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fc.submits[0].Option; got != "abstain" {
		t.Errorf("Option: got %q, want abstain (structured output)", got)
	}
}

// Regression for the production false-negative on run #6 of
// enju-layer1-rebuild: the LLM finished the review with
//   **DECISION: approve**
// (markdown-bolded). Pre-fix the parser only matched literal
// `DECISION:` and bare keywords, ignoring markdown wrapping;
// the bolded line wasn't seen, so the daemon fell back to
// request_changes despite the LLM clearly approving. Post-fix
// the line-cleaning pass strips `*` characters globally before
// keyword matching, so the marker finds its way through.
func TestDaemon_RunOnce_ReviewAction_MarkdownBoldedDecision(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	body := "Reviewing the breakdown against the spec.\n\n" +
		"The decomposition feels right and the tests look thorough.\n\n" +
		"**DECISION: approve**"
	stub := &StubHandler{Response: body}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := fc.submits[0].Decision; got != "approve" {
		t.Errorf("Decision: got %q, want approve (markdown bolding around DECISION: must not block recognition)", got)
	}
}

func TestDaemon_RunOnce_ReviewAction_TolerantOfMarkdownDecorations(t *testing.T) {
	// Catalog of LLM-emitted decorations that should all
	// recognize the verdict cleanly. Each row: a real-world
	// shape we've seen or might plausibly see, plus the
	// expected canonical verdict.
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bold-on-decision-marker", "**DECISION: approve**", "approve"},
		{"bold-on-bare-keyword", "**approve**", "approve"},
		{"underscore-emphasis", "_approve_", "approve"},
		{"inline-code", "`approve`", "approve"},
		{"strikethrough", "~~approve~~", "approve"},
		{"atx-header-level1", "# Approve", "approve"},
		{"atx-header-level3", "### approve", "approve"},
		{"blockquote-marker", "> DECISION: approve", "approve"},
		{"nested-blockquote-emphasis", "> > **approve**", "approve"},
		{"list-bullet-dash", "- approve", "approve"},
		{"list-bullet-asterisk", "* approve", "approve"},
		{"deep-indent", "        DECISION: approve", "approve"},
		{"bold-marker-with-trailing-block", "**DECISION: request_changes**\n\nNotes:\n- improve docs", "request_changes"},
		{"keyword-buried-after-prose", "After thorough review:\n\n# approve", "approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFCWithTask("bot1", "review", "")
			stub := &StubHandler{Response: tc.body}
			d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
			if _, err := d.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := fc.submits[0].Decision; got != tc.want {
				t.Errorf("body %q: Decision got %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// DECISION: marker on a "trailing block" — verdict isn't
// literally last, e.g. LLM appends "Notes:" after. Bottom-up
// scan should still find the marker.
func TestDaemon_RunOnce_ReviewAction_DecisionMarkerNotLastLine(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	body := "DECISION: request_changes\n" +
		"\n" +
		"Notes:\n" +
		"- consider extracting the validator into its own module\n" +
		"- the integration test is a bit thin"
	stub := &StubHandler{Response: body}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fc.submits[0].Decision; got != "request_changes" {
		t.Errorf("Decision: got %q, want request_changes (DECISION: line is at top, before a trailing Notes block)", got)
	}
}

func TestDaemon_RunOnce_ReviewAction_AcceptsCommonVerdictSpellings(t *testing.T) {
	cases := []struct {
		response string
		want     string
	}{
		{"APPROVE", "approve"},
		{"approve.", "approve"},
		{"Approve:", "approve"},
		{"REQUEST_CHANGES\nfix typos", "request_changes"},
		{"request changes\nlooks rough", "request_changes"}, // space form
		{"reject", "reject"},
		{"Comment\nFYI", "comment"},
	}
	for _, tc := range cases {
		t.Run(tc.response, func(t *testing.T) {
			fc := newFCWithTask("bot1", "review", "")
			stub := &StubHandler{Response: tc.response}
			d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
			if _, err := d.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := fc.submits[0].Decision; got != tc.want {
				t.Errorf("Decision: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDaemon_RunOnce_VoteAction_SplitsOptionFromBody(t *testing.T) {
	fc := newFCWithTask("bot1", "vote", "")
	// Declare options on the meta so vote parsing has a set to
	// validate against.
	fc.metaByID["1:1:t"].VoteOptionsJSON = `["option-a","option-b","option-c"]`
	stub := &StubHandler{Response: "option-b\nbecause B is correct"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fc.submits[0]
	if got.Option != "option-b" {
		t.Errorf("Option: got %q, want option-b", got.Option)
	}
	if !strings.Contains(got.Content, "because B is correct") {
		t.Errorf("Content rationale missing: %q", got.Content)
	}
}

func TestDaemon_RunOnce_VoteAction_FindsOptionInProse(t *testing.T) {
	// LLM rambles before naming the choice. Daemon scans the
	// whole response for declared options; exactly one match
	// wins.
	fc := newFCWithTask("bot1", "vote", "")
	fc.metaByID["1:1:t"].VoteOptionsJSON = `[{"id":"yes"},{"id":"no"}]`
	stub := &StubHandler{Response: "After weighing the trade-offs I think yes is the right call here."}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fc.submits[0].Option; got != "yes" {
		t.Errorf("Option: got %q, want yes", got)
	}
}

func TestDaemon_RunOnce_VoteAction_AmbiguousResponseFails(t *testing.T) {
	// Multiple options mentioned in the prose — no safe pick,
	// so the iteration errors. No phantom submit is sent.
	fc := newFCWithTask("bot1", "vote", "")
	fc.metaByID["1:1:t"].VoteOptionsJSON = `["yes","no"]`
	stub := &StubHandler{Response: "Could be yes or no, hard to say."}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error on ambiguous vote response")
	}
	if !strings.Contains(err.Error(), "multiple options") {
		t.Errorf("error should name the failure mode: %v", err)
	}
	if len(fc.submits) != 0 {
		t.Errorf("no submit should fire on parse failure, got %d", len(fc.submits))
	}
}

func TestDaemon_RunOnce_VoteAction_NoMatchFails(t *testing.T) {
	fc := newFCWithTask("bot1", "vote", "")
	fc.metaByID["1:1:t"].VoteOptionsJSON = `["yes","no"]`
	stub := &StubHandler{Response: "Maybe later."}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error when response names no declared option")
	}
	if !strings.Contains(err.Error(), "does not name") {
		t.Errorf("error should name the failure mode: %v", err)
	}
}

func TestDaemon_RunOnce_ContributeAction(t *testing.T) {
	fc := newFCWithTask("bot1", "contribute", "")
	stub := &StubHandler{Response: "contributed work"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fc.submits) != 1 {
		t.Fatalf("expected 1 submit, got %d", len(fc.submits))
	}
	if fc.submits[0].Content != "contributed work" {
		t.Errorf("Content not threaded through: %q", fc.submits[0].Content)
	}
}

func TestDaemon_RunOnce_ComputeAction(t *testing.T) {
	// Compute actions are produced by scripts, not LLMs, but
	// the daemon doesn't care — it hands the task to the
	// Handler and submits whatever comes back. (A future
	// ShellHandler will produce real script output here; the
	// stub stands in for that surface.)
	fc := newFCWithTask("bot1", "compute", "")
	stub := &StubHandler{Response: "compute output"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fc.submits) != 1 || fc.submits[0].Content != "compute output" {
		t.Errorf("compute submit not as expected: %+v", fc.submits)
	}
}

func TestDaemon_RunOnce_SkipsTasksNotAssignedToUs(t *testing.T) {
	fc := &fakeFC{
		username: "bot1",
		runs:     map[int64][]wire.Run{1: {{ID: 99, Seq: 10}}},
		ready: map[string][]map[string]interface{}{
			keyOf(1, 10): {readyTask("1:1:other-task", []string{"someone-else"})},
		},
	}
	d, _ := New(Config{FC: fc, Handler: NewStubHandler(), Bot: scenarioBot(), ProjectID: 1})
	worked, _ := d.RunOnce(context.Background())
	if worked {
		t.Error("expected to skip task assigned to someone else")
	}
	if len(fc.claims) != 0 {
		t.Errorf("expected no claim attempts, got %d", len(fc.claims))
	}
}

func TestDaemon_RunOnce_OpenAssignTo(t *testing.T) {
	// assign_to absent / nil = open task (anyone can claim).
	// Daemon should pick it up.
	fc := &fakeFC{
		username: "bot1",
		runs:     map[int64][]wire.Run{1: {{ID: 99, Seq: 10}}},
		ready: map[string][]map[string]interface{}{
			keyOf(1, 10): {readyTask("1:1:open-task", nil)},
		},
		metaByID: map[string]*service.TaskMeta{
			"1:1:open-task": {ID: "1:1:open-task", Action: "answer"},
		},
	}
	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "x"}, Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Error("expected open task to be claimed")
	}
}

func TestDaemon_RunOnce_AlreadyClaimedRace_NotAnError(t *testing.T) {
	fc := newFCWithTask("bot1", "review", "")
	fc.claimErr = errors.New("task already claimed by someone else")
	d, _ := New(Config{FC: fc, Handler: NewStubHandler(), Bot: scenarioBot(), ProjectID: 1})
	worked, err := d.RunOnce(context.Background())
	if err != nil {
		t.Errorf("claim race should not surface as iteration error: %v", err)
	}
	if worked {
		t.Error("worked should be false on lost race")
	}
	// No submit attempted — we never got the task.
	if len(fc.submits) != 0 {
		t.Errorf("submit should not be attempted on lost race")
	}
}

func TestDaemon_ResolvedPromptThreadedToHandler(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "")
	fc.claimInputs = map[string][]byte{
		"1:1:t": []byte(`{"resolved_prompt":"Render me, please."}`),
	}
	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1, SystemPrompt: "you are a bot"})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(stub.Inputs) != 1 {
		t.Fatalf("expected 1 handler input, got %d", len(stub.Inputs))
	}
	got := stub.Inputs[0]
	if got.Prompt != "Render me, please." {
		t.Errorf("resolved_prompt should reach handler, got %q", got.Prompt)
	}
	if got.SystemPrompt != "you are a bot" {
		t.Errorf("system prompt not threaded through: %q", got.SystemPrompt)
	}
	if got.Action != "answer" {
		t.Errorf("action: got %q", got.Action)
	}
}

func TestDaemon_ResolvedPromptFallsBackToTemplate(t *testing.T) {
	// When inputs is empty / malformed, the daemon falls back
	// to TaskMeta.Prompt rather than submitting against an
	// empty brief.
	fc := newFCWithTask("bot1", "answer", "")
	fc.metaByID["1:1:t"].Prompt = "fallback template"
	fc.claimInputs = nil // no resolved prompt available
	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stub.Inputs[0].Prompt != "fallback template" {
		t.Errorf("expected fallback to template, got %q", stub.Inputs[0].Prompt)
	}
}

func TestDaemon_ReleaseActiveClaim_OnShutdown(t *testing.T) {
	// Mid-iteration shutdown: claim succeeds, handler hangs
	// past ctx deadline, daemon should release the claim.
	fc := newFCWithTask("bot1", "review", "")
	hang := &hangingHandler{entered: make(chan struct{}), released: make(chan struct{})}
	d, _ := New(Config{FC: fc, Handler: hang, Bot: scenarioBot(), ProjectID: 1})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		_, _ = d.RunOnce(ctx)
		close(hang.released)
	}()

	// Wait for handler to be invoked (i.e. claim done).
	select {
	case <-hang.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never entered")
	}
	cancel()
	select {
	case <-hang.released:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon didn't return after cancel")
	}
	if len(fc.releases) != 1 {
		t.Errorf("expected 1 release on shutdown, got %d", len(fc.releases))
	}
}

func TestDaemon_RunOnce_SubmitFailureSurfacesAsError(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "")
	fc.submitErr = "coord rejected: bad commit"
	stub := &StubHandler{Response: "x"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected submit failure to surface")
	}
	if !strings.Contains(err.Error(), "coord rejected") {
		t.Errorf("error should carry coord message, got: %v", err)
	}
}

// hangingHandler blocks ProcessTask until ctx is cancelled.
// Used to test the "release active claim on shutdown" path.
// Caller pre-creates entered + released channels so the test
// can read entered without a race against the goroutine that
// invokes ProcessTask.
type hangingHandler struct {
	entered  chan struct{}
	once     sync.Once
	released chan struct{}
}

func (h *hangingHandler) ProcessTask(ctx context.Context, in HandlerInput) (HandlerOutput, error) {
	h.once.Do(func() { close(h.entered) })
	<-ctx.Done()
	return HandlerOutput{}, ctx.Err()
}

// newFCWithTask builds a fakeFC pre-populated with one ready task
// assigned to username, with TaskMeta and a sane default project /
// run shape. action drives the action field on the meta. content
// goes into the Prompt as the rendered text.
func newFCWithTask(username, action, content string) *fakeFC {
	return &fakeFC{
		username: username,
		runs:     map[int64][]wire.Run{1: {{ID: 99, Seq: 10}}},
		ready: map[string][]map[string]interface{}{
			keyOf(1, 10): {readyTask("1:1:t", []string{username})},
		},
		metaByID: map[string]*service.TaskMeta{
			"1:1:t": {ID: "1:1:t", Action: action, Prompt: content, ProjectID: 1, RunSeq: 1},
		},
	}
}

// TestDaemon_Iter1_NoTopicCheckoutOrWipe pins the iter-1
// (first claim) contract: topic-branch checkout and writes-wipe
// must NOT fire on a fresh task. The pre-claim pull leaves HEAD
// on the run branch, the existing reset cleans residue, and the
// handler runs against a clean run-branch tree. Pre-fix had no
// distinction between iter-1 and iter-2; this test guards
// against accidentally always-firing the revision-only steps.
func TestDaemon_Iter1_NoTopicCheckoutOrWipe(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "do work")
	// IterSeq=1 (default zero is 0; daemon treats > 1 as
	// revision, so 0/1 both mean first claim — set explicitly
	// for readability).
	fc.metaByID["1:1:t"].IterSeq = 1
	fc.metaByID["1:1:t"].IterationBranch = "run-1/t/iter-1"

	d, _ := New(Config{FC: fc, Handler: &StubHandler{Response: "ok"}, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(fc.checkoutTopicCalls) != 0 {
		t.Errorf("iter-1: CheckoutTopicBranchTip should NOT fire (got %d calls: %+v)",
			len(fc.checkoutTopicCalls), fc.checkoutTopicCalls)
	}
	if len(fc.wipeWritesCalls) != 0 {
		t.Errorf("iter-1: WipeDeclaredWrites should NOT fire (got %d calls: %+v)",
			len(fc.wipeWritesCalls), fc.wipeWritesCalls)
	}
}

// TestDaemon_Iter2_ChecksOutTopicWipesAndPrependsFeedback pins
// the revision-loop fix: on a re-claim after request_changes
// (IterSeq > 1, topic branch present, ReviewFeedback non-empty),
// the daemon must:
//
//  1. Check out the topic branch tip BEFORE the handler so the
//     LLM starts on iter-1's tree (where reviewer feedback
//     applies), not on the run-branch tip.
//  2. Wipe declared writes_artifacts paths so iter-2's commit
//     carries iter-2's content only — not a union of both
//     iterations' files when LLM non-determinism produces
//     different filenames.
//  3. Prepend the reviewer feedback to the prompt so the LLM
//     understands what to change. Without this, iter-2's prompt
//     equals iter-1's and the "revision" is just stochastic
//     sampling on identical input.
//
// Pre-fix all three were missing. The production symptom was
// iter-2's submit blowing up with "worktree contains unstaged
// changes" because of the state desync, AND silent revision
// noise because the LLM never saw the reviewer's feedback.
func TestDaemon_Iter2_ChecksOutTopicWipesAndPrependsFeedback(t *testing.T) {
	fc := newFCWithTask("bot1", "answer", "Implement the foo module.")
	fc.metaByID["1:1:t"].IterSeq = 2
	fc.metaByID["1:1:t"].IterationBranch = "run-1/t/iter-1"
	fc.metaByID["1:1:t"].WritesArtifacts = enjuYaml.WriteArtifacts{
		// Optional: true — the stub handler doesn't actually
		// write these files; we're only pinning the daemon's
		// pre-handler steps (checkout/wipe/prompt-prepend), not
		// the post-handler submission contract.
		{Path: "src/foo/entities.go", Track: true, Optional: true},
		{Path: "src/foo/errors.go", Track: true, Optional: true},
	}
	// claimInputs is keyed by task id (the daemon uses task id
	// as the lookup key in ClaimTask). The resolved_prompt is
	// what extractResolvedPrompt pulls out for the handler.
	fc.claimInputs = map[string][]byte{
		"1:1:t": []byte(`{"resolved_prompt":"Implement the foo module."}`),
	}
	// reviewFeedback is what the fakeFC's ClaimTask returns in
	// ClaimResult.ReviewFeedback; the daemon prepends it to the
	// prompt on iter > 1.
	fc.reviewFeedback = []byte("Please rename entities.go to entity.go (singular).")

	stub := &StubHandler{Response: "ok"}
	d, _ := New(Config{FC: fc, Handler: stub, Bot: scenarioBot(), ProjectID: 1})
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Topic checkout fired with the right branch.
	if len(fc.checkoutTopicCalls) != 1 {
		t.Fatalf("iter-2: expected 1 CheckoutTopicBranchTip call, got %d: %+v",
			len(fc.checkoutTopicCalls), fc.checkoutTopicCalls)
	}
	if got := fc.checkoutTopicCalls[0].branch; got != "run-1/t/iter-1" {
		t.Errorf("checkout branch = %q, want run-1/t/iter-1", got)
	}

	// Wipe fired with the declared writes_artifacts paths.
	if len(fc.wipeWritesCalls) != 1 {
		t.Fatalf("iter-2: expected 1 WipeDeclaredWrites call, got %d: %+v",
			len(fc.wipeWritesCalls), fc.wipeWritesCalls)
	}
	gotPaths := fc.wipeWritesCalls[0].paths
	if len(gotPaths) != 2 || gotPaths[0] != "src/foo/entities.go" || gotPaths[1] != "src/foo/errors.go" {
		t.Errorf("wipe paths = %v, want [src/foo/entities.go src/foo/errors.go]", gotPaths)
	}

	// Handler received the feedback both as a separate field AND
	// prepended in the prompt — the prompt prepend is what the
	// LLM actually sees for free; the field is for handlers that
	// want structured access.
	if len(stub.Inputs) != 1 {
		t.Fatalf("expected 1 handler invocation, got %d", len(stub.Inputs))
	}
	in := stub.Inputs[0]
	if in.ReviewFeedback != "Please rename entities.go to entity.go (singular)." {
		t.Errorf("HandlerInput.ReviewFeedback = %q, want the reviewer's note", in.ReviewFeedback)
	}
	if !strings.Contains(in.Prompt, "Reviewer feedback from previous iteration") {
		t.Errorf("prompt missing reviewer feedback header:\n%s", in.Prompt)
	}
	if !strings.Contains(in.Prompt, "rename entities.go") {
		t.Errorf("prompt missing reviewer feedback content:\n%s", in.Prompt)
	}
	if !strings.Contains(in.Prompt, "Implement the foo module.") {
		t.Errorf("prompt missing original task brief:\n%s", in.Prompt)
	}
}

