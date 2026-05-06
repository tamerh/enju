// Bot daemon — long-running loop that finds work assigned to a
// bot, runs it through the bot's Handler, and submits the result.
//
// Architecture (Phase 7):
//
//	Bot daemon                Handler (claude/stub/...)
//	   │                            │
//	   ├──── service.FatClient ─────┤
//	   │     (claim, submit,        │
//	   │      git, workspace)       │
//	   ▼                            ▼
//	   coord                       LLM / shell / rules
//
// The daemon owns NO orchestration logic of its own beyond
// "find → claim → handler → submit." All five task actions
// (answer / contribute / compute / review / vote) work because
// the fatclient already supports them; the daemon doesn't need
// per-action branching. This is the architectural fix the Phase
// 7 corrective rewrite landed: pre-7 code reimplemented a tiny
// subset of fat-client functionality as parallel infrastructure
// and could only handle 2/5 actions.
//
// Mirrors webui as a peer consumer of *service.FatClient: same
// "declare a local interface, FatClient satisfies it implicitly,
// tests inject a fake" pattern. See internal/webui/server.go.

package bots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/types"
	"github.com/enju-ai/enju/internal/common/wire"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// fatClient is the local interface the daemon programs against.
// The real *service.FatClient satisfies it implicitly via Go's
// structural typing — no adapter, no type assertion. The
// interface acts as the explicit list of methods this package
// touches; a coord-side surface change becomes a compile error
// here rather than a silent runtime miss.
//
// Type-leak trade-off: the method signatures below carry
// service.ClaimParams / SubmitParams / ClaimResult / TaskMeta
// directly rather than re-declaring bots-local mirror types.
// This keeps the daemon thin (no struct-to-struct copying at
// the boundary) but couples bots' interface to FatClient's
// param shapes — if FatClient ever wants to evolve those
// independently, this interface drags bots along. Acceptable
// today because the params are stable and bots-as-fatclient-
// consumer is exactly the architecture we're locking in; if
// the cost ever shows up the fix is bots-local mirror types
// + a translation layer at the construction site, same shape
// webui would use if its read-side types ever needed
// independent evolution.
type fatClient interface {
	Username() string
	CommitAuthor(ctx context.Context) (name, email string)

	ListProjects(ctx context.Context) ([]wire.Project, error)
	ListRuns(ctx context.Context, projectID int64) ([]wire.Run, error)
	ListReadyTasks(ctx context.Context, projectID, runID int64) ([]map[string]interface{}, error)

	ClaimTask(ctx context.Context, params service.ClaimParams) (*service.ClaimResult, error)
	ReleaseTask(ctx context.Context, taskID string) error
	FetchTaskMeta(ctx context.Context, taskID string) (*service.TaskMeta, error)
	SubmitTaskResult(ctx context.Context, params service.SubmitParams) *service.SubmitResult

	// ResolveBotWorkspace returns the abs path to this bot's
	// per-bot per-project managed clone at
	// `<project>/enju/bots/<botUsername>/clone/`, distinct from
	// the operator's working tree AND from any other bot's
	// clone on the same machine. Threaded to the Handler as
	// cwd so claude -p operates in the bot's own clone —
	// without per-bot isolation, two daemons on the same
	// project share a working tree and trip over each other's
	// branch switches and in-flight writes.
	//
	// botUsername scopes the clone to the citizen identity.
	// One daemon = one citizen, so callers pass d.fc.Username()
	// (the coord-assigned name, which may differ from the
	// manifest's requested name when collision auto-suffix
	// fired during registration). The coord username is what
	// shows up in the audit log and is path-safe by validation.
	//
	// Why this is bot-specific (not the same call humans use):
	// the operator's `enju mcp` flow operates directly on
	// their adopted dir; their working tree IS the project.
	// Bot flows must clone separately, one per bot, so multi-
	// bot fleets work in parallel.
	ResolveBotWorkspace(ctx context.Context, projectID int64, botUsername string) (string, error)

	// ResetBotCloneToCleanState wipes the bot clone's worktree
	// residue between iterations: drops staged + unstaged
	// changes and removes untracked files, leaving the clone
	// synced to whatever HEAD currently is (the run branch
	// tip post-pull). Daemon calls it after ClaimTask and
	// before the handler runs.
	ResetBotCloneToCleanState(ctx context.Context, projectID int64) error

	// CheckoutTopicBranchTip switches HEAD to the named topic
	// branch. Used on iter > 1 re-claim (after request_changes)
	// so the LLM starts on iter-1's tree, not on the run-branch
	// tip the pre-claim pull leaves behind. Caller invokes this
	// BEFORE ResetBotCloneToCleanState so the reset's
	// HardReset-to-HEAD lands on topic-branch state.
	CheckoutTopicBranchTip(ctx context.Context, projectID int64, branch string) error

	// WipeDeclaredWrites removes literal-path entries from
	// `writes` from the worktree. Used on iter > 1 to give the
	// LLM a clean canvas in its declared output paths, so
	// iter-2's commit doesn't union with iter-1's files when
	// the LLM picks different filenames. Glob/dir/templated
	// paths are skipped — literal-path scope only.
	WipeDeclaredWrites(ctx context.Context, projectID int64, writes enjuYaml.WriteArtifacts) error

	// FetchAllRefsForBot syncs the bot clone with every remote
	// branch's refs + objects. Daemon calls it pre-claim so
	// claude-p sees freshly-pushed topic branches from other
	// citizens. Without this step, per-bot clones drift apart
	// and reading bots see stale empty topic branches.
	FetchAllRefsForBot(ctx context.Context, projectID int64) error
}

