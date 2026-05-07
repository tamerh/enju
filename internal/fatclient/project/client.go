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
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
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

// sshAuthMethod returns an SSH auth method for the given remote URL.
// Tries SSH agent first (via SSH_AUTH_SOCK), then falls back to
// common private key files (~/.ssh/id_ed25519, id_rsa). Returns nil
// for non-SSH URLs (http/https/local paths) — go-git handles those
// without explicit auth.
func sshAuthMethod(remoteURL string) transport.AuthMethod {
	if !enjugit.IsSSHURL(remoteURL) {
		return nil
	}
	// Try SSH agent first.
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		if auth, err := gitssh.NewSSHAgentAuth("git"); err == nil {
			return auth
		}
	}
	// Fall back to key files.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	keyFiles := []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, ".ssh", "id_ecdsa"),
	}
	for _, kf := range keyFiles {
		if _, err := os.Stat(kf); err != nil {
			continue
		}
		auth, err := gitssh.NewPublicKeysFromFile("git", kf, "")
		if err != nil {
			continue // passphrase-protected, skip
		}
		return auth
	}
	return nil
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

// LeaveProject forgets the cached Project handle (if any) and
// removes the on-disk clone directory. The next ForProject call for
// this project will re-clone from the remote. Safe to call even if
// the project was never opened in this process — a missing clone
// directory is not an error.
//
// Use cases: reclaiming disk space for a project the citizen is
// done with, or recovering from a corrupted local clone. The remote
// is untouched; this is purely a local cache wipe.
func (ws *Opener) LeaveProject(projectID int64) error {
	if projectID == 0 {
		return fmt.Errorf("projectID is required")
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if p, ok := ws.clients[projectID]; ok {
		// Hold the project lock briefly to make sure no in-flight
		// submit is mid-write. Once we have it, drop the map entry
		// and release — nobody else can acquire this handle since
		// we're also holding the workspace lock.
		p.mu.Lock()
		p.mu.Unlock() //nolint:staticcheck // barrier against in-flight writers
		delete(ws.clients, projectID)
	}
	// Find whichever directory format exists (slug or numeric).
	workDir := ws.findProjectDir(projectID)
	if workDir == "" {
		return nil // nothing to remove
	}
	if err := os.RemoveAll(workDir); err != nil {
		return fmt.Errorf("removing clone at %s: %w", workDir, err)
	}
	return nil
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
	// Recover from a previous crash that left non-tracked files
	// staged in <workDir>.preserve-in-progress. Best-effort: a
	// failure here logs but doesn't block workspace open — the
	// preserve dir will stay on disk for manual inspection, and
	// subsequent operations still work on whatever's already in
	// workDir. See preserve.go for the recovery logic.
	if err := enjugit.RecoverLeftoverSharedPreserve(workDir, logger); err != nil && logger != nil {
		logger.Warn("preserve-dir recovery failed; leaving for manual inspection",
			"error", err, "path", workDir+enjugit.SharedPreserveDirSuffix)
	}

	// Existing clone path: open via the shared enjugit/internal/git
	// layer so project.Clone and any enjugit.Workflow opened on the
	// same dir end up holding the SAME *gogit.Repository pointer.
	// Eliminates dual-handle drift (#381) — both packages now read
	// the same in-memory ref state instead of stale-reading after
	// the other's writes.
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		gc, err := enjugit.OpenSharedClone(workDir, "", logger)
		if err != nil {
			return nil, fmt.Errorf("opening existing clone at %s: %w", workDir, err)
		}
		// Stale-clone detection: numeric workspace dirs can be
		// reused across DB wipes. If the on-disk origin doesn't
		// match the requested remoteURL, wipe and re-clone so the
		// citizen doesn't work against unrelated content.
		if remoteURL != "" && gc.RemoteURL() != remoteURL {
			logger.Warn("stale workspace — remote URL mismatch, re-cloning",
				"path", workDir, "expected", remoteURL, "found", gc.RemoteURL())
			os.RemoveAll(workDir)
			// Fall through to the fresh-clone path below.
		} else {
			return cloneFromGit(gc, logger), nil
		}
	}

	// Fresh-clone path: no existing checkout. Use the enjugit/git
	// helpers so the resulting *gogit.Repository is identical to
	// what enjugit would open on the same dir.
	if remoteURL == "" {
		// Local-only mode.
		gc, err := enjugit.InitLocalShared(workDir, "", logger)
		if err != nil {
			return nil, fmt.Errorf("initializing local-only repo: %w", err)
		}
		logger.Info("initialized local-only repo", "path", workDir)
		return cloneFromGit(gc, logger), nil
	}

	// Clean stale non-repo dir before clone so PlainClone writes
	// into a fresh path.
	if stat, err := os.Stat(workDir); err == nil && stat.IsDir() {
		entries, _ := os.ReadDir(workDir)
		if len(entries) > 0 {
			logger.Warn("removing existing non-repo directory before clone", "path", workDir)
			if err := os.RemoveAll(workDir); err != nil {
				return nil, fmt.Errorf("cleaning work dir before clone: %w", err)
			}
		}
	}
	gc, err := enjugit.CloneOrInitShared(workDir, remoteURL, "", logger)
	if err != nil {
		// Empty-remote bootstrap path (A.5 fix). Fresh remotes
		// with no initial commit surface as
		// transport.ErrEmptyRemoteRepository — the common
		// first-time scenario ("create a fresh repo, point enju
		// at it, start submitting tasks"). Init an empty local
		// clone with origin configured and let the first submit
		// push the bootstrap commit.
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			if stat, statErr := os.Stat(workDir); statErr == nil && stat.IsDir() {
				_ = os.RemoveAll(workDir)
			}
			gc, ierr := enjugit.InitLocalShared(workDir, "", logger)
			if ierr != nil {
				return nil, fmt.Errorf("initializing empty-remote clone: %w", ierr)
			}
			if eerr := gc.EnsureOrigin(remoteURL); eerr != nil {
				return nil, fmt.Errorf("configuring origin for empty remote: %w", eerr)
			}
			logger.Info("bootstrapped empty remote", "url", remoteURL, "path", workDir)
			return cloneFromGit(gc, logger), nil
		}
		return nil, friendlyGitError("clone", remoteURL, err)
	}
	logger.Info("cloned project", "url", remoteURL, "path", workDir)
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

