// Package service is the fat-client orchestration layer between
// per-tool handlers (mcphandlers/*) and the underlying primitives
// (coord HTTP client + workspace git/fs). Methods on FatClient bundle
// the dependencies handlers need — coord client, local workspace,
// citizen identity, model attribution, logger — so each tool's
// orchestration can be expressed without rebuilding the wiring at
// every call site.
//
// The service layer is intentionally thin: it owns the helpers that
// every per-tool flow uses (fetch task meta, open workspace project,
// pull-with-reconcile, commit author cache) and the per-tool service
// methods. It does NOT own MCP transport concerns (parameter
// parsing, response formatting) — those stay in mcphandlers, which
// calls into FatClient.
//
// Mirrors internal/coordinator/service/ on the coord side: same
// "extract orchestration from transport" shape, same "construct
// once, share for the life of the process" lifecycle.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/common/types"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/executor"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

// Config is the constructor input for New. Coord and WorkspaceRoot
// are the load-bearing dependencies; ModelName + Logger are
// process-scoped attribution / diagnostics. WorkspaceRoot is the
// directory used for fat-client housekeeping (logs, scratch, the
// reconcile cursor's .state/ dir); post-NDW.5 it is NOT where
// project clones live — those live at registry-resolved paths.
// Empty disables on-disk workspace flows entirely (test fixtures
// with coord-only setup).
type Config struct {
	Coord         *coord.Client
	WorkspaceRoot string
	ModelName     string
	Logger        *slog.Logger

	// LogName picks the trace log filename. The trace log lives at
	// <projectRoot>/.enju/logs/<LogName>.log; one file per role.
	// `enju mcp` passes "operator", `enju agent run` passes
	// "bot-<botName>". Empty falls back to trace-<pid>.log so
	// ad-hoc / test wirings still get a unique file.
	LogName string

	// ProjectRegistry tracks the projects this fat-client knows
	// about (standard clones + externally adopted dirs). Optional
	// — when nil, ListMaterializedProjects falls back to walking
	// the workspace root, and Register/Touch/Unregister are
	// no-ops. Production wiring (`enju mcp`, `enju ui`) supplies
	// projectreg.Open(projectreg.DefaultPath()); tests can inject
	// a temp-path registry or omit it entirely.
	ProjectRegistry *projectreg.Registry
}

// FatClient is the published consumer handle for the fat-client
// orchestration layer. Constructed once at process boot
// (mcphandlers.Register for `enju mcp`, the analogous wiring in
// `enju ui`, etc.) and shared across every consumer that calls
// into service.* . The methods on FatClient are the contract
// in-process consumers (MCP handlers, web handlers, CLI) program
// against; out-of-process consumers go through an MCP transport
// that wraps the same surface.
//
// Safe for concurrent use — all underlying dependencies are
// themselves goroutine-safe; the profile-cache load is gated by
// sync.Once.
type FatClient struct {
	coord     *coord.Client
	enjugit   *enjugit.Workspace
	modelName string
	logger    *slog.Logger
	projectRegistry  *projectreg.Registry

	// Cached citizen profile (name + email + kind) used to
	// populate git commit author fields on the fat-client submit
	// path and to classify the calling citizen ("human" / "bot" /
	// "model"). Fetched lazily on first use and held for the
	// life of the FatClient.
	profileOnce  sync.Once
	profileName  string
	profileEmail string
	profileKind  string

	// executorOverride lets tests substitute a FakeExecutor for
	// the launch seam. Nil in production — pickExecutor falls
	// back to executor.Pick, so the real local/slurm dispatch is
	// untouched and no constructor needs to set this. The whole
	// SLURM dispatch→reap→host-commit loop is cluster-free
	// testable purely by setting this field.
	executorOverride func(kind string) (executor.Executor, error)
}

// pickExecutor resolves the launcher for an executor kind. Tests
// inject a FakeExecutor via executorOverride; production leaves
// it nil and gets executor.Pick (local fork / sbatch). Single
// chokepoint so kickoff, the slurm reaper, and CancelRunWrappers
// all honor the same (overridable) resolution.
func (s *FatClient) pickExecutor(kind string) (executor.Executor, error) {
	if s.executorOverride != nil {
		return s.executorOverride(kind)
	}
	return executor.Pick(kind)
}