// Config bundles every dependency the Daemon needs at construction.
// All fields except Logger and the timing knobs are required.
type Config struct {
	// FC is the FatClient handle. The daemon never builds one
	// itself — the caller (cmdBotRun) constructs it with the
	// bot's credentials and hands it in pre-wired.
	FC fatClient

	// Handler is the bot's brain. Built via NewHandler(bot) at
	// the call site so the daemon stays handler-agnostic.
	Handler Handler

	// Bot is the manifest entry. Used for: identity check on
	// task assign_to, system prompt path, model attribution
	// (already baked into handler), and diagnostic logging.
	Bot *Bot

	// SystemPrompt is the bot's system prompt content (already
	// read from disk by the caller). Empty is legal — handlers
	// that don't use a system prompt ignore it.
	SystemPrompt string

	// ProjectID scopes the daemon to a single project. Zero =
	// poll every project the bot is a member of (cross-project
	// mode). For multi-project fleets the recommended layout is
	// one daemon per (bot, project) tuple, but cross-project is
	// supported for solo operators with a few small projects.
	ProjectID int64

	// PollFloor is the minimum sleep between empty polls. The
	// daemon uses exponential backoff: each consecutive empty
	// poll doubles the sleep, capped at BackoffMax. Defaults to
	// 1s when zero.
	PollFloor time.Duration

	// BackoffMax caps the empty-poll sleep. Defaults to 30s when
	// zero. Keeps the daemon responsive even on idle days
	// without burning CPU on a tight loop.
	BackoffMax time.Duration

	// Logger is the slog handle. nil falls back to slog.Default().
	Logger *slog.Logger
}

// Daemon is the bot's long-running loop. Construct with New,
// invoke Run with a cancellable context, optionally call
// ReleaseActiveClaim before exit if Run returned early without
// going through the deferred-release path.
type Daemon struct {
	fc         fatClient
	handler    Handler
	bot        *Bot
	systemPrompt string
	projectID  int64
	pollFloor  time.Duration
	backoffMax time.Duration
	logger     *slog.Logger

	// activeClaim is the task id this daemon currently has
	// claimed. Empty when between iterations. Set on successful
	// claim, cleared on submit/release. Read by the deferred
	// shutdown path so a Ctrl-C mid-handler still releases the
	// claim cleanly.
	activeClaim string
}