// SubmitRequest packages everything needed for one submit: the files
// to write, the commit message inputs, and (for push retry) the
// max number of retry attempts.
type SubmitRequest struct {
	// TaskID is the fully-qualified task id (e.g. "3:1:foo").
	// Used only for the commit message.
	TaskID string
	// Username is the submitting citizen's username (used in
	// commit message subject).
	Username string
	// AuthorName and AuthorEmail populate the git commit
	// `Author` field so commits pushed to the user's own GitHub
	// repo show up in contributor graphs, blame, and CODEOWNERS
	// workflows under the real citizen identity. When empty
	// (e.g. in unit tests that don't care), the commit falls
	// back to a generic `Enju Client <enju-client@localhost>`
	// placeholder. Callers that know their citizen's identity
	// — namely the MCP server's submit handler — should always
	// pass both fields.
	AuthorName  string
	AuthorEmail string
	// ModelName, when non-empty, indicates this submission was made
	// by an AI citizen. It's appended as a git trailer
	// (`AI-Model: <value>`) so `git log --format='%(trailers)'`
	// can distinguish AI vs human contributions.
	ModelName string
	// Files is the full set of files this commit writes — result
	// files AND any artifact writes in one batch. Order is not
	// significant.
	Files []FileWrite
	// ArtifactPaths lists the repo-relative paths in Files that
	// are artifact writes (under `artifacts/...`). Used for the
	// commit message body and for the reported artifacts list.
	// May be empty.
	ArtifactPaths []string
	// MaxRetries caps the push retry loop on non-fast-forward
	// rejections. Defaults to 3 if zero.
	MaxRetries int
	// Branch is the git branch this submit commits and pushes
	// to. Empty → the project's configured default branch.
	// Populated from the run's branch field when the MCP
	// client's submit handler builds the request — or, in the
	// living-workflow phase 6b.1 topic-branch flow, from the
	// per-iteration topic branch that the coordinator handed
	// back at claim time.
	Branch string

	// BaseBranch, when non-empty, is the fork point for `Branch`
	// when the latter doesn't yet exist locally or as a remote-
	// tracking ref. Used by the topic-branch flow: pass the run
	// branch as BaseBranch so the per-iteration topic branch
	// inherits the run branch's current state. Ignored when
	// Branch already exists. Empty → fall back to the project's
	// default base (origin/main, then origin/<HEAD>, then root).
	BaseBranch string

	// Trailers carries Enju-* trailer values to emit in the
	// commit message footer. TaskID defaults to req.TaskID at
	// commit time (so callers don't have to set it twice).
	// Compute tasks populate Trailers.ExitSet/ExitCode/
	// DurationSeconds; answer/review/vote submits leave those
	// zero and just get the task-complete trailer.
	Trailers enjugit.EnjuTrailers

	// ProjectID, if nonzero, pairs with StateDir to auto-
	// advance the fat-client's scan cursor past the commit
	// this submit produces. Saves the submitter from posting
	// their own self-generated trailer back through the
	// reconcile endpoint on the next scan. Zero → skip
	// (coordinator-side tests, store-level unit tests, any
	// caller that doesn't maintain a cursor).
	ProjectID int64

	// StateDir is the fat-client state directory holding
	// per-project cursor files (typically ~/.enju/state/).
	// Set alongside ProjectID to enable auto-advance. Empty
	// string means "no cursor to maintain" — the scanner will
	// later re-see our commit and idempotently no-op at the
	// coordinator; auto-advance is a bandwidth optimization,
	// not a correctness requirement.
	StateDir string
}

