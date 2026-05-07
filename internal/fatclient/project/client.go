// Package workspace provides the client-side git operations used by the
// MCP server (and any other fat client). It owns a local clone per
// project, writes task results and artifacts into the working tree in
// the standard enju layout, commits with the standard commit message
// format, and pushes to the project's configured remote using the
// user's ambient git credentials.
//
// This package is the "fat client" half of the coordinator-as-
// orchestrator architecture. The coordinator never invokes anything
// here — it only handles metadata. All git-touching code lives on the
// client side and runs under the user's own identity and credentials.
//
// Design points:
//
//   - One Workspace per MCP client process. Workspace roots under a
//     user-chosen directory (default `~/.enju/workspaces`) and
//     lazily clones projects on first access.
//
//   - Each project has its own local clone. Multiple projects are
//     fully independent. Switching between projects never requires
//     re-cloning or stashing.
//
//   - Writes are atomic at the commit level: one submit = one
//     commit. Result files and artifact files written in the same
//     submit land in a single commit so iteration 3.1's rollback
//     walker (and any future provenance tooling) can treat a commit
//     as the finest unit of task output.
//
//   - Push uses a fetch-reset-overlay-commit-push loop that handles
//     non-fast-forward cases without needing git rebase semantics.
//     The working tree is a cache; if the remote has advanced, we
//     refresh the local copy and re-apply our writes on top.
//
//   - Commit messages follow the format iteration 3.1's walker
//     expects:
//
//     Task {taskID} by @{username}: result
//     Task {taskID} by @{username}: result + N artifact(s)
//
//     Artifacts: path1, path2, ...
//
//     (Subject line matches `commitTaskSubjectRe` in the coordinator's
//     legacy rollback code; future iterations will replace the walker
//     with DB-only invalidation and drop this constraint, but for
//     the coexistence period the format stays stable.)
package project

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/gofrs/flock"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

// Opener manages local clones of projects. Callers create one at
// MCP client startup and re-use it for the lifetime of the
// process — it holds the open *Clone cache and the cross-process
// flock, both of which need a single owner per fat-client.
//
// Project paths come from the projectreg.Registry when one is
// attached via AttachRegistry. The registry is the single source
// of truth for "where does project N live on disk?" — every
// project home is registered explicitly at `enju_create_project`
// + `enju_init` time (both require `path=`), so there's no
// in-memory cache to keep in sync.
type Opener struct {
	rootDir  string
	logger   *slog.Logger
	registry *projectreg.Registry

	mu      sync.Mutex
	clients map[int64]*Clone // projectID → open project clone
}

// NewWorkspace creates (or reuses) a workspace rooted at the given
// directory. The directory is created with 0755 perms if missing.
// Pass an empty string to default to `$HOME/.enju/workspaces`.
//
// Attach a projectreg.Registry via AttachRegistry to enable path
// resolution for `ForProject`. Without it, ForProject only handles
// the IsLocalWorkingTree(remoteURL) and clone-from-remote paths —
// fine for the bot daemon which uses OpenBotCloneAt, but operator-
// side flows (MCP / webui) need the registry attached at startup.
func NewOpener(rootDir string, logger *slog.Logger) (*Opener, error) {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving home dir: %w", err)
		}
		rootDir = filepath.Join(home, ".enju", "workspaces")
	}
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("creating workspace root: %w", err)
	}
	return &Opener{
		rootDir: rootDir,
		logger:  logger,
		clients: make(map[int64]*Clone),
	}, nil
}

// AttachRegistry wires the projectreg.Registry that ForProject and
// ProjectDir consult for path resolution. Replaces the previous
// `RegisterExternalDir` + `externalDirs` map model — the registry
// is the durable record, no need for an in-memory shadow.
//
// Called from service.New() once at construction; the bot daemon's
// Workspace doesn't need it (OpenBotCloneAt takes paths explicitly).
func (ws *Opener) AttachRegistry(reg *projectreg.Registry) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.registry = reg
}

// projectHome looks up the project's working-tree path from the
// attached registry. Returns "" when no registry is attached or
// the project isn't registered. Caller must hold ws.mu.
func (ws *Opener) projectHome(projectID int64) string {
	if ws.registry == nil {
		return ""
	}
	entry, err := ws.registry.Get(projectID)
	if err != nil || entry == nil {
		return ""
	}
	return entry.LocalPath
}