// New constructs a FatClient. Logger defaults to slog.Default() when
// the caller didn't supply one — service helpers always have
// somewhere to log without a nil check at every call site.
//
// When both Workspace and ProjectRegistry are configured, the
// registry is attached to the workspace at construction so that
// ForProject / ProjectDir / OpenExisting can resolve project
// paths directly. The registry is the durable per-machine
// "project N → home path" record; the workspace consults it
// each call rather than maintaining its own mirror.
// AttachRegistry is a nil-safe no-op when either side is
// unconfigured (test fixtures, embeddings without a registry).
func New(cfg Config) *FatClient {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	fc := &FatClient{
		coord:           cfg.Coord,
		modelName:       cfg.ModelName,
		logger:          logger,
		projectRegistry: cfg.ProjectRegistry,
	}
	// Open the enjugit workspace at the configured root. Logs (but
	// doesn't fail) when construction errors — fat-client flows
	// that don't touch git stay usable.
	if cfg.WorkspaceRoot != "" {
		opts := []enjugit.Option{enjugit.WithLogger(logger)}
		if cfg.ProjectRegistry != nil {
			opts = append(opts, enjugit.WithRegistry(cfg.ProjectRegistry))
		}
		if cfg.LogName != "" {
			opts = append(opts, enjugit.WithLogName(cfg.LogName))
		}
		ws, err := enjugit.NewWorkspace(cfg.WorkspaceRoot,
			enjugit.NewProductionConventions(), opts...)
		if err != nil {
			logger.Warn("enjugit workspace init failed; new-API call sites will be unavailable",
				"root", cfg.WorkspaceRoot, "error", err)
		} else {
			fc.enjugit = ws
		}
	}
	return fc
}

// ProjectRegistry returns the per-machine registry the FatClient
// reads/writes for project-machine bindings. Nil when no
// registry was supplied at construction (test fixtures, hosted
// read-only setups).
func (s *FatClient) ProjectRegistry() *projectreg.Registry { return s.projectRegistry }

// RegisterProject upserts a registry entry. Called from the
// project-creation path (EagerInitProjectClone) so the UI's
// cross-project landing finds the project on next render,
// including external dirs that aren't discoverable from the
// workspace root.
//
// No-op when no registry is configured.
func (s *FatClient) RegisterProject(e projectreg.Entry) {
	if s.projectRegistry == nil {
		return
	}
	if err := s.projectRegistry.Upsert(e); err != nil {
		s.logger.Warn("project registry upsert failed",
			"id", e.ID, "path", e.LocalPath, "error", err)
	}
}

// TouchProject bumps LastTouched for an existing entry.
// Idempotent — no-op if the entry doesn't exist or no registry
// is configured. Wired into ClaimTask (claim.go) and
// SubmitTaskResult (submit.go), and from the handleCreateRun
// MCP handler. Drives "recently active project" sorting on the
// cross-project landing.
func (s *FatClient) TouchProject(id int64) {
	if s.projectRegistry == nil {
		return
	}
	if err := s.projectRegistry.Touch(id); err != nil {
		s.logger.Warn("project registry touch failed",
			"id", id, "error", err)
	}
}

// UnregisterProject drops the entry. Called from
// LocalLeaveProject after the local clone has been removed.
// No-op when no registry is configured.
func (s *FatClient) UnregisterProject(id int64) {
	if s.projectRegistry == nil {
		return
	}
	if err := s.projectRegistry.Remove(id); err != nil {
		s.logger.Warn("project registry remove failed",
			"id", id, "error", err)
	}
}

// Coord returns the underlying coord HTTP client. Exposed for
// callers that need to issue raw requests not yet wrapped by a
// FatClient method.
func (s *FatClient) Coord() *coord.Client { return s.coord }

// Enjugit returns the underlying enjugit Workspace. Mode-check
// guard for "MCP client mode" — nil means no on-disk workspace
// configured (test fixtures with coord-only setup).
func (s *FatClient) Enjugit() *enjugit.Workspace { return s.enjugit }