// New constructs a Daemon. Validates required Config fields up
// front — FC, Handler, and Bot are load-bearing; the daemon
// can't usefully start without them.
func New(cfg Config) (*Daemon, error) {
	if cfg.FC == nil {
		return nil, fmt.Errorf("FC is required")
	}
	if cfg.Handler == nil {
		return nil, fmt.Errorf("Handler is required")
	}
	if cfg.Bot == nil {
		return nil, fmt.Errorf("Bot is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pollFloor := cfg.PollFloor
	if pollFloor == 0 {
		pollFloor = 1 * time.Second
	}
	backoffMax := cfg.BackoffMax
	if backoffMax == 0 {
		backoffMax = 30 * time.Second
	}
	return &Daemon{
		fc:           cfg.FC,
		handler:      cfg.Handler,
		bot:          cfg.Bot,
		systemPrompt: cfg.SystemPrompt,
		projectID:    cfg.ProjectID,
		pollFloor:    pollFloor,
		backoffMax:   backoffMax,
		logger:       logger,
	}, nil
}

// Run drives the daemon's main loop until ctx is cancelled. The
// loop:
//
//  1. Find a task assigned to this bot (across projects/runs).
//  2. Claim it.
//  3. Hand the rendered prompt to the handler.
//  4. Submit the response.
//
// Empty polls trigger exponential backoff bounded by BackoffMax.
// Per-iteration errors are logged and the loop continues — a
// transient claim-race or submit-failure shouldn't take the
// daemon down. ctx cancellation breaks the loop and triggers a
// best-effort release of any in-flight claim.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.ReleaseActiveClaim(context.Background())

	d.logger.Info("bot daemon starting",
		"bot", d.bot.Name,
		"project_id", d.projectID,
		"username", d.fc.Username())

	backoff := d.pollFloor
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		worked, err := d.runOnce(ctx)
		if err != nil {
			// Permanent deployment-config errors short-circuit
			// the loop. Without this, a misconfigured project
			// (no remote_url AND no registered adopted path)
			// would loop forever logging the same workspace-
			// resolve failure every poll cycle. Surface the
			// error and exit so the operator sees the cause
			// once and can fix it; restarting with the same
			// config would only reproduce the spam.
			if errors.Is(err, service.ErrNoCloneSource) {
				d.logger.Error("permanent config error — exiting daemon",
					"error", err,
					"hint", "set remote_url with enju_set_project_remote, or run enju_init --path= to register an adopted tree")
				return err
			}
			// Log and keep going — a one-off failure on iter
			// N shouldn't kill the daemon. Real "this whole
			// daemon is broken" cases (no FC, panicked
			// handler) bubble up via context cancellation or
			// process exit, not via per-iteration errors.
			d.logger.Warn("iteration error", "error", err)
		}
		// Reset backoff only on a CLEAN successful iteration
		// (claim + handler + submit all OK). Errors mid-iteration
		// — failed claim, handler crash, submit refusal — must
		// still back off, otherwise the daemon hammers the coord
		// in a tight loop on persistent failure modes (e.g. a
		// bot mis-scoped to tasks it can't read produces 8
		// errors per second instead of 1 every few seconds).
		// Empty polls (worked=false, err=nil) also back off, on
		// the same code path.
		if worked && err == nil {
			backoff = d.pollFloor
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > d.backoffMax {
			backoff = d.backoffMax
		}
	}
}

// RunOnce performs at most one find-claim-process-submit cycle
// and returns. Used by --once mode (CI-style "wake up, do any
// pending work, exit") and the integration test harness.
//
// Returns (worked=true, nil) on a successful submit, (false, nil)
// on an empty poll, (worked, err) on iteration failure with
// worked=true if the failure happened post-claim (so the caller
// can see that a claim happened even if the submit didn't).
func (d *Daemon) RunOnce(ctx context.Context) (bool, error) {
	defer d.ReleaseActiveClaim(context.Background())
	return d.runOnce(ctx)
}

// runOnce is the body of one iteration. Split out so Run's loop
// and RunOnce share the implementation without duplicating the
// "release on failure" defer ordering.
func (d *Daemon) runOnce(ctx context.Context) (bool, error) {
	taskID, projectID, err := d.findWork(ctx)
	if err != nil {
		return false, fmt.Errorf("find work: %w", err)
	}
	if taskID == "" {
		return false, nil
	}

	// Pre-warm the bot's managed clone for this project BEFORE
	// the claim. ClaimTask's pre-claim reconcile internally
	// calls FatClient.OpenProject, which goes through
	// workspace.ForProject — and ForProject's externalDirs
	// short-circuit would otherwise resolve to the operator's
	// adopted dir AND poison the workspace's per-project cache
	// (`ws.clients[projectID]`) with that handle. Subsequent
	// SubmitTaskResult and HandlerInput.Workspace calls would
	// then keep returning the operator's tree even though the
	// bot explicitly asked for a managed clone.
	//
	// By calling ResolveBotWorkspace first we force
	// OpenBotCloneAt to populate the cache with the bot clone
	// at `<projectHome>/enju/.clone/`. ForProject's cache check
	// sees that and returns it.
	//
	// Production symptom this fixes (ISSUE-007 follow-up): bot
	// daemon claimed work, claude -p ran in the operator's
	// tree, bot's git checkout switched the operator's tree to
	// the topic branch, multi-task runs jammed at the second
	// branch switch on residue from the first task's commit.
	workspacePath, err := d.fc.ResolveBotWorkspace(ctx, projectID, d.fc.Username())
	if err != nil {
		return false, fmt.Errorf("resolve workspace for project %d: %w", projectID, err)
	}
	if workspacePath == "" {
		return false, fmt.Errorf("workspace path empty for project %d", projectID)
	}

	// Sync the bot's clone with everything other citizens have
	// pushed since the last poll. With per-bot clones, each
	// bot's local object DB only carries what THIS bot has
	// fetched — without an explicit fetch, reviewer-bot reading
	// developer-bot's freshly-pushed topic branch sees an empty
	// branch and emits a bogus request_changes (production saw
	// this as the develop_config rejection loop). Best-effort:
	// a fetch failure is logged but doesn't block the claim;
	// ReadFileAtCommit's lazy-fetch fallback picks up the slack.
	if ferr := d.fc.FetchAllRefsForBot(ctx, projectID); ferr != nil {
		d.logger.Warn("pre-claim fetch failed; will rely on read-time lazy fetch",
			"project_id", projectID, "error", ferr)
	}

	claim, err := d.fc.ClaimTask(ctx, service.ClaimParams{
		TaskID:         taskID,
		IncludeContext: true,
	})
	if err != nil {
		// Most claim races are "task already claimed by someone
		// else" — distinct from a real failure. The error
		// message is the only signal today; if/when the coord
		// returns structured error codes the classification
		// would key off those instead.
		if isAlreadyClaimedError(err) {
			d.logger.Debug("claim race lost", "task_id", taskID)
			return false, nil
		}
		return false, fmt.Errorf("claim %s: %w", taskID, err)
	}
	d.activeClaim = taskID

	if err := d.processAndSubmit(ctx, taskID, claim, workspacePath); err != nil {
		// Don't auto-release on submit failure — the claim is
		// ours, the work either succeeded or didn't. A retry
		// pass on the same task can be valuable. The deferred
		// ReleaseActiveClaim catches the genuine shutdown case.
		return true, fmt.Errorf("process+submit %s: %w", taskID, err)
	}
	d.activeClaim = ""
	return true, nil
}

// findWork locates a task assigned to this bot. Returns "" when
// no work is currently available. Returns the first match by
// (project, run, seq) order — deterministic across daemon
// restarts so two daemons of the same bot don't race on the
// same task more often than necessary (the coord still arbitrates
// the actual claim).
//
// Cost: O(projects × runs × ready_tasks) per iteration in
// cross-project mode (ProjectID == 0). Fine for solo operators
// with a handful of small projects, which is the v1 target.
// Multi-tenant fleets running bots across many projects should
// pin per-project daemons (--project-id=N) — the cost-cap there
// is one project's runs × tasks per poll. The proper fix is
// server-side filtering: a `/api/v1/tasks/ready?assign_to=USER`
// query parameter that pushes the filter to the coord and
// returns one flat list per request. Tracked as a coord-side
// follow-up; would also benefit other surfaces (UI's
// "what's waiting for me" view).
func (d *Daemon) findWork(ctx context.Context) (taskID string, projectID int64, err error) {
	username := d.fc.Username()

	// Resolve the project list. ProjectID > 0 narrows to a
	// single project; ProjectID == 0 walks every project the
	// bot is a member of (coord-side membership gate enforces
	// access).
	var projectIDs []int64
	if d.projectID > 0 {
		projectIDs = []int64{d.projectID}
	} else {
		projects, perr := d.fc.ListProjects(ctx)
		if perr != nil {
			return "", 0, fmt.Errorf("list projects: %w", perr)
		}
		for _, p := range projects {
			projectIDs = append(projectIDs, p.ID)
		}
	}

	for _, pid := range projectIDs {
		runs, err := d.fc.ListRuns(ctx, pid)
		if err != nil {
			d.logger.Warn("list runs failed; skipping project",
				"project_id", pid, "error", err)
			continue
		}
		for _, r := range runs {
			// Coord's /tasks/ready treats ?run_id=N as per-project
			// RunSeq, not the global int64 id. Passing r.ID
			// (global) instead of r.Seq makes the lookup miss
			// coord-side, and pre-fix the coord fell through to
			// "all projects" — bots scoped to project 3 would
			// see ready tasks from every other project, claim
			// them via the legacy zero-member bypass, then 403
			// on the read path. Pin the per-project run-seq
			// here.
			tasks, err := d.fc.ListReadyTasks(ctx, pid, int64(r.Seq))
			if err != nil {
				d.logger.Warn("list ready tasks failed; skipping run",
					"project_id", pid, "run_id", r.ID, "error", err)
				continue
			}
			for _, t := range tasks {
				if !taskAssignableTo(t, username) {
					continue
				}
				if id, _ := t["id"].(string); id != "" {
					// Return the (taskID, projectID) pair so
					// runOnce can pre-warm the bot's managed
					// clone for this project before the claim
					// fires (see runOnce comment).
					return id, pid, nil
				}
			}
		}
	}
	return "", 0, nil
}

// processAndSubmit hands a claimed task to the handler and
// submits the result. Pre-conditions: task is claimed by us,
// claim's Inputs blob carries the resolved prompt.
//
// Mid-handler run-terminate detection is NOT implemented here:
// once Handler.ProcessTask is invoked the daemon waits for it
// to return before checking task state again. If the operator
// fires enju_terminate_run mid-LLM-call the daemon happily
// burns API tokens until the LLM returns, then sees the
// "cannot accept result" refusal at submit. Token-wasteful but
// correctness-safe — the cascaded skip already abandoned the
// claim coord-side, so the failed submit doesn't leave bad
// state behind. The fix is a heartbeat goroutine that probes
// claim status and cancels the handler ctx on flip; deferred
// until the cost shows up against a real LLM workload.
func (d *Daemon) processAndSubmit(ctx context.Context, taskID string, claim *service.ClaimResult, workspacePath string) error {
	meta, err := d.fc.FetchTaskMeta(ctx, taskID)
	if err != nil {
		return fmt.Errorf("fetch meta: %w", err)
	}
	if meta == nil {
		return fmt.Errorf("no meta for claimed task")
	}

	prompt := extractResolvedPrompt(claim.Inputs)
	if prompt == "" {
		// Fall back to the raw template if we can't find a
		// rendered prompt — better than silently submitting
		// against an empty brief. The template still has the
		// per-task instructions; only the {{ref}} substitutions
		// are missing.
		prompt = meta.Prompt
	}

	// workspacePath was resolved by runOnce BEFORE ClaimTask
	// so the managed clone is cached in ws.clients[projectID]
	// and ClaimTask's pre-claim reconcile + this submit both
	// see it. See runOnce's comment for why pre-warming is
	// load-bearing.

	// Revision branch state: on iter > 1 (re-claim after a
	// reviewer's request_changes verdict), the existing topic
	// branch already carries iter-1's commit. The pre-claim
	// pull leaves HEAD on the run branch — exactly the wrong
	// place for a revision, since the LLM should start from
	// iter-1's tree (where the reviewer's feedback applies),
	// not from the run-branch tip. Switch to the topic branch
	// FIRST so the subsequent ResetBotCloneToCleanState lands
	// the worktree on iter-1's state. Without this, the LLM
	// runs against the wrong base, writes new files (often with
	// non-deterministic filenames), and the submit-time
	// CheckoutBranchFrom blows up with "worktree contains
	// unstaged changes" because index ↔ worktree don't match.
	if meta.IterSeq > 1 && meta.IterationBranch != "" {
		if cerr := d.fc.CheckoutTopicBranchTip(ctx, meta.ProjectID, meta.IterationBranch); cerr != nil {
			return fmt.Errorf("checkout topic branch %q for revision: %w", meta.IterationBranch, cerr)
		}
	}

	// Reset the bot clone to a clean state before the handler
	// runs. ClaimTask's pre-claim pull (or the iter > 1 topic
	// checkout above) advanced HEAD to the right tip; the reset
	// drops any leftover uncommitted edits and untracked files
	// from the previous iteration so the handler starts on the
	// same canvas every time. Without this, residue accumulates
	// across tasks and the next CheckoutBranchFrom can produce
	// a staged-deletion-plus-untracked desync.
	//
	// Best-effort: a reset failure logs but doesn't abort the
	// iteration — the handler may still succeed if the residue
	// happens not to collide with this task's outputs. Hard
	// errors surface on the subsequent submit instead, where
	// they're already handled.
	if rerr := d.fc.ResetBotCloneToCleanState(ctx, meta.ProjectID); rerr != nil {
		d.logger.Warn("bot clone reset failed; continuing with possibly-dirty worktree",
			"task_id", taskID, "project_id", meta.ProjectID, "error", rerr)
	}

	// On iter > 1, wipe the prior iteration's declared writes
	// so the LLM starts with a clean canvas in those paths and
	// iter-2's commit carries iter-2's content only — not a
	// union of both iterations' files (LLM non-determinism on
	// filenames would otherwise produce that union and confuse
	// reviewers). Iter-1's files remain reachable in iter-1's
	// commit history; the topic branch stacks-on-top so both
	// SHAs stay queryable.
	if meta.IterSeq > 1 && len(meta.WritesArtifacts) > 0 {
		if werr := d.fc.WipeDeclaredWrites(ctx, meta.ProjectID, meta.WritesArtifacts); werr != nil {
			return fmt.Errorf("wiping prior iteration's writes: %w", werr)
		}
	}

	// Prepend reviewer feedback to the prompt on a revision so
	// the LLM understands what the reviewer asked to change.
	// Without this, iter-2's prompt is identical to iter-1's
	// and the LLM's "revision" is just stochastic-sampling
	// noise on the same brief.
	if len(claim.ReviewFeedback) > 0 {
		var b strings.Builder
		b.WriteString("# Reviewer feedback from previous iteration\n\n")
		b.Write(claim.ReviewFeedback)
		b.WriteString("\n\n# Original task\n\n")
		b.WriteString(prompt)
		prompt = b.String()
	}

	out, err := d.handler.ProcessTask(ctx, HandlerInput{
		TaskID:         meta.ID,
		Action:         meta.Action,
		Prompt:         prompt,
		SystemPrompt:   d.systemPrompt,
		Workspace:      workspacePath,
		ReviewFeedback: string(claim.ReviewFeedback),
	})
	if err != nil {
		return fmt.Errorf("handler: %w", err)
	}

	authorName, authorEmail := d.fc.CommitAuthor(ctx)
	params := service.SubmitParams{
		TaskID:      taskID,
		Meta:        meta,
		Content:     out.Response,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
	}

	// Honor the task's writes_artifacts declaration. Expand the
	// declared patterns (literal / glob / directory) against the
	// working tree the bot just wrote into; required-but-missing
	// declarations fail the iteration loudly (silent acceptance
	// was the data-loss bug fixed earlier this session). Optional
	// declarations that produced nothing fold silently.
	//
	// The expanded set is split:
	//   - Tracked entries → read content into params.Artifacts so
	//     prepareFatSubmit packs them into the commit.
	//   - Untracked entries → paths only into
	//     params.UntrackedArtifacts so the coord records a
	//     tracked=false index row but the file stays out of git.
	if len(meta.WritesArtifacts) > 0 {
		expanded, missing, err := meta.WritesArtifacts.ExpandAgainstWorkdir(workspacePath)
		if err != nil {
			return fmt.Errorf("expanding writes_artifacts: %w", err)
		}
		if len(missing) > 0 {
			// Only required (non-optional) entries land in missing —
			// the expander folds optional misses silently. Naming
			// the path AND noting it was required helps the operator
			// triage at a glance: was the bot supposed to write
			// this, or did the manifest forget `optional: true`?
			return fmt.Errorf("required writes_artifacts missing on disk (declare `optional: true` if absence is acceptable): %s", strings.Join(missing, ", "))
		}
		artifactContents := make(map[string]string)
		var untrackedPaths []string
		for _, e := range expanded {
			if e.Track {
				body, rerr := os.ReadFile(filepath.Join(workspacePath, e.Path))
				if rerr != nil {
					// Expansion stat'd this path moments ago, so
					// a read failure here is transient IO. Surface
					// the same message format expansion would so
					// the operator can't tell the two apart and
					// retry resolves both.
					return fmt.Errorf("required writes_artifacts missing on disk (declare `optional: true` if absence is acceptable): %s", e.Path)
				}
				artifactContents[e.Path] = string(body)
				continue
			}
			untrackedPaths = append(untrackedPaths, e.Path)
		}
		if len(artifactContents) > 0 {
			params.Artifacts = artifactContents
		}
		if len(untrackedPaths) > 0 {
			params.UntrackedArtifacts = untrackedPaths
		}
	}
	// Action-specific shape resolution. The Handler can either:
	//
	//   - Return plain text in Response and let the daemon parse
	//     it with action-specific heuristics. Default for
	//     `claude -p` style handlers.
	//
	//   - Pre-fill HandlerOutput.Decision / .Option directly. The
	//     daemon trusts the handler's structured output and
	//     skips text parsing. For handlers that KNOW the shape
	//     (JSON-mode LLM, rule-based handler, custom prompt
	//     convention).
	//
	// The text-parsing fallback handles the common case of LLMs
	// writing think-then-conclude prose, with parseReviewResponse
	// trying multiple patterns (DECISION: marker bottom-up,
	// first-line bare keyword, last-line bare keyword) before
	// defaulting to request_changes. Vote has no safe default,
	// so an unparseable response surfaces as iteration error.
	switch meta.Action {
	case "review":
		if out.Decision != "" {
			// Trust-but-normalize: handler said "approve" but
			// might have any casing/punctuation. Run through
			// normalizeVerdict for canonical form. If it
			// doesn't match a known verdict, fall back to text
			// parsing — defense in depth against a buggy
			// custom handler.
			if v, ok := normalizeVerdict(out.Decision); ok {
				params.Decision = v
				params.Content = out.Response
			} else {
				params.Decision, params.Content = parseReviewResponse(out.Response)
			}
		} else {
			params.Decision, params.Content = parseReviewResponse(out.Response)
		}
	case "vote":
		if out.Option != "" {
			// Trust the handler verbatim — option ids are
			// task-defined, no canonical form to validate
			// against. The coord rejects unknown options on
			// submit if the handler got it wrong.
			params.Option = out.Option
			params.Content = out.Response
		} else {
			opt, content, err := parseVoteResponse(out.Response, meta.VoteOptionsJSON)
			if err != nil {
				// No safe default for vote — every option
				// means something. Fail the iteration loudly
				// so the claim releases and an operator (or
				// the next iteration with a tweaked prompt)
				// can retry.
				return fmt.Errorf("vote: %w (response: %s)", err, truncate(out.Response, 200))
			}
			params.Option, params.Content = opt, content
		}
	}

	res := d.fc.SubmitTaskResult(ctx, params)
	if res != nil && res.ErrorMessage != "" {
		return fmt.Errorf("submit: %s", res.ErrorMessage)
	}
	return nil
}

// ReleaseActiveClaim hands a claimed-but-not-submitted task back
// to the queue. Called from Run's deferred path so a Ctrl-C
// mid-handler doesn't leak the claim. Idempotent: no-op when
// activeClaim is empty.
func (d *Daemon) ReleaseActiveClaim(ctx context.Context) {
	if d.activeClaim == "" {
		return
	}
	taskID := d.activeClaim
	d.activeClaim = ""
	if err := d.fc.ReleaseTask(ctx, taskID); err != nil {
		d.logger.Warn("release on shutdown failed",
			"task_id", taskID, "error", err)
		return
	}
	d.logger.Info("released active claim on shutdown", "task_id", taskID)
}

// taskAssignableTo returns true when the task's assign_to
// includes username, or assign_to is empty (open task). The
// coord-side gate is authoritative; this client-side check is
// just to avoid wasting a claim POST on tasks we can't win.
func taskAssignableTo(task map[string]interface{}, username string) bool {
	raw, ok := task["assign_to"]
	if !ok || raw == nil {
		return true // open task
	}
	list, ok := raw.([]interface{})
	if !ok || len(list) == 0 {
		return true
	}
	for _, v := range list {
		if s, _ := v.(string); s == username {
			return true
		}
	}
	return false
}

// extractResolvedPrompt pulls "resolved_prompt" out of the
// inputs JSON the fat-client produces during ClaimTask. Returns
// empty string when the blob is malformed or the field is
// missing — caller falls back to the raw template.
func extractResolvedPrompt(inputs []byte) string {
	if len(inputs) == 0 {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(inputs, &m); err != nil {
		return ""
	}
	if s, _ := m["resolved_prompt"].(string); s != "" {
		return s
	}
	return ""
}

// parseReviewResponse extracts the verdict + rationale from a
// review handler's free-text output. Real LLMs write whatever
// they feel like — bare keyword first, decoration ("**DECISION:
// approve**"), block-quoted ("> approve"), inline code (`approve`),
// header ("# Approve"), indented in a code block, or buried
// after several paragraphs of think-out-loud prose. A strict
// pattern matcher loses this arms race; instead we strip
// markdown decoration line-by-line and scan the WHOLE response
// for verdict keywords with a deterministic precedence rule.
//
// Algorithm:
//
//  1. Clean every line: strip leading list/blockquote/header
//     markers, strip emphasis characters (* _ ` ~) globally on
//     the line. The cleaned form is used ONLY for keyword
//     matching; the original response goes into the rationale
//     untouched.
//
//  2. Scan bottom-up for `DECISION: <verdict>` on any cleaned
//     line. Strongest signal — the LLM explicitly labeled its
//     answer. Bottom-up means a late-revising LLM
//     ("...considering reject... DECISION: approve") gets its
//     final word counted.
//
//  3. If no marker, scan all cleaned lines for a bare verdict
//     keyword (the whole line, after cleaning, IS exactly one
//     of the four verdicts modulo trailing punctuation). Take
//     the LAST hit — punch-line style ("I conclude X.\napprove")
//     wins over a leading bare keyword that the LLM might have
//     softened later.
//
//  4. No hit anywhere → fall back to `request_changes` + full
//     response as rationale. Safer than silent approve when the
//     LLM was unparseably wordy.
//
// What we deliberately don't do: per-bot regex configs,
// JSON-mode parsing, model-specific shape switches. A bot
// author with a non-default response shape implements a custom
// Handler and pre-fills HandlerOutput.Decision (the structured-
// output escape hatch). The daemon's parser stays the
// best-effort default for plain `claude -p`.
func parseReviewResponse(s string) (decision, rationale string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return string(types.ReviewDecisionRequestChanges), ""
	}
	rawLines := strings.Split(s, "\n")
	cleaned := make([]string, len(rawLines))
	for i, l := range rawLines {
		cleaned[i] = cleanForVerdict(l)
	}

	// 1. DECISION: marker (bottom-up across all cleaned lines).
	for i := len(cleaned) - 1; i >= 0; i-- {
		if v, ok := extractDecisionFromLine(cleaned[i]); ok {
			return v, s
		}
	}

	// 2. Bare keyword line, prefer LAST hit. (Multiple matches
	// across the response → the LLM wrote both an early aside
	// and a final answer; trust the final.)
	var hit string
	for _, line := range cleaned {
		if v, ok := normalizeVerdict(line); ok {
			hit = v
		}
	}
	if hit != "" {
		return hit, s
	}

	// 3. Fallback.
	return string(types.ReviewDecisionRequestChanges), s
}

// cleanForVerdict produces a keyword-search-friendly form of
// one line. Lossy by design — strips characters that don't
// appear in any verdict keyword — and used only for matching.
// The original line text is preserved verbatim in the rationale.
//
// Two passes:
//
//  1. Peel line-prefix markers repeatedly: blockquote (`>`),
//     ATX headers (`#`, `##`, ...), list bullets (`-`, `*`,
//     `+`), and any combination ("> ## **approve**" → "**approve**"
//     → "approve").
//  2. Strip all `*` `_` `` ` `` `~` characters globally. These
//     are markdown emphasis / code / strikethrough wrappers.
//     Verdicts contain none of them, so deleting them
//     unconditionally is safe.
//
// Trailing whitespace + punctuation that normalizeVerdict
// already handles (`. : ! ? , ;`) stays — that helper does the
// final cleanup on the candidate verdict token.
func cleanForVerdict(line string) string {
	for {
		prev := line
		line = strings.TrimSpace(line)
		// Single-character line-prefix markers, ordered
		// longest-first so "## " beats "# ". We only peel one
		// per loop iteration; the outer for retries until
		// fixed-point so deeply nested markers all come off.
		for _, prefix := range []string{"### ", "## ", "# ", "> ", ">", "- ", "+ "} {
			if strings.HasPrefix(line, prefix) {
				line = line[len(prefix):]
				break
			}
		}
		// "* " is a list bullet at line start, but "*" anywhere
		// (including line start with no following space) is
		// emphasis. Distinguish only at the prefix step.
		if strings.HasPrefix(line, "* ") {
			line = line[2:]
		}
		if line == prev {
			break
		}
	}
	// Strip emphasis / code / strikethrough characters globally.
	// Verdict keywords (`approve`, `request_changes`, `reject`,
	// `comment`) contain none of these, so the strip is safe.
	line = strings.Map(func(r rune) rune {
		switch r {
		case '*', '_', '`', '~':
			return -1
		}
		return r
	}, line)
	return strings.TrimSpace(line)
}

// extractDecisionFromLine looks at one already-cleaned line and
// returns (verdict, true) if the line starts with `DECISION:`
// (case-insensitive) followed by a recognized verdict token.
// The line should already be free of markdown decoration —
// callers pass cleanForVerdict(rawLine).
func extractDecisionFromLine(line string) (string, bool) {
	const marker = "decision:"
	if len(line) < len(marker) {
		return "", false
	}
	if !strings.EqualFold(line[:len(marker)], marker) {
		return "", false
	}
	rest := strings.TrimSpace(line[len(marker):])
	if rest == "" {
		return "", false
	}
	// First whitespace-separated token after the colon is the
	// verdict; trailing rationale on the same line ("DECISION:
	// approve — the design is sound") is recovered via the
	// full-response rationale.
	token := rest
	if i := strings.IndexAny(token, " \t"); i > 0 {
		token = token[:i]
	}
	return normalizeVerdict(token)
}

// normalizeVerdict maps a free-text first line to one of the
// canonical verdicts. Trims leading/trailing punctuation and
// whitespace, lowercases, then checks against the enum. Returns
// (canonical, true) on hit and ("", false) on miss.
//
// Accepts a few common LLM idioms:
//
//	"APPROVE"            -> approve
//	"approve."           -> approve
//	"Approve:"           -> approve
//	"REQUEST_CHANGES"    -> request_changes
//	"request changes"    -> request_changes  (space form)
//	"REJECT"             -> reject
//	"comment"            -> comment
//
// Anything richer (markdown bullets, fenced blocks, JSON-shaped
// answers) doesn't match — by design, we want explicit verdicts
// at line 1, not heuristic excavation. Bots that want richer
// shapes graduate to a structured Handler output type later.
func normalizeVerdict(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ".:!?,;\"'`*")
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	// Accept "request changes" (space) as well as the canonical
	// "request_changes" — both are natural for an LLM to emit.
	if s == "request changes" {
		return string(types.ReviewDecisionRequestChanges), true
	}
	if types.IsValidReviewDecision(s) {
		return s, true
	}
	return "", false
}