// RootDir returns the directory that holds per-project clones.
func (ws *Opener) RootDir() string { return ws.rootDir }

// projectDir returns the on-disk path for one project's local clone.
// When projectName is non-empty, the directory is named "{slug}-{id}"
// (e.g. "battle-test-7") for human readability. When empty, falls
// back to the numeric ID (e.g. "7").
//
// If a legacy numeric-only directory exists and a name is now known,
// projectDir renames it to the slug form so existing clones survive.
func (ws *Opener) projectDir(projectID int64, projectName string) string {
	numericDir := filepath.Join(ws.rootDir, fmt.Sprintf("%d", projectID))
	if projectName == "" {
		// Caller didn't tell us the name, but a previous open
		// (e.g. via a different code path with the name) may have
		// already created a slug-id dir on disk. Use that if it
		// exists so we don't fork a parallel numeric dir for the
		// same project — the path-divergence class of bugs the
		// project↔enjugit migration keeps surfacing.
		if existing := ws.findProjectDir(projectID); existing != "" {
			return existing
		}
		return numericDir
	}
	slug := slugify(projectName)
	if slug == "" {
		return numericDir
	}
	namedDir := filepath.Join(ws.rootDir, fmt.Sprintf("%s-%d", slug, projectID))
	// Migrate: if the old numeric dir exists but the named dir
	// doesn't, rename for a seamless upgrade.
	if _, err := os.Stat(numericDir); err == nil {
		if _, err := os.Stat(namedDir); os.IsNotExist(err) {
			_ = os.Rename(numericDir, namedDir)
		}
	}
	return namedDir
}

// slugify turns a project name into a filesystem-safe slug:
// lowercase, non-alphanumeric runs replaced with a single hyphen,
// trimmed of leading/trailing hyphens.
func slugify(name string) string {
	var b strings.Builder
	prevDash := true // suppress leading dash
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// ProjectDir returns the on-disk working directory for a project.
// Registry wins (the authoritative source for "where N lives");
// falls back to a slug/numeric scan of the opener root for
// integration tests that build a project via the coord directly
// without going through the handler's EagerInitProjectClone
// (which writes to the registry). Read-only — does not trigger
// a clone.
func (ws *Opener) ProjectDir(projectID int64) string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if home := ws.projectHome(projectID); home != "" {
		return home
	}
	return ws.findProjectDir(projectID)
}

// findProjectDir locates the on-disk clone directory for a project,
// checking both the slug-based ("{slug}-{id}") and legacy numeric
// ("{id}") naming conventions. Returns empty string if no clone
// exists. This is used by callers that don't know the project name.
//
// Tie-break: prefer slug-form when both exist. The numeric form is
// legacy plus a known accidental-init shape (a buggy read-only
// caller using ForProject(id, "") could create an empty
// "{rootDir}/{id}" stub). Alphabetical os.ReadDir order would
// otherwise return the numeric stub before the real slug clone.
// Two-pass: collect both candidates, return slug if present.
func (ws *Opener) findProjectDir(projectID int64) string {
	suffix := fmt.Sprintf("-%d", projectID)
	numericName := fmt.Sprintf("%d", projectID)
	entries, err := os.ReadDir(ws.rootDir)
	if err != nil {
		return ""
	}
	var slugMatch, numericMatch string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		gitDir := filepath.Join(ws.rootDir, name, ".git")
		st, err := os.Stat(gitDir)
		if err != nil || !st.IsDir() {
			continue
		}
		switch {
		case name == numericName:
			numericMatch = filepath.Join(ws.rootDir, name)
		case strings.HasSuffix(name, suffix):
			slugMatch = filepath.Join(ws.rootDir, name)
		}
	}
	if slugMatch != "" {
		return slugMatch
	}
	return numericMatch
}