// SweepStaleScratchAtStartup removes any leftover compute-task
// scratch directories under THIS bot's subtree of the project's
// .enju/ tree. Intended for bot startup — a daemon that exited
// mid-task (crash, kill, OOM) leaves scratch dirs whose owning
// task no longer runs; the next daemon invocation should clear
// them so disk doesn't slowly fill with orphans across restarts.
// No-op when the workspace or coord identity isn't configured
// (MCP-client-only mode, test fixtures).
//
// Project-scoped now (post-Phase-8): the daemon is bound to a
// single project, and bot scratch lives under
// <projectRoot>/.enju/bots/<bot>/scratch/. Two replicas of the
// same bot on one machine still don't clobber each other —
// replica-A's startup only touches its own subdir, leaving
// replica-B's live scratch alone. See
// compute.SweepStaleScratchAtStartup for the safety invariant.
//
// Returns (count_removed, first_error_or_nil). Caller may log
// and continue on error — a partial sweep is harmless.
func (s *FatClient) SweepStaleScratchAtStartup(ctx context.Context, projectID int64) (int, error) {
	if s.enjugit == nil {
		return 0, nil
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return 0, err
	}
	return compute.SweepStaleScratchAtStartup(wf.ProjectRoot(), s.coord.Username())
}

// SweepRunStateDirsForProject removes per-run on-disk state
// directories (<projectRoot>/.enju/runs/<seq>-<slug>/) for runs
// the coordinator considers terminal (completed / failed /
// terminated). Called at bot startup so snapshot dirs from
// runs that finished while the previous daemon was down don't
// accumulate.
//
// "Alive" set is fetched via ListRuns and filtered to
// state ∉ {completed, failed, terminated}. Any on-disk per-run
// dir whose seq isn't in that set is removed.
//
// Returns (removed, error). Errors don't abort caller startup —
// the daemon logs and continues; a stale snapshot dir is dead
// weight, not a correctness hazard.
//
// No-op when:
//   - workspace isn't set (MCP-client-only mode)
//   - ListRuns fails (coord unreachable at boot) — log + skip
//     so the sweep doesn't WIDOW the daemon
//   - the project's .enju/runs/ dir doesn't exist yet
func (s *FatClient) SweepRunStateDirsForProject(ctx context.Context, projectID int64) (int, error) {
	if s.enjugit == nil {
		return 0, nil
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return 0, err
	}
	runs, err := s.ListRuns(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("list runs for sweep: %w", err)
	}
	alive := make(map[int]bool, len(runs))
	for _, r := range runs {
		switch r.State {
		case "completed", "failed", "terminated":
			// terminal — eligible for sweep
		default:
			alive[r.Seq] = true
		}
	}
	return compute.SweepRunStateDirs(wf.ProjectRoot(), alive)
}