// SubmitResult is the outcome of a successful SubmitTaskResult call.
type SubmitResult struct {
	// CommitSHA is the SHA of the commit that was actually pushed
	// to the remote (or committed locally if there's no remote).
	// Clients report this back to the coordinator so it can update
	// the task's result_path index and artifact index.
	CommitSHA string
	// Attempts is the number of push attempts that occurred —
	// always at least 1. Higher values mean the remote advanced
	// between our first attempt and eventual success, and we
	// rebased by refetching and re-applying.
	Attempts int
}

// SubmitTaskResult is the main write path for the client. It handles
// the overlay → commit → push cycle, with a `git pull --rebase`
// retry on non-fast-forward rejections. Returns the commit SHA
// that actually landed so the caller can report it to the
// coordinator.
//
// The caller MUST hold the project lock for the duration of the call.
//
// Behavior details:
//
//   - Files are written into the working tree at their declared
//     paths (creating parent directories as needed). Any existing
//     files at those paths are overwritten. **Writes go on top of
//     whatever is currently at HEAD** — user commits made in the
//     workspace between submits are preserved, not rolled back.
//
//   - A single commit is created with the standard enju subject
//     format. All files in the request land in that one commit.
//
//   - The commit is pushed. If the remote rejected the push as
//     non-fast-forward (another client committed in the meantime),
//     we run `git pull --rebase --autostash` to rebase our local
//     chain (any user commits + our just-made task commit) onto
//     the new remote tip, then push again. The rebase preserves
//     all local work; only a real path-level conflict can fail
//     it, and we surface that as a clear error rather than
//     silently dropping commits.
//
//   - If we hit MaxRetries without succeeding, the most recent
//     push error is returned. The caller should surface this so
//     the citizen knows their submit didn't land.
func (p *Clone) SubmitTaskResult(req SubmitRequest) (*SubmitResult, error) {
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("no files to write")
	}
	if req.TaskID == "" || req.Username == "" {
		return nil, fmt.Errorf("TaskID and Username are required")
	}

	// Default trailers carry at minimum the task id so the
	// fetch-path scanner can pick any task-submit commit up
	// without the caller having to opt in.
	trailers := req.Trailers
	if trailers.TaskID == "" {
		trailers.TaskID = req.TaskID
	}
	if len(trailers.Artifacts) == 0 && len(req.ArtifactPaths) > 0 {
		trailers.Artifacts = req.ArtifactPaths
	}
	commitMsg := buildCommitMessage(req.TaskID, req.Username, req.ArtifactPaths, req.ModelName, trailers)

	// Ensure we're on the target branch BEFORE writing files —
	// commits otherwise land on the current HEAD's branch,
	// which is fine only when the caller is already on the
	// right branch. When the caller passed BaseBranch (topic-
	// branch flow), use it as the fork point so a per-iteration
	// topic branch lands on top of the run branch's tip.
	if err := p.gitClone.CheckoutBranchFrom(p.resolveBranch(req.Branch), req.BaseBranch, p.defaultBranchOr()); err != nil {
		return nil, fmt.Errorf("switching to branch %q: %w", p.resolveBranch(req.Branch), err)
	}

	// Overlay the new files on top of current HEAD. Any local
	// commits the user made (e.g. a manual edit to a script
	// between submits) stay where they are — we do NOT reset
	// HEAD to origin/<default> any more. That reset was the
	// root cause of the "fat-client clobbers user commits" bug.
	for _, f := range req.Files {
		full := filepath.Join(p.workDir, f.RepoRelPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return nil, fmt.Errorf("creating dir for %s: %w", f.RepoRelPath, err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0644
		}
		if err := os.WriteFile(full, f.Content, mode); err != nil {
			return nil, fmt.Errorf("writing %s: %w", f.RepoRelPath, err)
		}
		// WriteFile respects umask on creation but existing
		// files keep their prior mode. Explicit chmod so the
		// requested mode actually sticks across overwrite-in-
		// place cases (re-submit, re-snapshot).
		if err := os.Chmod(full, mode); err != nil {
			return nil, fmt.Errorf("chmod %s: %w", f.RepoRelPath, err)
		}
	}

	// One commit per submit, on top of whatever was there.
	// Stage ONLY the paths the caller explicitly wrote —
	// AddGlob(".") would sweep any other pending untracked
	// edits (e.g. a co-authored template) into the commit.
	stagePaths := make([]string, 0, len(req.Files))
	for _, f := range req.Files {
		stagePaths = append(stagePaths, f.RepoRelPath)
	}
	if _, err := p.commit(commitMsg, stagePaths, req.AuthorName, req.AuthorEmail); err != nil {
		return nil, fmt.Errorf("creating commit: %w", err)
	}

	// Push with retry. On non-FF rejection, rebase our chain
	// onto the new remote tip and try again. Every attempt
	// preserves local history — only a real path conflict can
	// stop the rebase, and we surface that as an error rather
	// than discard work.
	maxRetries := req.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	branch := p.resolveBranch(req.Branch)
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Read the local tip so PushWithVerify can confirm
		// the remote ref actually advanced to that SHA after
		// the push call returns success — catches the silent-
		// success class of bugs (transport quirks, server hooks
		// that drop the push but don't error). Empty remoteURL
		// short-circuits to nil inside the gitClone push path,
		// matching the "local-only operator mode" semantics.
		expectedSHA := ""
		if head, herr := p.repo.Head(); herr == nil {
			expectedSHA = head.Hash().String()
		}
		pushErr := p.gitClone.PushWithVerify(branch, expectedSHA)
		if pushErr == nil || errors.Is(pushErr, enjugit.ErrSharedNoRemote) {
			advanceCursorIfConfigured(req.ProjectID, req.StateDir, branch, expectedSHA)
			return &SubmitResult{CommitSHA: expectedSHA, Attempts: attempt}, nil
		}
		if !errors.Is(pushErr, enjugit.ErrSharedPushNonFF) {
			return nil, fmt.Errorf("push failed: %w", pushErr)
		}
		// Non-FF: the remote moved while we were working.
		// `git pull --rebase --autostash` fetches the new
		// commits and replays our local commits (user's +
		// ours) on top. Uses the system git CLI because
		// go-git's rebase support doesn't cover the cases we
		// need (especially divergent-history with
		// user-authored commits).
		if rebaseErr := p.gitClone.RebaseOnRemote(branch); rebaseErr != nil {
			return nil, fmt.Errorf(
				"submit push rejected and rebase failed — your submit likely touches a file another client also changed. Local work is still in git reflog. Details: %w",
				rebaseErr)
		}
	}
	return nil, fmt.Errorf("submit failed after %d push attempts", maxRetries)
}
// extractTaskIDTrailer reads the `Enju-Task-Complete:` trailer
// out of a commit message, or returns "" if the commit isn't a
// task submission. The trailer key matches
// TrailerTaskComplete in trailers.go — the one every
// buildCommitMessage commit emits. A rebase rewrites commit
// hashes but preserves message bodies, so this stays the
// stable key for mapping post-rebase SHAs back to the
// originating submit entry.
func extractTaskIDTrailer(msg string) string {
	prefix := enjugit.TrailerTaskComplete + ":"
	for _, line := range strings.Split(msg, "\n") {
		// HasPrefix on the trimmed line so the match is a
		// real trailer, not a body mention. A commit whose
		// description happens to contain the phrase
		// "Enju-Task-Complete: foo" in prose shouldn't be
		// misread as the task's own commit.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return ""
}

// CommitFilesRequest packages inputs for a generic file-commit
// that isn't tied to a task submission (the existing
// SubmitTaskResult is task-shaped — TaskID, ArtifactPaths,
// task-specific commit subject). Use CommitFiles for client-side
// artifacts that belong to a run or project but aren't a task
// result: DAG snapshots, README updates, diagnostic bundles.
type CommitFilesRequest struct {
	Files       []FileWrite
	CommitMsg   string // full commit message body (subject on first line, blank line, body)
	AuthorName  string
	AuthorEmail string
	ModelName   string // when non-empty, appends an `AI-Model: <x>` trailer
	MaxRetries  int    // defaults to 3
	// Branch targets a specific branch for this commit. Empty
	// → the project's configured default.
	Branch string
}

// CommitFilesResult is the outcome of a successful CommitFiles
// call. Mirrors SubmitResult — commit SHA + attempt count for
// logging/diagnostic purposes.
type CommitFilesResult struct {
	CommitSHA string
	Attempts  int
	// NoOp is true when none of the requested files would have
	// changed the working tree (every target path already
	// holds identical content). When NoOp is true, no commit
	// is created and CommitSHA is the SHA of the current HEAD.
	// Callers that want "skip no-op exports" can key off this.
	NoOp bool
}

// CommitFiles is SubmitTaskResult's task-free sibling: overlay
// files onto HEAD → commit → push → rebase-retry on non-FF.
// Shares the push/rebase logic so a failing push on a diagram
// export retries with the same "preserve user commits" behavior
// as a task submit.
//
// Semantics:
//
//   - Caller must hold the project lock for the duration.
//   - If every file in the request would be a no-op (same bytes
//     already on disk), nothing is written, no commit is made,
//     and the result carries NoOp=true. Useful for idempotent
//     exports — calling enju_export_diagram twice with the same
//     run state shouldn't clutter git history with empty
//     commits.
//   - Otherwise: one commit with the caller's message, then
//     push with the standard rebase-on-non-FF retry loop.
//
// Returned CommitSHA is the SHA that actually landed on the
// remote (rebase may rewrite it between the local commit and
// the eventual push).
func (p *Clone) CommitFiles(req CommitFilesRequest) (*CommitFilesResult, error) {
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("no files to write")
	}
	if req.CommitMsg == "" {
		return nil, fmt.Errorf("commit message is required")
	}
	maxRetries := req.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	// No-op check: read each target path's current content (if
	// any) and bail if every requested write matches. Done
	// before any on-disk mutation so a skipped export leaves
	// the working tree byte-identical.
	allMatch := true
	for _, f := range req.Files {
		full := filepath.Join(p.workDir, f.RepoRelPath)
		existing, err := os.ReadFile(full)
		if err != nil || !bytes.Equal(existing, f.Content) {
			allMatch = false
			break
		}
	}
	if allMatch {
		sha := ""
		if head, herr := p.repo.Head(); herr == nil {
			sha = head.Hash().String()
		}
		return &CommitFilesResult{CommitSHA: sha, Attempts: 0, NoOp: true}, nil
	}

	// Commit message — optionally append the AI-Model trailer
	// so `git log --format='%(trailers)'` can distinguish AI
	// vs human contributions, same convention as SubmitTaskResult.
	msg := req.CommitMsg
	if req.ModelName != "" {
		msg += fmt.Sprintf("\n\nAI-Model: %s\n", req.ModelName)
	}

	// Ensure the commit lands on the right branch.
	if err := p.gitClone.CheckoutBranchFrom(p.resolveBranch(req.Branch), "", p.defaultBranchOr()); err != nil {
		return nil, fmt.Errorf("switching to branch %q: %w", p.resolveBranch(req.Branch), err)
	}

	// Overlay files onto current HEAD (no reset — see
	// SubmitTaskResult godoc for the rationale).
	for _, f := range req.Files {
		full := filepath.Join(p.workDir, f.RepoRelPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return nil, fmt.Errorf("creating dir for %s: %w", f.RepoRelPath, err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0644
		}
		if err := os.WriteFile(full, f.Content, mode); err != nil {
			return nil, fmt.Errorf("writing %s: %w", f.RepoRelPath, err)
		}
		if err := os.Chmod(full, mode); err != nil {
			return nil, fmt.Errorf("chmod %s: %w", f.RepoRelPath, err)
		}
	}

	stagePaths := make([]string, 0, len(req.Files))
	for _, f := range req.Files {
		stagePaths = append(stagePaths, f.RepoRelPath)
	}
	sha, err := p.commit(msg, stagePaths, req.AuthorName, req.AuthorEmail)
	if err != nil {
		return nil, fmt.Errorf("creating commit: %w", err)
	}

	branch := p.resolveBranch(req.Branch)
	for attempt := 1; attempt <= maxRetries; attempt++ {
		expectedSHA := sha
		if head, herr := p.repo.Head(); herr == nil {
			expectedSHA = head.Hash().String()
		}
		pushErr := p.gitClone.PushWithVerify(branch, expectedSHA)
		if pushErr == nil || errors.Is(pushErr, enjugit.ErrSharedNoRemote) {
			return &CommitFilesResult{CommitSHA: expectedSHA, Attempts: attempt}, nil
		}
		if !errors.Is(pushErr, enjugit.ErrSharedPushNonFF) {
			return nil, fmt.Errorf("push failed: %w", pushErr)
		}
		if rebaseErr := p.gitClone.RebaseOnRemote(branch); rebaseErr != nil {
			return nil, fmt.Errorf(
				"push rejected and rebase failed — local work is still in git reflog. Details: %w",
				rebaseErr)
		}
	}
	return nil, fmt.Errorf("commit failed after %d push attempts", maxRetries)
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



// commit stages everything in the working tree and creates a
// commit with the given message and author identity. If authorName
// or authorEmail is empty, a generic `Enju Client` placeholder is
// used — this is only the case for unit tests that don't care
// about author attribution. Production callers (the MCP server's
// submit handler) always pass the citizen's real name and email so
// pushes to the user's own git host attribute correctly on
// contributor graphs, blame, and CODEOWNERS.
// commit builds a git commit with the given message + author
// signature, staging ONLY the explicit paths the caller passes.
// An earlier version used wt.AddGlob(".") which incidentally
// swept every untracked file in the worktree into the commit —
// that was the template-scatter bug, where co-authored
// untracked templates (tmpl-b sitting next to tmpl-a being
// instantiated) silently landed on the run's branch and
// vanished from main. Scoped staging fixes both that bug and
// the general principle that a commit helper should only
// touch paths the caller asked for.
//
// An empty `paths` argument keeps the old "stage everything
// under worktree root" semantics — we don't have any callers
// relying on that anymore, but the fall-through exists so a
// future caller that genuinely wants a sweep-everything
// commit can opt in by passing nil.
func (p *Clone) commit(message string, paths []string, authorName, authorEmail string) (string, error) {
	wt, err := p.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("getting worktree: %w", err)
	}
	if paths == nil {
		if err := wt.AddGlob("."); err != nil {
			return "", fmt.Errorf("staging: %w", err)
		}
	} else {
		for _, rel := range paths {
			if rel == "" {
				continue
			}
			// wt.Add handles New, Modified, and Deleted
			// file states for the given path. Errors
			// propagate (typically unknown-path, which
			// means the caller's bookkeeping is off).
			if _, err := wt.Add(rel); err != nil {
				return "", fmt.Errorf("staging %s: %w", rel, err)
			}
		}
	}
	status, err := wt.Status()
	if err != nil {
		return "", fmt.Errorf("status: %w", err)
	}
	if status.IsClean() {
		return "", fmt.Errorf("nothing to commit")
	}
	if authorName == "" {
		authorName = "Enju Client"
	}
	if authorEmail == "" {
		authorEmail = "enju-client@localhost"
	}
	hash, err := wt.Commit(message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("committing: %w", err)
	}
	return hash.String(), nil
}

// --- Remote comparison (ported from internal/git iteration 4) ---

// RemoteComparisonStatus enumerates the possible relationships
// between the local HEAD and the remote refs/heads/main ref. Used by
// Project.CompareToRemote and by the MCP client's project_remote_status
// tool to render actionable guidance (ahead is fast-forwardable,
// diverged requires force, etc).
type RemoteComparisonStatus string

const (
	RemoteInSync      RemoteComparisonStatus = "in_sync"
	RemoteAhead       RemoteComparisonStatus = "ahead"
	RemoteBehind      RemoteComparisonStatus = "behind"
	RemoteDiverged    RemoteComparisonStatus = "diverged"
	RemoteUnrelated   RemoteComparisonStatus = "unrelated"
	RemoteRemoteEmpty RemoteComparisonStatus = "remote_empty"
	RemoteLocalEmpty  RemoteComparisonStatus = "local_empty"
	RemoteNoRemote    RemoteComparisonStatus = "no_remote"
	RemoteUnreachable RemoteComparisonStatus = "unreachable"
)

// RemoteComparison is the structured result of CompareToRemote. It
// describes whether the local branch can be fast-forwarded onto the
// remote (Ahead), whether the remote has commits local doesn't
// (Behind), whether both sides have unique commits (Diverged), or
// whether they share no history (Unrelated).
type RemoteComparison struct {
	Status     RemoteComparisonStatus
	LocalHead  string // SHA of local HEAD, empty if no commits yet
	RemoteHead string // SHA of remote refs/heads/main, empty if absent
	AheadBy    int    // commits on local that aren't on remote
	BehindBy   int    // commits on remote that aren't on local
	// Unreachable carries the ls-remote error when Status ==
	// RemoteUnreachable so the caller can surface it to the user.
	Unreachable string
}

// CompareToRemote performs an ls-remote against the configured
// origin and classifies the local branch's relationship to the
// remote's refs/heads/main. The distinction between Ahead and
// Diverged matters because a non-force push is safe in the first
// case and will be rejected in the second — the MCP client's sync
// handler uses this as a preflight to decide whether it can push
// without asking the user to confirm force=true.
//
// Ported verbatim from the iteration 4 coordinator-side
// `git.Writer.CompareToRemote`, then moved to the client side
// during the iteration A orchestrator rewrite so the coordinator
// stops touching git entirely.
func (p *Clone) CompareToRemote() (*RemoteComparison, error) {
	r := &RemoteComparison{}

	if p.remoteURL == "" {
		r.Status = RemoteNoRemote
		return r, nil
	}
	// Solo enju_init projects store the working-tree path as
	// the coordinator's remote_url even though the local repo
	// has no `origin` configured. Without this second check,
	// the comparison would attempt `git ls-remote` against the
	// working tree's missing origin, fail with "no origin
	// remote", and mark the status as RemoteUnreachable —
	// confusing UX for a healthy local-only project. Report
	// RemoteNoRemote instead so the user-facing answer matches
	// the actual git state.
	if p.gitClone.RemoteURL() == "" {
		r.Status = RemoteNoRemote
		return r, nil
	}

	localHash, _, _ := p.gitClone.Head()
	r.LocalHead = localHash

	remoteHash, err := p.gitClone.RemoteBranchHash(p.resolveBranch(""))
	if err != nil {
		r.Status = RemoteUnreachable
		r.Unreachable = err.Error()
		return r, nil
	}
	r.RemoteHead = remoteHash

	switch {
	case localHash == "" && remoteHash == "":
		r.Status = RemoteInSync
		return r, nil
	case localHash == "":
		r.Status = RemoteLocalEmpty
		return r, nil
	case remoteHash == "":
		r.Status = RemoteRemoteEmpty
		return r, nil
	case localHash == remoteHash:
		r.Status = RemoteInSync
		return r, nil
	}

	// Both sides have commits and they differ — figure out the
	// relationship via the merge base. go-git's MergeBase walks
	// ancestors of both commits and returns any commits reachable
	// from both; an empty result means the two histories don't
	// share any ancestor at all (unrelated trees).
	localCommit, err := p.repo.CommitObject(plumbing.NewHash(localHash))
	if err != nil {
		return nil, fmt.Errorf("loading local commit: %w", err)
	}
	// The remote commit may not be present in the local object
	// database yet (we haven't fetched — we only did ls-remote).
	// Attempt to load it; if it's missing, fetch and retry so
	// merge-base and counts work. A fetch is cheap compared to
	// push and is only done on the explicit diagnostic path.
	remoteCommit, err := p.repo.CommitObject(plumbing.NewHash(remoteHash))
	if err != nil {
		if fetchErr := p.repo.Fetch(&gogit.FetchOptions{
			RemoteName: "origin",
			Auth:       sshAuthMethod(p.remoteURL),
		}); fetchErr != nil && fetchErr != gogit.NoErrAlreadyUpToDate {
			// Can't fetch — report diverged conservatively so the
			// user doesn't assume they're safe to fast-forward.
			r.Status = RemoteDiverged
			return r, nil
		}
		remoteCommit, err = p.repo.CommitObject(plumbing.NewHash(remoteHash))
		if err != nil {
			r.Status = RemoteDiverged
			return r, nil
		}
	}

	// remoteCommit is ancestor of localCommit? → strictly ahead.
	remoteIsAncestor, err := remoteCommit.IsAncestor(localCommit)
	if err == nil && remoteIsAncestor {
		r.Status = RemoteAhead
		r.AheadBy = countCommitsBetween(localCommit, remoteHash)
		return r, nil
	}
	localIsAncestor, err := localCommit.IsAncestor(remoteCommit)
	if err == nil && localIsAncestor {
		r.Status = RemoteBehind
		r.BehindBy = countCommitsBetween(remoteCommit, localHash)
		return r, nil
	}

	// Neither is an ancestor of the other — truly diverged, or
	// unrelated. Use MergeBase to distinguish.
	bases, err := localCommit.MergeBase(remoteCommit)
	if err != nil || len(bases) == 0 {
		r.Status = RemoteUnrelated
		return r, nil
	}
	baseHash := bases[0].Hash.String()
	r.Status = RemoteDiverged
	r.AheadBy = countCommitsBetween(localCommit, baseHash)
	r.BehindBy = countCommitsBetween(remoteCommit, baseHash)
	return r, nil
}

// countCommitsBetween walks the first-parent history starting at
// `from` and returns the number of commits traversed before
// reaching `until` (exclusive). Used to populate AheadBy / BehindBy
// in a RemoteComparison. Returns 0 if anything goes wrong — this is
// a diagnostic, not correctness-critical.
func countCommitsBetween(from *object.Commit, until string) int {
	count := 0
	current := from
	for current != nil {
		if current.Hash.String() == until {
			return count
		}
		count++
		if current.NumParents() == 0 {
			return count
		}
		parent, err := current.Parent(0)
		if err != nil {
			return count
		}
		current = parent
	}
	return count
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

// friendlyGitError wraps a raw go-git error with an actionable hint
// based on the operation being performed (clone/push/pull/fetch/
// ls-remote) and a best-effort classification of the underlying cause
// (auth, network, unknown host, non-fast-forward). The original error
// is wrapped with %w so callers can still errors.Is/As against it.
//
// op is a short verb phrase like "clone", "push", "fetch origin" that
// appears at the start of the message. remoteURL is optional; when
// set it's included so the user sees which remote failed.
func friendlyGitError(op, remoteURL string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	var hint string
	switch {
	case strings.Contains(msg, "ssh:") ||
		strings.Contains(msg, "handshake failed") ||
		strings.Contains(msg, "publickey") ||
		strings.Contains(msg, "unable to authenticate"):
		hint = "check that your SSH agent has the right key loaded (`ssh-add -l`) and that the key is authorized on the remote"
	case strings.Contains(msg, "authentication required") ||
		strings.Contains(msg, "authorization failed") ||
		strings.Contains(msg, "401") ||
		strings.Contains(msg, "403"):
		hint = "check your git credential helper or ~/.netrc — HTTPS remotes need a valid token/password"
	case strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "rejected"):
		hint = "remote has advanced — run enju_project_sync to refresh, or retry the submit"
	case strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "network is unreachable"):
		hint = "network/DNS issue — check connectivity to the git host"
	case strings.Contains(msg, "repository not found") ||
		strings.Contains(msg, "does not exist"):
		if isLocalGitPath(remoteURL) {
			hint = "verify the path exists and points to a valid bare repository"
		} else {
			hint = "verify the remote URL and that your account has access"
		}
	}
	where := ""
	if remoteURL != "" {
		where = " " + remoteURL
	}
	if hint == "" {
		return fmt.Errorf("%s%s: %w", op, where, err)
	}
	return fmt.Errorf("%s%s: %w (hint: %s)", op, where, err, hint)
}

// isLocalGitPath returns true if remoteURL looks like a local
// filesystem path rather than a network URL. Used to pick the
// right "not found" hint: network URLs want a credentials/URL
// hint, local paths want a "does the path exist" hint.
func isLocalGitPath(remoteURL string) bool {
	if remoteURL == "" {
		return false
	}
	// Network URL schemes: https://, git://, ssh://, file:// (which
	// is technically local but conventionally accessed by URL).
	if strings.Contains(remoteURL, "://") {
		return false
	}
	// SCP-style SSH remote: user@host:path. The ":" is the
	// distinguishing marker — local paths with ":" are vanishingly
	// rare on Unix (and a windows "C:\" path won't match since the
	// leading char is alpha-colon-backslash, not alpha-colon-alpha).
	if i := strings.Index(remoteURL, ":"); i > 0 {
		// Make sure the pre-colon part doesn't start with "/" or
		// "." (which would mean it's an absolute or relative
		// path, not user@host). user@host form requires an `@`.
		if strings.Contains(remoteURL[:i], "@") {
			return false
		}
	}
	return true
}