// OpenBotCloneAt opens (or clones) the bot's managed clone at an
// explicit caller-supplied path, sourcing from the supplied URL.
// Used by service.ResolveBotWorkspace, where the clone lives at
// `<projectHome>/enju/bots/<botUsername>/clone/` (per-bot, so
// parallel bots on the same project don't collide on working-tree
// state) and sources from the per-project bare at
// `<projectHome>/enju/.bare.git/` (or a real github remote).
// Bot-flow only — operator-side reads go through ForProject.
//
// clonePath and sourceURL are both required. The clone is opened
// in-place if it already exists (and origin matches sourceURL),
// otherwise PlainClone'd from sourceURL. Cross-process operations
// against the same projectID coordinate via a flock at
// `<filepath.Dir(clonePath)>/.bot-clone.lock` — local to the
// project, doesn't depend on ws.rootDir at all.
//
// Cache: the opener's `clients[id]` map is keyed by projectID.
// A bot daemon process that started by calling ForProject (e.g.
// some legacy startup path) might have populated the cache with
// the operator's tree handle; if we see a stale entry at a
// different workDir, evict it before opening the bot's clone.
// In practice the daemon never calls ForProject, so this branch
// only fires defensively.
func (ws *Opener) OpenBotCloneAt(projectID int64, clonePath, sourceURL string) (*Clone, error) {
	if projectID == 0 {
		return nil, fmt.Errorf("projectID is required")
	}
	if clonePath == "" {
		return nil, fmt.Errorf("OpenBotCloneAt requires clonePath (project %d)", projectID)
	}
	if sourceURL == "" {
		return nil, fmt.Errorf("OpenBotCloneAt requires sourceURL (project %d)", projectID)
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	if p, ok := ws.clients[projectID]; ok {
		if p.workDir == clonePath {
			return p, nil
		}
		ws.logger.Info(
			"OpenBotCloneAt: evicting cached project at different path",
			"project_id", projectID,
			"cached_workdir", p.workDir,
			"requested_workdir", clonePath,
		)
		delete(ws.clients, projectID)
	}

	p, err := openOrClone(clonePath, sourceURL, ws.logger)
	if err != nil {
		return nil, err
	}
	p.projectID = projectID
	// Lock lives next to the clone, inside the project's enju/
	// dir. Per-project, doesn't collide with anything else.
	lockPath := filepath.Join(filepath.Dir(clonePath), ".bot-clone.lock")
	p.fileLock = flock.New(lockPath)
	ws.clients[projectID] = p
	return p, nil
}

// HasExternalDir returns true if the given project has a
// registered home path (from enju_init or enju_create_project).
// Wraps the registry lookup; named for backward compatibility
// with tests that pre-date the registry-bridge unification.
func (ws *Opener) HasExternalDir(projectID int64) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.projectHome(projectID) != ""
}

