// Bot runner — the poll/claim/invoke-LLM/submit loop.
//
// One Runner instance binds a single bot identity (token,
// model, system prompt, tool allowlist) to a single
// coordinator + project scope. The fatclient supervisor (Phase
// 4+, future) launches one Runner per (project, bot) pair.
//
// Walking-skeleton scope: text-action tasks only (review,
// vote, plain answer with no git work). action=contribute /
// compute land in a later phase along with worktree
// management — see docs/bots.md for the staging plan.
//
// Claim-leak policy: the runner only tracks one in-flight
// claim at a time (activeTaskID). When an LLM invocation
// fails, the claim stays open coord-side and the runner
// remembers it so the deferred shutdown release covers it.
// But under continuous operation, successive LLM failures
// overwrite activeTaskID — only the most recent failed
// claim is tracked. Earlier failed claims are cleaned up by
// the coord's reaper, NOT by the runner. This is intentional
// — building a "release every prior failed claim before next
// iteration" loop would slow normal operation for what's
// already covered by the reaper's safety net.

package bots

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// CoordClient is the bot runner's view of the coordinator. The
// abstraction lets tests inject a fake driver without spinning
// up a coord HTTP server, AND lets a real implementation evolve
// the wire shape (submit body fields, claim parameters) without
// rippling through the runner.
//
// Methods are intentionally narrow — listing + claiming +
// submitting is the whole loop's coord dance. Anything richer
// (run state queries, event subscription, etc.) belongs in a
// follow-up interface or in the Runner's auxiliary methods.
type CoordClient interface {
	// ListReadyForBot returns the READY tasks the given bot
	// could claim — pre-filtered to "this bot's name appears
	// in assign_to" so the runner doesn't shovel the entire
	// ready list around.
	//
	// projectID can be 0 to mean "across every project the
	// bot is a member of" — the coord-side listing already
	// honors per-project membership. Most bots will pin a
	// single project per Runner instance and pass that id.
	ListReadyForBot(ctx context.Context, projectID int64, botUsername string) ([]TaskInfo, error)

	// Claim attempts to claim the task. Returns ErrClaimRace
	// when another citizen claimed it first (another bot or
	// a human raced); any other error is fatal-for-this-iteration
	// and the runner will back off.
	Claim(ctx context.Context, taskID, botUsername, model string) error

	// Submit posts the LLM's text result for an already-claimed
	// task. The exact body shape is the implementation's
	// concern — the runner just hands over the result bytes
	// and trusts the implementation to wire content/decision/
	// vote_choice/etc. correctly per the task's action.
	Submit(ctx context.Context, task TaskInfo, response string, model string) error

	// Release abandons an in-flight claim without submitting.
	// Called by the runner on graceful shutdown — leaving
	// claims open would wedge work for the next bot until
	// the coord's reaper times them out.
	//
	// Best-effort: failures are logged but don't block
	// shutdown. The reaper handles the worst case if the
	// release POST itself fails.
	Release(ctx context.Context, taskID, botUsername string) error
}

// TaskInfo is the runner's view of a coord task. Only the
// fields the loop actually needs are surfaced; richer task
// detail (artifact provenance, history, instance params)
// stays inside the CoordClient implementation.
type TaskInfo struct {
	ID         string   // full task id "projID:runSeq:taskDefID[:instanceKey]"
	Action     string   // "review" | "vote" | "answer" | ...
	Prompt     string   // task's prompt text (already %{param}-substituted by coord)
	UserPrompt string   // optional addendum from the user
	AssignTo   []string // citizen usernames this task targets
}

// ErrClaimRace is returned by CoordClient.Claim when another
// claimant beat us to the task. The runner treats it as a
// soft-error: skip and pick the next task in the ready list.
var ErrClaimRace = errors.New("claim race: task already taken")