// PrepareLLMClaimCWD creates the per-claim ephemeral working
// directory for an LLM task's handler invocation and
// materializes the iter-branch tip's whole tree into it, then
// rebinds the task's declared reads_artifacts to their producing
// commits (declaredReads, each {path, producing commit SHA} as
// recorded by the artifact index). The whole-tree pass keeps a
// project-shaped CWD so existing relative-path prompts work; the
// overlay guarantees declared inputs are the run's own producing
// output, never a stale tree's prior-run bytes. declaredReads nil
// is the pre-overlay behavior (bulk tree only).
//
// Path shape: <wsRoot>/scratch/<botUsername>/<taskID>-iter-<N>/
// (the same path compute scratch already uses, so the unified
// startup sweep + age-filter mechanics from Phase 5 apply
// without per-bot-type duplication).
//
// Why materialize the whole iter-branch tree? The LLM's tools
// (Read, Write, Bash) expect a project-shaped CWD. Existing
// prompts use relative paths like `Read goal.md` or
// `cd src && go build ./...`. Materializing just declared
// reads_artifacts would break those — the LLM's context shrinks
// to the declared list only. The whole-tree path keeps
// existing prompts working. Per-claim cost is bounded by
// repo size (seconds of I/O for typical projects).
//
// FUTURE OPTIMIZATION (review fix #2): on filesystems that
// support reflink / copy-on-write (btrfs, ZFS, APFS, recent
// XFS), the materialization could hardlink-or-reflink rather
// than copy file-by-file. The first-pass cost would drop to
// constant time. Hasn't been measured against a 1GB+ repo
// yet — that benchmark is the gate for considering the
// optimization. For now the straight read-write loop in
// MaterializeRunRepo keeps the code path simple.
//
// COST FLAG (review fix #6): each call resolves the project's
// Workflow via OpenWorkflow, which on first hit per project
// opens the git store and acquires the per-project flock. The
// workspace layer caches the open across subsequent calls so
// repeated claims within one bot daemon amortize. Cross-
// project bots pay the open cost once per project; high-fleet
// configurations (one daemon claiming across hundreds of
// projects) would feel it. Not measured against such a fleet
// yet — flag if real workloads surface latency here.
//
// iterBranch is the per-iteration topic branch the coordinator
// assigned at claim time. For the very first iter-N claim it
// has a NAME but no git ref yet (the ref gets created lazily
// at submit time via prepareBranchForCommit). runBranch is the
// fallback materialization source for that case — it's the
// fork base for iter-N by definition, and EnsureRunBranch
// guarantees its local ref exists.
//
// Branch-selection priority:
//  1. iterBranch when its local ref exists → exact iteration state
//     (re-claim after request_changes: the prior iteration's tree).
//  2. runBranch → fork base for iter-1's first claim, or fallback
//     when iter branch hasn't been refreshed locally.
//  3. Both empty → return ("", nil); caller falls back to the
//     persistent worktree path.
//
// Errors are unrecoverable filesystem failures (mkdir denied,
// git object missing). Callers log + fall back to the
// persistent worktree to keep workflows progressing under
// transient i/o trouble.
func (s *FatClient) PrepareLLMClaimCWD(ctx context.Context, projectID int64, botUsername, taskID string, iter int, iterBranch, runBranch, baseSHA string, declaredReads []enjugit.ArtifactRef) (string, error) {
	if s.enjugit == nil || botUsername == "" || taskID == "" {
		return "", nil
	}
	if iterBranch == "" && runBranch == "" {
		// No branch context at all (legacy / pre-iteration with
		// no run branch threaded through). Caller falls back to
		// the persistent worktree path.
		return "", nil
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return "", err
	}
	path := compute.ResolveTaskScratchDir(wf.ProjectRoot(), botUsername, taskID, iter)
	if path == "" {
		return "", nil
	}
	// Pick the branch whose ref actually exists locally. Iter
	// branch has no ref on first iter-N claim (coord-assigned
	// name, ref deferred until submit); run branch is the fork
	// base and is guaranteed live by EnsureRunBranch at run create.
	//
	// CRITICAL: an iter-branch ref EXISTING is not enough to trust
	// it. Iter-branch names are `<run-slug>/<task>/iter-N` and
	// COLLIDE across different runs that share a slug (every run #1
	// of a given workflow gets the same slug; slugs recur across
	// coord wipes). A same-named ref left by a PRIOR run points at
	// that run's (possibly stale) tree — materializing from it
	// silently runs the wrong repo state (e.g. pre-rename prompt
	// files), breaking run reproducibility for agent claims.
	//
	// Trust test: the iter branch must descend from THIS run's
	// pinned base commit. A genuine in-run iteration branch is
	// `baseSHA + this run's submit`, so baseSHA is in its history.
	// This catches the HIGH-severity class — a prior run whose
	// HEAD has since advanced has a DIFFERENT baseSHA, so its
	// leftover same-named ref does not descend from this run's
	// baseSHA (covers both flavors: merged into that prior run, or
	// diverged from a failed/terminated one). The ancestor-of-
	// runBranch heuristic caught only the merged flavor; baseSHA-
	// descent catches both of these.
	//
	// KNOWN RESIDUAL (lower severity): two runs created from an
	// UNCHANGED project HEAD share the same baseSHA (coord
	// wiped+rerun without a project edit — the routine coord-vs-
	// disk drift case). A prior run's leftover ref then DOES
	// descend from the shared base, so this test trusts it and
	// leaks that run's iteration edits. It is not the high-
	// severity bug (same base ⇒ same project tree, so no stale
	// pre-rename project code — that needs HEAD to move, which
	// changes baseSHA and IS caught). The true cure is structural:
	// iter-branch names are keyed on the recurring slug, a
	// colliding namespace; run-unique names would close it
	// entirely. baseSHA-descent is a detection layer over that
	// namespace, not the fix for it — see [[project_coord_vs_disk_split]].
	//
	// baseSHA is empty for inline-yaml runs: those are UNPINNED by
	// design (no source commit to be reproducible against), so
	// there is nothing to anchor to — fall back to the prior
	// best-effort heuristic (reject only an already-integrated
	// ancestor ref). Whenever the iter branch is rejected, fall
	// back to runBranch — the pinned, reproducible source the
	// compute path also uses.
	materializeFrom := iterBranch
	if materializeFrom != "" {
		iterSHA, _ := wf.LocalBranchHash(materializeFrom)
		if iterSHA == "" {
			materializeFrom = ""
		} else if baseSHA != "" {
			// Correct anchor: trust the iter branch only if this
			// run's pinned base commit is in its ancestry.
			if anc, _ := wf.IsAncestor(baseSHA, iterSHA); !anc {
				materializeFrom = ""
			}
		} else if runBranch != "" {
			// Inline-yaml fallback (no baseSHA): best-effort —
			// reject a same-named ref that is already-integrated
			// history (ancestor of the run branch).
			if runSHA, _ := wf.LocalBranchHash(runBranch); runSHA != "" {
				if anc, _ := wf.IsAncestor(iterSHA, runSHA); anc {
					materializeFrom = ""
				}
			}
		}
	}
	if materializeFrom == "" {
		materializeFrom = runBranch
	}
	if materializeFrom == "" {
		return "", nil
	}
	// MaterializeRunRepo creates the target dir + walks the
	// branch's tree into it. Idempotent on existing files
	// (writes are file-by-file).
	if _, merr := wf.MaterializeRunRepo(materializeFrom, path); merr != nil {
		return "", fmt.Errorf("materialize claim CWD from %q: %w", materializeFrom, merr)
	}
	// Rebind declared reads to their producing commits. The bulk
	// tree above comes from a LOCAL branch ref (or, for handlers
	// that read $ENJU_REPO_DIR, the create-time frozen snapshot) —
	// both can lag the upstream's just-merged output, so a tracked
	// path an upstream RE-produced this run can still hold a PRIOR
	// run's bytes in that tree. Overlaying each declared read from
	// the producing commit the artifact index recorded makes the
	// on-disk inputs deterministic + run-scoped, exactly matching
	// the inlined-prompt path. Best-effort: a failure leaves the
	// (possibly stale) bulk copy rather than dropping the whole CWD
	// back to the persistent worktree, which is staler still.
	if len(declaredReads) > 0 {
		if n, oerr := wf.OverlayDeclaredReads(path, declaredReads); oerr != nil {
			s.logger.Warn("overlay declared reads onto claim CWD failed; declared inputs may reflect a stale tree",
				"task_id", taskID, "overlaid", n, "error", oerr)
		}
	}
	return path, nil
}