// parseVoteResponse extracts the chosen option + rationale from
// a vote handler's free-text output. Unlike review, there is no
// safe default — every option carries meaning, so a wordy or
// unparseable response is a hard error rather than a fallback.
// The caller turns this into an iteration-level error which
// releases the claim coord-side; a future iteration with a
// tweaked prompt can retry.
//
// Algorithm:
//
//  1. Read the manifest's declared options from
//     meta.VoteOptionsJSON (a JSON-encoded list of
//     {id: ..., ...} objects).
//  2. First-line match: if the first non-empty line, normalized
//     (trimmed, lowercased), equals one of the option ids, take
//     it and put the remainder in rationale.
//  3. Whole-response scan: if first-line didn't match, search
//     the response for any option id appearing as a standalone
//     token (word-boundaries). On exactly one hit, take it and
//     keep the full response as rationale.
//  4. Otherwise, error.
//
// optionsJSON shapes accepted: the canonical
// `[{"id": "...", "label": "..."}, ...]` shape, plus the
// degenerate `["id1", "id2"]` legacy shape — both round-trip
// here, since coord-side parsing accepts both.
func parseVoteResponse(s, optionsJSON string) (option, rationale string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("empty response")
	}
	options := parseVoteOptions(optionsJSON)
	if len(options) == 0 {
		// Coord declared no options — best we can do is
		// preserve old behavior and ship the first line as the
		// option. The coord will reject if the task actually
		// expected something specific.
		first, rest := splitFirstLine(s)
		return first, rest, nil
	}
	first, rest := splitFirstLine(s)
	firstLower := strings.ToLower(strings.Trim(first, ".:!?,;\"'`*"))
	for _, opt := range options {
		if strings.ToLower(opt) == firstLower {
			return opt, rest, nil
		}
	}
	// Whole-response scan. Look for any option appearing as a
	// standalone token. Multiple matches → ambiguous → error,
	// because picking the first or last would silently bias
	// the vote.
	lower := strings.ToLower(s)
	var hits []string
	for _, opt := range options {
		needle := strings.ToLower(opt)
		if containsWord(lower, needle) {
			hits = append(hits, opt)
		}
	}
	if len(hits) == 1 {
		return hits[0], s, nil
	}
	if len(hits) > 1 {
		return "", "", fmt.Errorf("response mentions multiple options %v; cannot disambiguate", hits)
	}
	return "", "", fmt.Errorf("response does not name any of the declared options %v", options)
}