// ErrNoWork is returned by Runner.RunOnce when the coord had
// nothing ready for the bot. The runner uses it to drive the
// Run loop's backoff schedule — empty rounds get progressively
// longer sleep intervals up to BackoffMax.
var ErrNoWork = errors.New("no ready tasks for bot")

// Runner ties one bot identity to one coordinator and one LLM
// backend, then drives the claim/work/submit loop. Pure
// orchestration: no HTTP, no exec, no git — those are the
// CoordClient + LLMBackend's job. Lets tests cover the full
// loop without networking.
type Runner struct {
	Bot          *Bot
	ProjectID    int64
	Coord        CoordClient
	LLM          LLMBackend
	Logger       *slog.Logger

	// PollInterval is the floor sleep between empty polls.
	// Defaults to 1s. Each empty round doubles up to BackoffMax,
	// then resets to PollInterval after a successful claim.
	PollInterval time.Duration

	// BackoffMax caps the empty-round sleep. Defaults to 30s.
	// Long enough to not hammer the coord; short enough that
	// new work is picked up promptly when it lands.
	BackoffMax time.Duration

	// SystemPrompt is the bot's persona / role text loaded from
	// the manifest's system_prompt path. The runner sets this
	// once at construction and reuses it across every iteration.
	SystemPrompt string

	// activeMu guards activeTaskID — set by RunOnce after a
	// successful claim, cleared after submit. The shutdown
	// path reads it under the same lock so a race between
	// "submit completes" and "ctx cancels" can't double-release
	// or release the wrong task.
	//
	// Defensive locking note: today, Run() and the deferred
	// ReleaseActiveClaim run serially (the defer fires after
	// Run returns, not concurrently). The mutex's actual users
	// are tests that probe state from a goroutine. We keep it
	// because Phase 4+ may add concurrent supervision (e.g. a
	// status-reporter goroutine reading activeTaskID for
	// `enju_bot_status`) — getting the locking right once is
	// cheaper than a future race-during-shipping retrofit.
	activeMu     sync.Mutex
	activeTaskID string
}

// setActiveTask records the in-flight claim. Used internally
// by RunOnce; exposed via the unexported method for symmetry
// with clearActiveTask. Single setter so the locking
// discipline lives in one place.
func (r *Runner) setActiveTask(id string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	r.activeTaskID = id
}

func (r *Runner) clearActiveTask() {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	r.activeTaskID = ""
}

// snapshotActiveTask returns the current in-flight claim id
// (or "" if none). Used by ReleaseActiveClaim under shutdown.
func (r *Runner) snapshotActiveTask() string {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	return r.activeTaskID
}

// ReleaseActiveClaim asks the coord to abandon any in-flight
// claim this runner currently holds. Called by Run() on exit
// (deferred) so a graceful shutdown — stdin-EOF, signal,
// context cancellation — frees the claim instead of leaving
// it dangling until the reaper notices.
//
// Best-effort: a release failure is logged but doesn't bubble
// up. The shutdown path is already short on options at that
// point; the reaper is the safety net.
//
// Uses a fresh context with a short deadline rather than the
// runner's (already-cancelled) ctx, since the cancel is what
// triggered us in the first place. 5s is enough for one HTTP
// roundtrip; if the coord is unreachable, the reaper will
// clean up eventually.
func (r *Runner) ReleaseActiveClaim() {
	taskID := r.snapshotActiveTask()
	if taskID == "" {
		return
	}
	logger := r.logger()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Coord.Release(ctx, taskID, r.Bot.Name); err != nil {
		logger.Warn("failed to release claim on shutdown — coord reaper will clean up",
			"task_id", taskID, "error", err)
		return
	}
	r.clearActiveTask()
	logger.Info("released active claim on shutdown", "task_id", taskID)
}