// CleanupLLMClaimCWD applies the Phase-5-style lifecycle:
// successful submits remove the CWD; failed submits preserve
// it so the operator's retry can pick up the LLM's work from
// disk. Stale preserves age out via the startup sweep at
// StaleScratchAgeThreshold.
//
// Empty path is a no-op (caller fell back to the persistent
// worktree).
func (s *FatClient) CleanupLLMClaimCWD(path string, successful bool) {
	if path == "" {
		return
	}
	if !successful {
		s.logger.Warn("submit failed; preserving LLM claim CWD for retry",
			"path", path)
		return
	}
	if err := os.RemoveAll(path); err != nil {
		s.logger.Warn("LLM claim CWD cleanup failed",
			"path", path, "error", err)
	}
}

// RunSnapshotDir returns the absolute path to a run's on-disk
// snapshot dir (<projectRoot>/.enju/runs/<seq>-<slug>/snapshot/).
// Exposed so the bot daemon can thread the path into the
// handler subprocess as $ENJU_REPO_DIR — handlers read frozen
// project content from there.
//
// Returns ("", nil) when the workspace isn't configured
// (test fixtures, MCP-client-only mode); the daemon treats
// empty as "no $ENJU_REPO_DIR exported."
//
// Does NOT verify the directory exists on disk — create_run is
// what materializes it, and we want this helper to be cheap
// (no stat) so the daemon's claim hot-path doesn't pay for
// per-task filesystem checks. A missing snapshot dir is the
// handler's problem to surface at runtime, with the same env
// var as a breadcrumb pointing operators at where to look.
func (s *FatClient) RunSnapshotDir(ctx context.Context, projectID int64, runSeq int, runSlug string) (string, error) {
	if s.enjugit == nil {
		return "", nil
	}
	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil || wf == nil {
		return "", err
	}
	root := wf.ProjectRoot()
	if root == "" {
		return "", nil
	}
	return filepath.Join(root, corelayout.RunSnapshotOnDiskDir(runSeq, runSlug)), nil
}