// parseVoteOptions extracts option ids from the coord's
// VoteOptionsJSON. Tolerates both shapes:
//   - `[{"id":"a", "label":"..."}, {"id":"b"}]`
//   - `["a", "b"]`
//
// Empty / malformed JSON returns nil — caller falls back to
// pass-through behavior.
func parseVoteOptions(j string) []string {
	if j == "" {
		return nil
	}
	// Try the object-shape first.
	var objs []map[string]interface{}
	if err := json.Unmarshal([]byte(j), &objs); err == nil {
		out := make([]string, 0, len(objs))
		for _, o := range objs {
			if id, _ := o["id"].(string); id != "" {
				out = append(out, id)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Try the bare-string shape.
	var bare []string
	if err := json.Unmarshal([]byte(j), &bare); err == nil {
		return bare
	}
	return nil
}

// containsWord reports whether s contains needle as a whole
// "word" — surrounded by non-letter-or-digit characters or
// string boundaries. Prevents "yes" from matching inside
// "yesterday".
func containsWord(s, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] != needle {
			continue
		}
		left := i == 0 || !isWordByte(s[i-1])
		right := i+len(needle) == len(s) || !isWordByte(s[i+len(needle)])
		if left && right {
			return true
		}
	}
	return false
}

func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z',
		b >= 'A' && b <= 'Z',
		b >= '0' && b <= '9',
		b == '_':
		return true
	}
	return false
}

// truncate caps a string at n bytes for inclusion in error
// messages. Avoids dumping a 4kB LLM response into a log line.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// splitFirstLine separates the first non-empty line from the
// remainder.
func splitFirstLine(s string) (head, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	idx := strings.IndexByte(s, '\n')
	if idx < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:])
}

// isAlreadyClaimedError reports whether err is the coord's
// "task already claimed" race response. Substring match against
// the exact strings emitted by internal/engine/claim.go's
// ComputeClaim path — same brittleness as
// claimOneForSelector's classification (in
// internal/fatclient/service/claim_batch.go), with the same
// long-term fix: structured error codes on the JSON response
// (`{"error": "...", "code": "already_claimed"}`) so both
// classifiers can key off `code` rather than message text.
// If the coord-side wording shifts, races would surface as
// generic iteration errors and trigger backoff unnecessarily —
// not a correctness bug but a noisy log.
func isAlreadyClaimedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already claimed") ||
		strings.Contains(msg, "cannot accept result") ||
		errors.Is(err, errAlreadyClaimed)
}

// errAlreadyClaimed is exported only for tests that want to
// assert the daemon treats the race as "skip, don't crash."
// Production code goes through the substring path above.
var errAlreadyClaimed = errors.New("already claimed")