// RunOnce performs one iteration: list ready tasks, claim one,
// invoke the LLM, submit the result. Returns ErrNoWork when
// the ready list was empty so the Run loop can apply backoff;
// any other error is iteration-specific (the loop logs and
// continues).
func (r *Runner) RunOnce(ctx context.Context) error {
	logger := r.logger()
	tasks, err := r.Coord.ListReadyForBot(ctx, r.ProjectID, r.Bot.Name)
	if err != nil {
		return fmt.Errorf("list ready: %w", err)
	}
	if len(tasks) == 0 {
		return ErrNoWork
	}

	// Walk the ready list claiming the first one we win. Race
	// losses (another claimant beat us to it) are routine and
	// not an error — just move on.
	var task *TaskInfo
	for i := range tasks {
		if claimErr := r.Coord.Claim(ctx, tasks[i].ID, r.Bot.Name, r.Bot.Model); claimErr != nil {
			if errors.Is(claimErr, ErrClaimRace) {
				logger.Debug("claim raced, trying next", "task_id", tasks[i].ID)
				continue
			}
			// Real error (auth, server) on this task — surface
			// it. The loop will back off and retry.
			return fmt.Errorf("claim %s: %w", tasks[i].ID, claimErr)
		}
		task = &tasks[i]
		break
	}
	if task == nil {
		// Every task in the ready list was raced away. Treat
		// as no-work for this round — backoff applies, and
		// the next poll will see whatever's still left.
		return ErrNoWork
	}

	// Record the in-flight claim so ReleaseActiveClaim can
	// find it on shutdown. Cleared ONLY after Submit succeeds
	// (see clearActiveTask call below). The LLM-error path
	// deliberately leaves activeTaskID set so the deferred
	// shutdown release covers it — better to release on a
	// later shutdown than to silently lose the tracking.
	//
	// Caveat: under continuous operation, successive LLM
	// failures overwrite activeTaskID each iteration. Only
	// the most recent failed claim is tracked; earlier
	// failures stay open until the coord's reaper times them
	// out. See package doc for the intent.
	r.setActiveTask(task.ID)

	logger.Info("claimed task, invoking LLM",
		"task_id", task.ID, "action", task.Action, "model", r.Bot.Model)

	prompt := composeTaskPrompt(task)
	response, llmErr := r.LLM.Invoke(ctx, LLMRequest{
		Model:        r.Bot.Model,
		SystemPrompt: r.SystemPrompt,
		TaskPrompt:   prompt,
	})
	if llmErr != nil {
		// LLM failure: don't submit garbage. The claim stays
		// open until release/timeout — operators can intervene
		// via enju_release_task or wait for the reaper. Logged
		// so the daemon's tail surfaces the problem.
		logger.Error("LLM invocation failed; leaving claim open",
			"task_id", task.ID, "error", llmErr)
		return fmt.Errorf("llm invoke %s: %w", task.ID, llmErr)
	}

	if err := r.Coord.Submit(ctx, *task, response, r.Bot.Model); err != nil {
		logger.Error("submit failed; claim may need manual release",
			"task_id", task.ID, "error", err)
		return fmt.Errorf("submit %s: %w", task.ID, err)
	}

	// Submit succeeded — the claim is now closed coord-side,
	// so we shouldn't release it on shutdown anymore.
	r.clearActiveTask()
	logger.Info("submitted task", "task_id", task.ID, "response_len", len(response))
	return nil
}