// Username delegates to the coord client so callers see live values
// across auto-reregister rotations.
func (s *FatClient) Username() string { return s.coord.Username() }

// ModelName returns the process-default model identifier (the
// `-model` flag the MCP client was launched with).
func (s *FatClient) ModelName() string { return s.modelName }

// Logger returns the FatClient's logger. Service helpers and the
// handlers that wrap them share this logger.
func (s *FatClient) Logger() *slog.Logger { return s.logger }

// EffectiveModel returns the model identifier to attribute a single
// action to. If the caller passed an explicit override (the per-call
// `model` argument on submit / submit_results_batch), use it.
// Otherwise fall back to the process default — the `-model` flag the
// MCP client was launched with.
//
// The override path is what makes mixed-model workflows work without
// restarting MCP.
func (s *FatClient) EffectiveModel(override string) string {
	if override != "" {
		return override
	}
	return s.modelName
}

// CommitAuthor returns the `name email` pair to use as git commit
// author for submits made on this citizen's behalf. Fetches the
// citizen profile from the coordinator once and caches it for the
// life of the FatClient. Falls back to the configured display name
// when no profile is available, and to a synthetic
// `{username}@enju.local` address when no real email is set.
//
// Real email addresses attribute commits to the right GitHub user
// when they match the citizen's GitHub email; synthetic ones at
// least make different citizens' commits distinguishable in
// contributor graphs instead of collapsing to one bot identity.
func (s *FatClient) CommitAuthor(ctx context.Context) (name, email string) {
	s.loadProfile(ctx)
	return s.profileName, s.profileEmail
}

// CitizenKind returns the calling citizen's kind ("human" | "bot" |
// "model"), populated lazily through the same one-shot fetch as
// CommitAuthor. Defaults to "human" on lookup failure or unmigrated
// rows where Kind is empty server-side.
func (s *FatClient) CitizenKind(ctx context.Context) string {
	s.loadProfile(ctx)
	if s.profileKind == "" {
		return string(types.CitizenKindHuman)
	}
	return s.profileKind
}

// loadProfile fetches the citizen profile once and stashes the
// fields we care about on FatClient. Shared by CommitAuthor and
// CitizenKind so a single GET populates both. Safe to call
// repeatedly — sync.Once gates the network.
func (s *FatClient) loadProfile(ctx context.Context) {
	s.profileOnce.Do(func() {
		username := s.coord.Username()
		s.profileName = username
		s.profileEmail = username + "@enju.local"
		s.profileKind = string(types.CitizenKindHuman)

		data, err := s.coord.Get(ctx, "/api/v1/citizens/by-username/"+username)
		if err != nil {
			s.logger.Warn("loadProfile: failed to fetch profile, using defaults",
				"username", username, "error", err)
			return
		}
		var p map[string]interface{}
		if err := json.Unmarshal(data, &p); err != nil {
			return
		}
		if n, ok := p["name"].(string); ok && n != "" {
			s.profileName = n
		}
		if e, ok := p["email"].(string); ok && e != "" {
			s.profileEmail = e
		}
		if k, ok := p["kind"].(string); ok && k != "" {
			s.profileKind = k
		}
	})
}