// EvictProjectCache drops the cached *Clone for projectID (if any),
// briefly holding the project lock to barrier in-flight submits.
// On-disk removal lives on the enjugit Workspace; service-level
// LeaveProject orchestration calls this for project-side cache
// eviction, then enjugit.Workspace.LeaveProject for the dir wipe.
// Goes away with the project package.
func (ws *Opener) EvictProjectCache(projectID int64) {
	if projectID == 0 {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if p, ok := ws.clients[projectID]; ok {
		p.mu.Lock()
		p.mu.Unlock() //nolint:staticcheck // barrier against in-flight writers
		delete(ws.clients, projectID)
	}
}

// ForProject returns a handle to the local clone of the given project,
// cloning it from remoteURL on first access. Subsequent calls for the
// same projectID return the cached handle. If remoteURL is empty, the
// project is treated as a local-only clone (no origin, no push) and
// must already exist on disk — this supports a future degenerate mode
// where a self-hosted user keeps everything in a local working tree
// without a remote at all.
//
// projectName is optional — when provided, the on-disk directory uses
// a human-readable "{slug}-{id}" format (e.g. "battle-test-7") instead
// of a bare numeric id. Pass "" when the name isn't available.
//
// READ-ONLY callers (any path that just wants to read git data —
// ReadResultAtCommit, BuildInbox, UI views) should call
// OpenExisting instead. ForProject's clone/init path can
// silently create an orphan stub when remoteURL=="" and the
// computed numeric path doesn't match the actual slug-form
// clone on disk; OpenExisting refuses to materialize state.
func (ws *Opener) ForProject(projectID int64, remoteURL string, projectName ...string) (*Clone, error) {
	if projectID == 0 {
		return nil, fmt.Errorf("projectID is required")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	// LOAD-BEARING cache hit: bot daemons rely on this returning
	// the bot-clone Project that OpenBotCloneAt seeded during the
	// pre-warm step in bots/daemon.go runOnce. If this branch
	// ever stops returning the cached entry (e.g. "validate
	// before return", "always re-resolve when remoteURL looks
	// remote"), the bot's downstream OpenProject calls fall
	// through to the externalDirs short-circuit below and
	// silently route to the operator's tree. The pre-warm dance
	// only works because cache-hit-wins. See bots/daemon.go
	// runOnce comment ("Pre-warm the bot's managed clone...") for
	// the inverse half of this contract.
	if p, ok := ws.clients[projectID]; ok {
		return p, nil
	}

	// Registry path resolution: the projectreg.Registry holds
	// the durable "project N lives at /path" mapping. When a
	// registry is attached (operator-side flows always do this
	// at New()), the project's working tree lives at that path.
	if home := ws.projectHome(projectID); home != "" {
		p, err := openOrClone(home, "", ws.logger)
		if err != nil {
			return nil, err
		}
		p.projectID = projectID
		ws.clients[projectID] = p
		return p, nil
	}

	// Detect local working tree: if remoteURL is a local path
	// with a .git dir (not bare), open it directly instead of
	// cloning. Defensive — covers callers with no registry
	// attached but a local-path remoteURL (rare; bot daemon's
	// resolveBare flow doesn't use this entry point).
	if remoteURL != "" && enjugit.IsLocalWorkingTree(remoteURL) {
		p, err := openOrClone(remoteURL, "", ws.logger)
		if err != nil {
			return nil, err
		}
		p.projectID = projectID
		ws.clients[projectID] = p
		return p, nil
	}

	name := ""
	if len(projectName) > 0 {
		name = projectName[0]
	}
	workDir := ws.projectDir(projectID, name)
	p, err := openOrClone(workDir, remoteURL, ws.logger)
	if err != nil {
		// No extra "opening project N:" wrap — openOrClone's
		// inner error (from friendlyGitError or
		// PlainInit/PlainOpen) already carries enough context,
		// and the caller in server.go adds "opening local
		// clone:" on top.
		return nil, err
	}
	// openOrClone is project-agnostic; stamp the projectID in so
	// path helpers (ArtifactPath, etc.) can namespace by project.
	p.projectID = projectID
	// Attach the cross-process file lock. Lives at the workspace
	// root (one level above the clone dir) so it survives
	// LeaveProject's RemoveAll(workDir) and doesn't risk being
	// stepped on by go-git operations inside the clone. A
	// workspace shared across multiple MCP processes will converge
	// on the same lock path for the same projectID.
	lockPath := filepath.Join(ws.rootDir, fmt.Sprintf("project-%d.lock", projectID))
	p.fileLock = flock.New(lockPath)
	ws.clients[projectID] = p
	return p, nil
}

// OpenExisting returns a handle to an already-on-disk project
// clone. Resolves via findProjectDir (handles slug-form,
// numeric-form, and externally-registered paths). Returns
// ErrCloneNotFound when no clone exists — never inits, never
// clones.
//
// Use this for read-only callers (ReadResultAtCommit,
// BuildInbox, UI views) that should fail fast rather than
// silently materialize an empty stub. ForProject's clone-from-
// remote path is the historical accidental-init source: when
// remoteURL=="" and the computed numeric path doesn't match
// the actual slug-form clone on disk, ForProject would
// PlainInit a fresh empty repo at "{rootDir}/{id}", outranking
// the real clone in subsequent findProjectDir scans.
func (ws *Opener) OpenExisting(projectID int64) (*Clone, error) {
	if projectID == 0 {
		return nil, fmt.Errorf("projectID is required")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	if p, ok := ws.clients[projectID]; ok {
		return p, nil
	}

	// Resolve the on-disk path: registry wins, fallback to
	// findProjectDir's slug/numeric scan for legacy clones not
	// yet registered (rare; tests that build a Workspace
	// directly without a registry attached).
	workDir := ws.projectHome(projectID)
	if workDir == "" {
		workDir = ws.findProjectDir(projectID)
	}
	if workDir == "" {
		return nil, ErrCloneNotFound
	}

	// PlainOpen only — no fallback to init. If the .git dir
	// disappeared between findProjectDir and now, that's a
	// real I/O error worth surfacing.
	repo, err := gogit.PlainOpen(workDir)
	if err != nil {
		return nil, fmt.Errorf("open existing clone at %s: %w", workDir, err)
	}
	// Drain a leftover preserve dir from a prior process's
	// crashed checkout. Mirrors openOrClone's recovery hook
	// so read-only callers (inbox, UI, ReadResultAtCommit)
	// also clean up — without this rung, a preserve dir from
	// a webui-side crash sat around forever because the next
	// open went through OpenExisting, not openOrClone.
	if err := enjugit.RecoverLeftoverSharedPreserve(workDir, ws.logger); err != nil && ws.logger != nil {
		ws.logger.Warn("preserve-dir recovery failed during OpenExisting; leaving for manual inspection",
			"error", err, "path", workDir+enjugit.SharedPreserveDirSuffix)
	}
	p := &Clone{
		projectID: projectID,
		workDir:   workDir,
		repo:      repo,
		logger:    ws.logger,
	}
	// Hydrate remoteURL from the on-disk origin so lazy-fetch
	// paths (ReadFileAtCommit on commit-not-found) can self-heal.
	// Without this, the cross-citizen read gap silently re-opens
	// for every OpenExisting'd clone — the bug that left the
	// webui showing "(content unavailable — commit unreachable
	// from this clone)" even though the bare had the commit.
	if rem, err := repo.Remote("origin"); err == nil {
		if cfg := rem.Config(); cfg != nil && len(cfg.URLs) > 0 {
			p.remoteURL = cfg.URLs[0]
		}
	}
	lockPath := filepath.Join(ws.rootDir, fmt.Sprintf("project-%d.lock", projectID))
	p.fileLock = flock.New(lockPath)
	ws.clients[projectID] = p
	return p, nil
}

// ErrCloneNotFound is returned by OpenExisting when no on-disk
// clone exists for the given project. Distinct from a generic
// "open failed" so read-only callers can render a friendly
// "clone not yet materialized" message rather than treating
// the absence as a hard error.
var ErrCloneNotFound = fmt.Errorf("workspace: no clone exists for project")

// Project is a handle to one project's local clone. All writes for
// this project are serialized through a two-layer lock:
//
//   - The in-process sync.Mutex serializes concurrent callers
//     within this Workspace (one goroutine at a time).
//   - The on-disk flock serializes writes across MCP processes
//     that share the same ~/.enju/workspaces/{id}/ directory —
//     common when a citizen has both Claude Desktop and Claude
//     Code running, each spawning its own `enju mcp` stdio
//     process that points at the same workspace root. Without the
//     flock, two processes can race on .git/index.lock and
//     corrupt the clone.
type Clone struct {
	projectID int64
	workDir   string
	remoteURL string
	repo      *gogit.Repository
	logger    *slog.Logger

	// gitClone is the shared enjugit/internal/git handle backing
	// this project.Clone. p.repo is set to gitClone.Repo() so
	// reads/writes through either package land in identical
	// in-memory state — kills the dual-handle drift class of
	// bugs (#381) without requiring every method to migrate at
	// once. Nil only for synthetic test clones built without
	// going through openOrClone (rare).
	gitClone *enjugit.SharedClone

	mu       sync.Mutex   // in-process serialization
	fileLock *flock.Flock // cross-process serialization (optional)

	// defaultBranch is the branch Pull/Push operate on when the
	// caller doesn't specify one, and the fallback target when
	// CheckoutBranch needs to create a fresh branch from a
	// starting point. Set via SetDefaultBranch when the
	// workspace layer has the coordinator's record; defaults to
	// "main" so bare-repo initialization and pre-branch-model
	// tests keep working.
	defaultBranch string
}

// defaultBranchOr returns p.defaultBranch when set, "main"
// otherwise. Keeps the "no branch configured yet" case producing
// the historical main-branch behavior so callers that pre-date
// the branch model still work unchanged.
func (p *Clone) defaultBranchOr() string {
	if p.defaultBranch == "" {
		return "main"
	}
	return p.defaultBranch
}

// resolveBranch picks the branch a specific op should use: the
// explicit override when non-empty, falling back to the
// project's default. Central point so every call site stays
// consistent.
func (p *Clone) resolveBranch(override string) string {
	if override != "" {
		return override
	}
	return p.defaultBranchOr()
}

// SetDefaultBranch configures the fallback branch for git ops
// that don't take a branch override. Usually called by the
// workspace layer right after ForProject, using the value from
// the coordinator's project record. Idempotent; no-op on empty
// input so callers that don't know the branch yet can leave it
// untouched.
func (p *Clone) SetDefaultBranch(branch string) {
	if branch == "" {
		return
	}
	p.defaultBranch = branch
}

// DefaultBranch returns the currently configured default
// branch, or "main" if none was set.
func (p *Clone) DefaultBranch() string {
	return p.defaultBranchOr()
}

// ProjectID returns the coordinator-assigned project ID this clone
// belongs to.
func (p *Clone) ProjectID() int64 { return p.projectID }

// WorkDir returns the local working-tree path.
func (p *Clone) WorkDir() string { return p.workDir }

// GitClone returns the underlying enjugit git handle. Exposed so
// callers can drive enjugit-side verbs (PullBranch, etc.) against
// the SAME *git.Clone the project.Clone wraps — avoiding the
// dual-handle staleness bug where two independent git.Clone
// instances over the same .git see each other's pack files only
// after re-open.
func (p *Clone) GitClone() *enjugit.SharedClone { return p.gitClone }

// RemoteURL returns the configured origin URL, or an empty string
// for local-only clones.
func (p *Clone) RemoteURL() string { return p.remoteURL }

// Lock acquires the per-project write mutex AND the on-disk
// flock. Callers performing a sequence of WriteFile + Commit +
// Push operations must hold this across the whole sequence so
// neither intra-process goroutines nor cross-process MCP
// instances can race on .git/index.lock.
//
// The flock is blocking: if another process is mid-submit against
// the same clone, Lock() waits for it to finish. This matches the
// intuition of "two Claude sessions trying to submit at the same
// time should queue up, not corrupt the clone."
//
// Lock panics if the file lock cannot be acquired for reasons
// other than a peer holding it — e.g. the workspace directory
// became unwritable. This is intentional: silently falling back
// to intra-process-only locking would reintroduce the corruption
// risk the flock exists to prevent.
func (p *Clone) Lock() {
	p.mu.Lock()
	if p.fileLock != nil {
		if err := p.fileLock.Lock(); err != nil {
			p.mu.Unlock()
			panic(fmt.Sprintf("workspace: acquiring project flock at %s: %v",
				p.fileLock.Path(), err))
		}
	}
}

// Unlock releases the flock first, then the in-process mutex. The
// reverse order of Lock, mirroring a standard stacked-lock
// release.
func (p *Clone) Unlock() {
	if p.fileLock != nil {
		_ = p.fileLock.Unlock()
	}
	p.mu.Unlock()
}

// openOrClone opens an existing project clone or creates a fresh one
// by cloning from remoteURL. If workDir exists and is a valid git
// repo, it's opened in place. If it doesn't exist, it's cloned from
// remoteURL. If workDir exists but isn't a repo (or the clone is
// corrupted), it's removed and re-cloned.
func openOrClone(workDir, remoteURL string, logger *slog.Logger) (*Clone, error) {
	gc, err := enjugit.OpenOrCloneShared(workDir, remoteURL, logger)
	if err != nil {
		return nil, err
	}
	return cloneFromGit(gc, logger), nil
}

// cloneFromGit builds a project.Clone façade over an enjugit/git
// Clone. Single construction site so every Clone created via
// openOrClone holds the same field-population pattern. Caller
// fills projectID + fileLock after, since those are bound to the
// Opener's policy, not the underlying git layer.
func cloneFromGit(gc *enjugit.SharedClone, logger *slog.Logger) *Clone {
	return &Clone{
		workDir:   gc.WorkDir(),
		remoteURL: gc.RemoteURL(),
		repo:      gc.Repo(),
		logger:    logger,
		gitClone:  gc,
	}
}

// FileWrite describes one file to write into the working tree as part
// of a single atomic commit. Used by SubmitTaskResult to pack together
// a task's result file(s), metadata, and any artifact writes.
type FileWrite struct {
	// RepoRelPath is the file's path relative to the repo root
	// (e.g., "runs/1/foo/result.md", "artifacts/notes/intro.md").
	RepoRelPath string
	// Content is the raw bytes to write.
	Content []byte
	// Mode is the file permission bits to write with. When
	// zero, the caller doesn't care and we default to 0644.
	// Callers that need executable scripts (template-bundle
	// snapshots, in particular — see ReadBundleFiles) must
	// set this to 0755 so go-git's Worktree.Add picks up the
	// exec bit when building the tree entry, and downstream
	// `git pull` on a fresh clone restores it.
	//
	// Without this, committing a shell script as a 0644 blob
	// means the executor on the other side hits "permission
	// denied" when trying to run it from the snapshot — the
	// exact regression the 2026-04-18 template-bundle pass
	// introduced.
	Mode os.FileMode
}


// readUnmergedFiles parses `git status --porcelain` for entries
// in conflict states (UU/AA/DD/AU/UA/DU/UD), returning the
// affected paths. Used to populate ErrMergeConflict.ConflictFiles
// after a non-FF merge fails. Best-effort: a status-read failure
// returns nil rather than masking the underlying merge error.
//
// Format dependency: relies on `git status --porcelain` v1
// shape (`XY <path>`, two-char status field starting at column
// 0). v2 / future formats would need parser updates; we pin
// implicitly by not passing --porcelain=v2. A locale-shifted
// or future-version git that changes the format silently
// returns no files and the caller falls through to the generic
// merge error — losing the conflict-files detail but never
// corrupting the merge.
func readUnmergedFiles(workDir string) []string {
	cmd := exec.Command("git", "-C", workDir, "status", "--porcelain")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		switch line[:2] {
		case "UU", "AA", "DD", "AU", "UA", "DU", "UD":
			files = append(files, strings.TrimSpace(line[3:]))
		}
	}
	return files
}



// --- commit message format ---

// buildCommitMessage constructs the standard commit subject+body
// used by all enju clients. The subject format
//
//	Task {taskID} by @{username}: result
//
// is stable and machine-parseable — iteration 3.1's rollback walker
// and any future audit tooling rely on it. A commit that writes
// artifacts extends the subject:
//
//	Task {taskID} by @{username}: result + N artifact(s)
//
//	Artifacts: path1, path2, ...
func buildCommitMessage(taskID, username string, artifactPaths []string, modelName string, trailers enjugit.EnjuTrailers) string {
	var subject string
	if len(artifactPaths) == 0 {
		subject = fmt.Sprintf("Task %s by @%s: result", taskID, username)
	} else {
		subject = fmt.Sprintf("Task %s by @%s: result + %d artifact(s)\n\nArtifacts: %s",
			taskID, username, len(artifactPaths), strings.Join(artifactPaths, ", "))
	}
	// Trailer paragraph collects every `Key: value` line that
	// should go at the very end of the message — git's trailer
	// convention. Co-Authored-By and AI-Model live here
	// alongside the Enju-* task-completion metadata so a single
	// trailer-aware reader (scanners, `git interpret-trailers`,
	// humans running `git log --format='%(trailers)'`) picks
	// them all up with the same parse rules.
	var trailerLines []string
	if modelName != "" {
		trailerLines = append(trailerLines, aiCoAuthor(modelName))
		trailerLines = append(trailerLines, "AI-Model: "+modelName)
	}
	if rendered := enjugit.RenderEnjuTrailers(trailers); rendered != "" {
		trailerLines = append(trailerLines, rendered)
	}
	if len(trailerLines) > 0 {
		subject += "\n\n" + strings.Join(trailerLines, "\n")
	}
	return subject
}

// aiCoAuthor returns a Co-Authored-By trailer for the given model name.
// Recognized prefixes get a branded identity (Claude, Gemini, GPT, etc.);
// unknown models get a generic "AI" label.
func aiCoAuthor(modelName string) string {
	lower := strings.ToLower(modelName)
	var name, email string
	switch {
	case strings.HasPrefix(lower, "claude"):
		name = "Claude (" + modelName + ")"
		email = "noreply@anthropic.com"
	case strings.HasPrefix(lower, "gemini"):
		name = "Gemini (" + modelName + ")"
		email = "noreply@google.com"
	case strings.HasPrefix(lower, "gpt"):
		name = "GPT (" + modelName + ")"
		email = "noreply@openai.com"
	default:
		name = "AI (" + modelName + ")"
		email = "noreply@enju.ai"
	}
	return fmt.Sprintf("Co-Authored-By: %s <%s>", name, email)
}

// --- standard path helpers ---
//
// Enju state lives under `enju/` in the workspace clone so it
// coexists cleanly with existing repo content. Artifacts live
// at their natural paths (no prefix). Templates live under
// `enju/templates/`.

// ResultDir lived here historically and built the task's
// repo-relative result path from (runSeq, instanceKey,
// taskDefID). It's been deleted pre-launch because the layout
// schema moved coordinator-side into engine.ComputeResultDir:
// the server now stamps the full path onto every task
// response's result_dir field, and clients consume it as-is.
// Keeping the rule in one place was the whole point of the
// layout overhaul (visible enju/ root + task-first +
// key=value segments) — see docs/storage.md for the new
// shape.