// Run is the long-running daemon loop. Calls RunOnce repeatedly,
// applying exponential backoff on ErrNoWork (empty rounds) and
// resetting the backoff clock after every successful submission.
//
// Exits cleanly on ctx cancellation — returns ctx.Err() so the
// caller can distinguish graceful shutdown from error termination.
//
// Iteration errors (claim failures, LLM errors, submit errors)
// are logged and treated as no-op for the backoff schedule —
// the loop sleeps PollInterval and tries again.
//
// Shutdown shape (Phase 3, shipped):
//   - stdin-EOF: cmdBotRun's watchStdinEOF goroutine cancels
//     ctx when its Stdin reaches EOF; the supervisor (Phase 4)
//     uses this by closing the daemon's stdin pipe.
//   - SIGINT/SIGTERM: cmdBotRun installs signal.NotifyContext.
//   - In-flight LLM: exec.CommandContext kills the subprocess
//     when ctx cancels (ClaudeBackend's WaitDelay caps the
//     I/O drain at 5s).
//   - Active claim: defer r.ReleaseActiveClaim() abandons it
//     so the next bot doesn't have to wait for the reaper.
//
// Run-terminate cooperative exit (operator fires
// enju_terminate_run mid-iteration) is observed at submit
// time — the coord rejects the late submit, the runner logs,
// the loop continues. Mid-iteration polling for "is my run
// still alive?" is deferred; the submit-time signal is
// already coherent given the cooperative-shutdown design.
func (r *Runner) Run(ctx context.Context) error {
	r.applyDefaults()
	logger := r.logger()
	// Defer the release pass: any in-flight claim still tracked
	// by the runner when Run exits gets abandoned. Covers all
	// exit paths uniformly — ctx cancellation, signal, panic-
	// recovery, etc. Cheap no-op when no claim is active.
	defer r.ReleaseActiveClaim()
	backoff := r.PollInterval
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := r.RunOnce(ctx)
		switch {
		case err == nil:
			// Successful claim+submit — reset to floor.
			backoff = r.PollInterval
			continue
		case errors.Is(err, ErrNoWork):
			// Idle: sleep with backoff, then try again.
			logger.Debug("no work; backing off", "sleep", backoff)
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff *= 2
			if backoff > r.BackoffMax {
				backoff = r.BackoffMax
			}
		default:
			// Iteration error — log and treat as a soft retry
			// at PollInterval. Don't crash the daemon for one
			// bad task: a flaky network or a transient coord
			// 5xx shouldn't take the bot down.
			logger.Warn("iteration error; retrying after PollInterval",
				"error", err, "sleep", r.PollInterval)
			if !sleepCtx(ctx, r.PollInterval) {
				return ctx.Err()
			}
		}
	}
}

func (r *Runner) applyDefaults() {
	if r.PollInterval <= 0 {
		r.PollInterval = 1 * time.Second
	}
	if r.BackoffMax <= 0 {
		r.BackoffMax = 30 * time.Second
	}
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// composeTaskPrompt builds the per-task prompt the LLM sees.
// The system prompt (bot persona) goes via LLMRequest.SystemPrompt
// separately; this is the per-call context: task action +
// prompt + optional user-prompt addendum.
//
// IMPORTANT — walking-skeleton limitation: the prompt arrives
// here with `{{param}}` references already substituted by the
// coordinator, but `{{task.X.content}}` references to upstream
// task results are NOT resolved (resolution requires reading
// git blobs at the upstream commit_sha, which needs a
// workspace). A reviewer bot reviewing a task with upstream-
// content references will see literal `{{task.X.content}}`
// strings and grade based on the prompt scaffolding alone —
// useless for substantive review.
//
// For Phase 2 / walking-skeleton, the supported review/vote
// shapes are SELF-CONTAINED prompts (no `{{task.*.content}}`
// dependencies). Phase 2.4+ adds the workspace-aware
// resolution path (calls /tasks/{id}/inputs descriptor +
// reads from git via workspace.ReadFileAtCommit) so bots can
// review tasks with upstream dependencies. cmdBotRun's
// startup banner advertises this so operators don't get a
// surprise.
func composeTaskPrompt(task *TaskInfo) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[Task %s — action: %s]\n\n", task.ID, task.Action))
	if task.Prompt != "" {
		b.WriteString(task.Prompt)
		b.WriteString("\n")
	}
	if task.UserPrompt != "" {
		b.WriteString("\n[Operator note]\n")
		b.WriteString(task.UserPrompt)
		b.WriteString("\n")
	}
	return b.String()
}

// sleepCtx blocks for d or until ctx is done. Returns true on
// normal completion, false on context cancellation. Tiny helper
// because every loop site otherwise repeats the same select
// boilerplate.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
