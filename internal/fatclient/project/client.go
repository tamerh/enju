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
	"encoding/json"
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
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/gofrs/flock"

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

// HasLocalClone returns true if a clone for the given project
// already exists on disk in this project. Used by list-style
// callers (enju_list_projects) that want to decorate their output
// with local state WITHOUT triggering a fresh clone as a side
// effect of the listing call.
func (ws *Opener) HasLocalClone(projectID int64) bool {
	dir := ws.findProjectDir(projectID)
	return dir != ""
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
// `<projectHome>/enju/.clone/` and sources from the per-project
// bare at `<projectHome>/enju/.bare.git/` (or a real github
// remote). Bot-flow only — operator-side reads go through
// ForProject.
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
	if !IsSSHURL(remoteURL) {
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

// isSSHURL returns true if the URL looks like an SSH remote
// (git@host:..., ssh://...).
func IsSSHURL(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	// git@github.com:org/repo.git pattern
	if strings.Contains(url, "@") && strings.Contains(url, ":") && !strings.Contains(url, "://") {
		return true
	}
	return false
}

// isLocalWorkingTree returns true if the path is a local directory
// with a .git subdirectory (a git working tree, not a bare repo).
// Used to detect enju_init'd projects whose path is stored as
// remote_url on the coordinator.
func IsLocalWorkingTree(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	gitDir := filepath.Join(path, ".git")
	gitInfo, err := os.Stat(gitDir)
	return err == nil && gitInfo.IsDir()
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
	if remoteURL != "" && IsLocalWorkingTree(remoteURL) {
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
	if err := recoverLeftoverPreserve(workDir, ws.logger); err != nil && ws.logger != nil {
		ws.logger.Warn("preserve-dir recovery failed during OpenExisting; leaving for manual inspection",
			"error", err, "path", workDir+preserveDirSuffix)
	}
	p := &Clone{
		projectID: projectID,
		workDir:   workDir,
		repo:      repo,
		logger:    ws.logger,
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

	// Push status bookkeeping — in-memory only, per process
	// lifetime. Updated by pushInternal on both success and
	// failure so the project_remote_status tool can report
	// "last push at / last push error". Resets on MCP client
	// restart, which is fine because the information is purely
	// diagnostic.
	lastPushAt    time.Time
	lastPushError string
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

// RemoteURL returns the configured origin URL, or an empty string
// for local-only clones.
func (p *Clone) RemoteURL() string { return p.remoteURL }

// GitOriginURL returns the URL of the "origin" remote from the git
// config, or empty if no origin is configured. For init'd projects,
// this is the actual push target (e.g. git@github.com:org/repo.git),
// which may differ from RemoteURL() (the local folder path stored
// on the coordinator).
func (p *Clone) GitOriginURL() string {
	rem, err := p.repo.Remote("origin")
	if err != nil {
		return ""
	}
	cfg := rem.Config()
	if cfg == nil || len(cfg.URLs) == 0 {
		return ""
	}
	return cfg.URLs[0]
}

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
	if err := recoverLeftoverPreserve(workDir, logger); err != nil && logger != nil {
		logger.Warn("preserve-dir recovery failed; leaving for manual inspection",
			"error", err, "path", workDir+preserveDirSuffix)
	}

	if repo, err := gogit.PlainOpen(workDir); err == nil {
		// Clone exists — verify the remote URL matches.
		// After a DB wipe, the same workspace dir (keyed by
		// project int ID) may hold a clone from a previous
		// project that used the same numeric ID. If the
		// origin URL doesn't match, wipe and re-clone so
		// the citizen doesn't work against stale content.
		if remoteURL != "" {
			mismatch := true
			if rem, err := repo.Remote("origin"); err == nil {
				if cfg := rem.Config(); cfg != nil && len(cfg.URLs) > 0 && cfg.URLs[0] == remoteURL {
					mismatch = false
				}
			}
			if mismatch {
				logger.Warn("stale workspace — remote URL mismatch, re-cloning",
					"path", workDir, "expected", remoteURL)
				os.RemoveAll(workDir)
				// Fall through to the clone path below.
				goto clone
			}
		}
		p := &Clone{
			workDir: workDir,
			repo:    repo,
			logger:  logger,
		}
		if remoteURL == "" {
			if rem, err := repo.Remote("origin"); err == nil {
				if cfg := rem.Config(); cfg != nil && len(cfg.URLs) > 0 {
					remoteURL = cfg.URLs[0]
				}
			}
		}
		p.remoteURL = remoteURL
		return p, nil
	}
clone:

	// Clone doesn't exist — either clone from remote or init empty.
	if remoteURL == "" {
		// Local-only mode: init the working tree and seed it
		// with one commit so subsequent operations
		// (branchBaseHash, template scans, submit's first
		// parent lookup) have something to work against.
		// Mirrors what InitBareWithSeed produces inside a bare;
		// the contents (README + enju/templates/.gitkeep) match
		// so the layout users see is the same regardless of
		// whether a remote was configured at create time.
		if err := os.MkdirAll(workDir, 0755); err != nil {
			return nil, fmt.Errorf("creating work dir: %w", err)
		}
		repo, err := gogit.PlainInitWithOptions(workDir, &gogit.PlainInitOptions{
			InitOptions: gogit.InitOptions{
				DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("initializing local-only repo: %w", err)
		}
		if err := seedLocalWorkspace(repo, workDir); err != nil {
			return nil, fmt.Errorf("seeding local-only repo: %w", err)
		}
		logger.Info("initialized local-only repo", "path", workDir)
		return &Clone{
			workDir: workDir,
			repo:    repo,
			logger:  logger,
		}, nil
	}

	// Clone from remote. If the parent dir exists but is empty or
	// stale, remove it first so go-git's PlainClone can write into
	// a fresh path.
	if stat, err := os.Stat(workDir); err == nil && stat.IsDir() {
		entries, _ := os.ReadDir(workDir)
		if len(entries) > 0 {
			logger.Warn("removing existing non-repo directory before clone", "path", workDir)
			if err := os.RemoveAll(workDir); err != nil {
				return nil, fmt.Errorf("cleaning work dir before clone: %w", err)
			}
		}
	}
	repo, err := gogit.PlainClone(workDir, false, &gogit.CloneOptions{
		URL:           remoteURL,
		ReferenceName: plumbing.ReferenceName("refs/heads/main"),
		SingleBranch:  true,
		Auth:          sshAuthMethod(remoteURL),
	})
	if err != nil {
		// Empty-remote bootstrap path (A.5 fix). Fresh repos on
		// GitHub/GitLab with no initial commit return
		// `transport.ErrEmptyRemoteRepository` from PlainClone —
		// this is the most common first-time user scenario
		// ("create a fresh repo, point enju at it, start
		// submitting tasks"). Initialize an empty local clone
		// configured with origin, and let the first submit push
		// the initial commit which populates the remote.
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			// Clean up anything PlainClone may have left behind
			// before re-initializing in the same path.
			if stat, statErr := os.Stat(workDir); statErr == nil && stat.IsDir() {
				_ = os.RemoveAll(workDir)
			}
			if err := os.MkdirAll(workDir, 0755); err != nil {
				return nil, fmt.Errorf("creating work dir for empty remote: %w", err)
			}
			repo2, err := gogit.PlainInitWithOptions(workDir, &gogit.PlainInitOptions{
				InitOptions: gogit.InitOptions{
					DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
				},
			})
			if err != nil {
				return nil, fmt.Errorf("initializing empty-remote clone: %w", err)
			}
			if _, err := repo2.CreateRemote(&config.RemoteConfig{
				Name: "origin",
				URLs: []string{remoteURL},
			}); err != nil {
				return nil, fmt.Errorf("configuring origin for empty remote: %w", err)
			}
			logger.Info("bootstrapped empty remote", "url", remoteURL, "path", workDir)
			return &Clone{
				workDir:   workDir,
				remoteURL: remoteURL,
				repo:      repo2,
				logger:    logger,
			}, nil
		}
		return nil, friendlyGitError("clone", remoteURL, err)
	}
	logger.Info("cloned project", "url", remoteURL, "path", workDir)
	return &Clone{
		workDir:   workDir,
		remoteURL: remoteURL,
		repo:      repo,
		logger:    logger,
	}, nil
}

// Pull fetches the latest state of the project's configured
// default branch and fast-forwards the local branch to match.
// If the working tree has uncommitted local changes (shouldn't
// happen in normal flow — clients should always commit what
// they wrote before yielding), Pull returns an error. The
// caller MUST hold the project lock.
//
// To pull a specific branch (typically the branch of a specific
// run), use PullBranch(branch) — this shorthand uses the
// project-level default.
func (p *Clone) Pull() error {
	return p.PullBranch("")
}

// PullBranch is the branch-aware variant of Pull. Pass "" to
// use the project's configured default branch.
//
// First-submit on a new branch: if origin has no
// refs/heads/<branch> yet, we return nil. go-git's Pull raises
// a "reference not found" error in that case, which would wedge
// the very first claim on e.g. branch="run-2" before any
// commits exist on the remote. The caller's next push creates
// the remote ref naturally.
func (p *Clone) PullBranch(branch string) error {
	if p.remoteURL == "" {
		return nil // local-only, nothing to pull
	}
	// Solo enju_init projects store the working-tree path as
	// remote_url on the coordinator (not an external bare),
	// so p.remoteURL is non-empty even though the local repo
	// has no `origin` configured. Treat that as local-only too
	// — without this guard, RemoteBranchHash below errors
	// with "no origin remote" and the failure cascades up to
	// the caller as a spurious remote-error message.
	if p.GitOriginURL() == "" {
		return nil
	}
	b := p.resolveBranch(branch)
	// Cheap ls-remote check so a brand-new branch doesn't
	// propagate a reference-not-found error. Any network /
	// auth failure here is passed through — we only swallow
	// the specific "branch doesn't exist yet" case.
	remoteSHA, err := p.RemoteBranchHash(b)
	if err != nil {
		return err
	}
	if remoteSHA == "" {
		return nil
	}
	wt, err := p.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}
	refName := plumbing.NewBranchReferenceName(b)
	err = wt.Pull(&gogit.PullOptions{
		RemoteName:    "origin",
		ReferenceName: refName,
		SingleBranch:  true,
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return friendlyGitError("pull", p.remoteURL, err)
	}
	return nil
}

// HeadHash returns the SHA of the current local HEAD.
func (p *Clone) HeadHash() (string, error) {
	ref, err := p.repo.Head()
	if err != nil {
		return "", fmt.Errorf("reading HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

// LocalBranchHash returns the SHA of the named local branch ref,
// falling back to refs/remotes/origin/<branch> when the local
// ref doesn't exist, and finally to empty string when neither
// exists. Used by the fetch-path reconcile hook to seed the
// scanner cursor for a freshly-created run branch (local ref
// exists at the base hash, no origin ref yet) without relying
// on the worktree's current HEAD — HEAD can point at a
// different branch after the user switches runs in the same
// session. Empty `branch` resolves through the project's
// configured default.
func (p *Clone) LocalBranchHash(branch string) (string, error) {
	b := p.resolveBranch(branch)
	localRef := plumbing.NewBranchReferenceName(b)
	if ref, err := p.repo.Reference(localRef, true); err == nil {
		return ref.Hash().String(), nil
	}
	remoteRef := plumbing.NewRemoteReferenceName("origin", b)
	if ref, err := p.repo.Reference(remoteRef, true); err == nil {
		return ref.Hash().String(), nil
	}
	return "", nil
}

// RemoteHeadHash contacts the remote via ls-remote and returns
// the SHA of the project's configured default branch, or empty
// string if the remote has no such ref. Used by CompareToRemote
// to compare local HEAD against the authoritative remote state
// without a full fetch.
func (p *Clone) RemoteHeadHash() (string, error) {
	return p.RemoteBranchHash("")
}

// RemoteBranchHash is the branch-aware variant of
// RemoteHeadHash. Pass "" to use the project's configured
// default.
func (p *Clone) RemoteBranchHash(branch string) (string, error) {
	if p.remoteURL == "" {
		return "", fmt.Errorf("no remote configured")
	}
	rem, err := p.repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("no origin remote: %w", err)
	}
	refs, err := rem.List(&gogit.ListOptions{
		Auth: sshAuthMethod(p.remoteURL),
	})
	if err != nil {
		return "", friendlyGitError("check remote status", p.remoteURL, err)
	}
	target := plumbing.NewBranchReferenceName(p.resolveBranch(branch))
	for _, r := range refs {
		if r.Name() == target {
			return r.Hash().String(), nil
		}
	}
	return "", nil
}

// ReadFile reads a file from the working tree at a repo-relative path.
// Used by the client-side template resolver to read upstream task
// results and artifact contents.
func (p *Clone) ReadFile(repoRelPath string) ([]byte, error) {
	full := filepath.Join(p.workDir, repoRelPath)
	return os.ReadFile(full)
}

// ReadFileAtCommit reads a file's contents as of a specific commit.
// Used by the template resolver when the caller wants the exact
// version associated with an upstream task's submitted commit SHA,
// rather than whatever happens to be in the working tree today.
func (p *Clone) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
	hash := plumbing.NewHash(commitSHA)
	commit, err := p.repo.CommitObject(hash)
	if err != nil {
		return nil, false, fmt.Errorf("loading commit %s: %w", commitSHA, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, false, fmt.Errorf("loading tree: %w", err)
	}
	file, err := tree.File(repoRelPath)
	if err == object.ErrFileNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("looking up file: %w", err)
	}
	content, err := file.Contents()
	if err != nil {
		return nil, false, fmt.Errorf("reading contents: %w", err)
	}
	return []byte(content), true, nil
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
	Trailers EnjuTrailers

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
	// Single-submit thin wrapper: the post-push HEAD is this
	// submit's commit (possibly rewritten by a rebase
	// retry). Batch callers prepare N commits then call
	// PushPendingCommits + CommitSHAsByTaskID directly so
	// each entry's post-rebase SHA can be recovered.
	if _, err := p.PrepareCommit(req); err != nil {
		return nil, err
	}
	attempts, finalSHA, err := p.PushPendingCommits(req.Branch, req.MaxRetries)
	if err != nil {
		return nil, err
	}
	advanceCursorIfConfigured(req.ProjectID, req.StateDir, p.resolveBranch(req.Branch), finalSHA)
	return &SubmitResult{CommitSHA: finalSHA, Attempts: attempts}, nil
}

// PrepareCommit overlays a submit request's files onto the
// working tree and creates a local commit, WITHOUT pushing.
// Used by the batch submit path — N PrepareCommit calls
// accumulate commits on the branch, then a single
// PushPendingCommits flushes them in one network round-trip.
//
// The caller MUST hold the project lock for the duration AND
// across the subsequent PushPendingCommits. Rebase rewriting
// commits concurrently with an un-pushed local chain would
// produce commits that aren't connected to the post-rebase
// tip.
//
// Returns the pre-push commit SHA. If PushPendingCommits
// triggers a rebase, the SHA will change — use
// CommitSHAsByTaskID after the push to recover the final SHA
// by matching the Enju-Task-Id trailer.
//
// Single-submit callers can still use SubmitTaskResult, which
// composes PrepareCommit + PushPendingCommits internally and
// returns the post-push SHA directly.
func (p *Clone) PrepareCommit(req SubmitRequest) (string, error) {
	if len(req.Files) == 0 {
		return "", fmt.Errorf("no files to write")
	}
	if req.TaskID == "" || req.Username == "" {
		return "", fmt.Errorf("TaskID and Username are required")
	}

	// Default trailers carry at minimum the task id so the
	// fetch-path scanner can pick any task-submit commit up
	// without the caller having to opt in. The Enju-Task-Id
	// trailer is also the batch path's hook for mapping
	// post-rebase SHAs back to the originating submit
	// entry — see CommitSHAsByTaskID.
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
	if err := p.CheckoutBranchFrom(req.Branch, req.BaseBranch); err != nil {
		return "", fmt.Errorf("switching to branch %q: %w", p.resolveBranch(req.Branch), err)
	}

	// Overlay the new files on top of current HEAD. Any local
	// commits the user made (e.g. a manual edit to a script
	// between submits) stay where they are — we do NOT reset
	// HEAD to origin/<default> any more. That reset was the
	// root cause of the "fat-client clobbers user commits" bug.
	for _, f := range req.Files {
		full := filepath.Join(p.workDir, f.RepoRelPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return "", fmt.Errorf("creating dir for %s: %w", f.RepoRelPath, err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0644
		}
		if err := os.WriteFile(full, f.Content, mode); err != nil {
			return "", fmt.Errorf("writing %s: %w", f.RepoRelPath, err)
		}
		// WriteFile respects umask on creation but existing
		// files keep their prior mode. Explicit chmod so the
		// requested mode actually sticks across overwrite-in-
		// place cases (re-submit, re-snapshot).
		if err := os.Chmod(full, mode); err != nil {
			return "", fmt.Errorf("chmod %s: %w", f.RepoRelPath, err)
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
	sha, err := p.commit(commitMsg, stagePaths, req.AuthorName, req.AuthorEmail)
	if err != nil {
		return "", fmt.Errorf("creating commit: %w", err)
	}
	return sha, nil
}

// PushPendingCommits pushes the current local branch tip to
// origin with retry-on-non-fast-forward. Returns (attempts,
// final HEAD SHA, error). The final SHA is whatever HEAD
// resolves to after a successful push — for a single-entry
// submit that's the task's commit; for a batch of N it's the
// latest entry's post-rebase commit.
//
// maxRetries <= 0 defaults to 3. Caller must hold the
// project lock.
func (p *Clone) PushPendingCommits(branch string, maxRetries int) (int, string, error) {
	if maxRetries <= 0 {
		maxRetries = 3
	}
	// Push with retry. On non-FF rejection, rebase our chain
	// onto the new remote tip and try again. Every attempt
	// preserves local history — only a real path conflict can
	// stop the rebase, and we surface that as an error rather
	// than discard work. Batch callers piggyback on this:
	// multiple local commits rebase together in one go, so
	// the rebase cost is paid once no matter how many tasks
	// are in the batch.
	for attempt := 1; attempt <= maxRetries; attempt++ {
		pushErr := p.pushBranchInternal(branch, false)
		if pushErr == nil {
			sha := ""
			if head, herr := p.repo.Head(); herr == nil {
				sha = head.Hash().String()
			}
			return attempt, sha, nil
		}
		if !isNonFastForwardError(pushErr) {
			return attempt, "", fmt.Errorf("push failed: %w", pushErr)
		}
		// Non-FF: the remote moved while we were working.
		// `git pull --rebase --autostash` fetches the new
		// commits and replays our local commits (user's +
		// ours) on top. Uses the system git CLI because
		// go-git's rebase support doesn't cover the cases we
		// need (especially divergent-history with
		// user-authored commits).
		if rebaseErr := p.rebaseOnRemote(branch); rebaseErr != nil {
			return attempt, "", fmt.Errorf(
				"submit push rejected and rebase failed — your submit likely touches a file another client also changed. Local work is still in git reflog. Details: %w",
				rebaseErr)
		}
	}
	return maxRetries, "", fmt.Errorf("submit failed after %d push attempts", maxRetries)
}

// ResetBranchToHash hard-resets the current branch to the
// given commit hash, discarding any local commits + staged
// and unstaged working-tree changes in between. Used by the
// batch submit path to roll back a partially-prepared chain
// when one entry's PrepareCommit fails mid-loop — without
// the reset, a retry would layer a second set of commits
// alongside the original orphans, producing git log noise
// with duplicate Enju-Task-Complete trailers per task.
//
// Caller must hold the project lock.
func (p *Clone) ResetBranchToHash(hash string) error {
	if hash == "" {
		return fmt.Errorf("empty hash")
	}
	wt, err := p.repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if err := wt.Reset(&gogit.ResetOptions{
		Mode:   gogit.HardReset,
		Commit: plumbing.NewHash(hash),
	}); err != nil {
		return fmt.Errorf("hard reset to %s: %w", hash, err)
	}
	return nil
}

// ResetWorktreeToCleanState wipes any worktree residue —
// staged changes, unstaged changes to tracked files, and
// untracked files — leaving the working tree byte-identical
// to whatever HEAD currently points at. Used by the bot
// daemon between task iterations: a previous task's
// `claude -p` may have left uncommitted edits or scratch
// files behind, and the next iteration's CheckoutBranchFrom
// can fail when those leftovers collide with the new topic
// branch's index. The "no user state to preserve" property
// of bot clones (system-managed, at <project>/enju/.clone/)
// makes the wipe safe; operator-side clones MUST NOT call
// this method — the operator's working tree may carry
// intentional uncommitted WIP.
//
// Implementation:
//   - HardReset to HEAD drops staged + unstaged changes to
//     tracked files. The index resyncs to HEAD's tree, so a
//     "staged deletion" left over from a previous bad
//     checkout disappears here.
//   - A second walk removes untracked files. We can't use
//     gogit's CheckoutOptions.Force (it nukes untracked
//     across the whole tree but doesn't preserve `.git/`
//     itself reliably across go-git versions); a manual walk
//     skipping infra dirs is more conservative.
//
// Caller MUST hold the project lock.
func (p *Clone) ResetWorktreeToCleanState() error {
	wt, err := p.repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	head, err := p.repo.Head()
	if err != nil {
		return fmt.Errorf("HEAD: %w", err)
	}
	if err := wt.Reset(&gogit.ResetOptions{
		Mode:   gogit.HardReset,
		Commit: head.Hash(),
	}); err != nil {
		return fmt.Errorf("hard reset to HEAD: %w", err)
	}

	// Build the tracked-paths set so we can identify untracked
	// entries during the walk. The set covers everything in
	// the post-reset index, which equals HEAD's tree.
	idx, err := p.repo.Storer.Index()
	if err != nil {
		return fmt.Errorf("reading index: %w", err)
	}
	tracked := make(map[string]struct{}, len(idx.Entries))
	for _, e := range idx.Entries {
		tracked[e.Name] = struct{}{}
	}

	return filepath.Walk(p.workDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if path == p.workDir {
			return nil
		}
		// Skip infrastructure dirs at any depth: `.git/` (this
		// repo's own state), `.bare.git/` and `.clone/` (when
		// we're walking a parent that happens to host them),
		// and any in-progress preserve dir from a crashed
		// checkout (recovered separately via openOrClone).
		base := filepath.Base(path)
		if info.IsDir() {
			if base == ".git" || base == ".bare.git" || base == ".clone" ||
				strings.HasSuffix(base, preserveDirSuffix) {
				return filepath.SkipDir
			}
			return nil
		}
		// Files only past this point.
		relOS, rerr := filepath.Rel(p.workDir, path)
		if rerr != nil {
			return rerr
		}
		rel := filepath.ToSlash(relOS)
		if _, ok := tracked[rel]; ok {
			return nil
		}
		// Untracked file — remove. Best-effort; a removal
		// failure (permission denied on a symlink we can't
		// resolve, etc.) doesn't fail the whole reset since
		// the residual file may not actually collide with the
		// next checkout. Log via the project's logger if set.
		if rmErr := os.Remove(path); rmErr != nil && p.logger != nil {
			p.logger.Warn("clone reset: couldn't remove untracked file",
				"path", rel, "error", rmErr)
		}
		return nil
	})
}

// CommitSHAsByTaskID walks HEAD backward for up to maxWalk
// commits, matching each commit's Enju-Task-Id trailer
// against the supplied task ids. Returns a map of taskID →
// current SHA. Used by the batch submit path after
// PushPendingCommits to recover post-rebase SHAs: a rebase
// rewrites commits, changing every SHA, but the commit
// message (including the trailer) is preserved. The trailer
// is therefore the stable key through a rebase.
//
// maxWalk <= 0 defaults to a safe upper bound; the batch
// caller passes len(entries) × 2 to tolerate interleaved
// user commits on the same branch.
//
// Missing task ids (no commit found with that trailer
// within the walk window) are simply absent from the map;
// the caller decides whether that's an error.
func (p *Clone) CommitSHAsByTaskID(taskIDs []string, maxWalk int) (map[string]string, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	if maxWalk <= 0 {
		maxWalk = len(taskIDs)*2 + 16
	}
	wanted := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		wanted[id] = true
	}
	out := make(map[string]string, len(taskIDs))
	iter, err := p.repo.Log(&gogit.LogOptions{})
	if err != nil {
		return nil, fmt.Errorf("log walk: %w", err)
	}
	defer iter.Close()
	for i := 0; i < maxWalk && len(out) < len(wanted); i++ {
		commit, err := iter.Next()
		if err != nil {
			break
		}
		taskID := extractTaskIDTrailer(commit.Message)
		if taskID == "" || !wanted[taskID] {
			continue
		}
		if _, already := out[taskID]; already {
			continue // keep the newest occurrence — iter walks newest-first
		}
		out[taskID] = commit.Hash.String()
	}
	return out, nil
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
	prefix := TrailerTaskComplete + ":"
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
	if err := p.CheckoutBranch(req.Branch); err != nil {
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

	for attempt := 1; attempt <= maxRetries; attempt++ {
		pushErr := p.pushBranchInternal(req.Branch, false)
		if pushErr == nil {
			if head, herr := p.repo.Head(); herr == nil {
				sha = head.Hash().String()
			}
			return &CommitFilesResult{CommitSHA: sha, Attempts: attempt}, nil
		}
		if !isNonFastForwardError(pushErr) {
			return nil, fmt.Errorf("push failed: %w", pushErr)
		}
		if rebaseErr := p.rebaseOnRemote(req.Branch); rebaseErr != nil {
			return nil, fmt.Errorf(
				"push rejected and rebase failed — local work is still in git reflog. Details: %w",
				rebaseErr)
		}
	}
	return nil, fmt.Errorf("commit failed after %d push attempts", maxRetries)
}

// CheckoutBranch switches the working tree to `branch`, creating
// it locally if it doesn't exist. The creation path bases the
// new branch on whatever's currently checked out (typically the
// project's default branch), so an unseen `run-2` branches off
// from the tip of `main` — matching the "branches as isolated
// run workspaces" mental model.
//
// Idempotent — a no-op when HEAD is already on `branch`. The
// caller MUST hold the project lock.
func (p *Clone) CheckoutBranch(branch string) error {
	return p.CheckoutBranchFrom(branch, "")
}

// CheckoutBranchFrom is the base-branch-aware variant of
// CheckoutBranch. When `baseBranch` is non-empty AND `branch`
// doesn't yet exist locally or as a remote-tracking ref, the new
// branch is forked from the tip of `baseBranch` (resolved via
// origin/<baseBranch>, then the local refs/heads/<baseBranch>).
// Used by the living-workflow phase 6b.1 topic-branch flow:
// per-iteration topic branches fork from the run branch tip,
// not from origin/main, so iter-1's commits land on top of the
// run branch's current state.
//
// When `baseBranch` is empty, behavior is identical to
// CheckoutBranch — the project's default base (origin/main, then
// origin/<remote HEAD>, then root) is used as the fork point.
//
// Caller MUST hold the project lock.
func (p *Clone) CheckoutBranchFrom(branch, baseBranch string) error {
	target := p.resolveBranch(branch)
	wt, err := p.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}
	refName := plumbing.NewBranchReferenceName(target)
	// Already on this branch? No-op.
	if head, err := p.repo.Head(); err == nil && head.Name() == refName {
		return nil
	}
	// Does the branch exist locally (or track a known remote)?
	// Simple checkout, no fork-from dance.
	if _, err := p.repo.Reference(refName, true); err == nil {
		return wt.Checkout(&gogit.CheckoutOptions{Branch: refName})
	}
	// Branch doesn't exist yet. Fork it from the project's
	// BASE — not from workspace HEAD. Forking from HEAD silently
	// inherits whatever branch was checked out last, which was
	// the tester-reported "enju/work got created from run-1's
	// tip instead of main" bug. "Project base" here = origin/main
	// when available (the conventional seed), falling back to
	// origin/<HEAD>, then the repo's root commit. This gives
	// every new branch a clean, predictable ancestor.
	//
	// When the caller passed an explicit baseBranch (phase 6b.1
	// topic-branch flow), prefer that as the fork point so a
	// per-iteration branch lands on top of the run branch's
	// current tip. Falls back to the default base resolution
	// when baseBranch is empty or its tip can't be resolved.
	var baseHash plumbing.Hash
	if baseBranch != "" {
		if h, ok := p.resolveBaseBranchHash(baseBranch); ok {
			baseHash = h
		}
	}
	if baseHash.IsZero() {
		h, err := p.branchBaseHash()
		if err != nil {
			return fmt.Errorf("resolving base for new branch %q: %w", target, err)
		}
		baseHash = h
	}
	// Create the branch ref at the base hash, then point HEAD
	// at it. go-git's Worktree.Checkout with Create=true uses
	// current HEAD as the starting point; doing the ref dance
	// manually lets us fork from a different commit.
	branchRef := plumbing.NewHashReference(refName, baseHash)
	if err := p.repo.Storer.SetReference(branchRef); err != nil {
		return fmt.Errorf("creating branch ref %s: %w", target, err)
	}
	// Checkout the new branch's tree with Force so files
	// tracked on the PREVIOUS branch but not on the new one
	// get removed from the worktree. Without Force, go-git
	// bails on "unstaged changes" (the prior branch's tracked
	// files look like unstaged removals from the new branch's
	// POV), OR with Keep:true silently carries those files
	// into the next submit — which was the tester-reported
	// "lane-b inherits lane-a's commits" leak.
	//
	// Force:true in go-git ALSO wipes untracked files (different
	// from `git checkout --force` CLI, which only overwrites
	// conflicting paths) — an earlier version of this code lost
	// a tester's in-progress template directory to exactly that
	// behavior. To get Force's branch-isolation AND preserve
	// user-authored scratch / gitignored artifacts, we rename
	// non-tracked paths out of workDir before the checkout and
	// rename them back after. The move is a metadata-only
	// inode-table update, so a 50 GB untracked BAM file (common
	// in bio workflows) moves in microseconds — no memory blow-
	// up, no data copy. See preserve.go for the full rationale.
	//
	// Concurrency: this operation runs under the caller's
	// proj.Lock(), so the preserve dir can't collide with
	// another CheckoutBranch on the same project.
	preserveDir := p.workDir + preserveDirSuffix

	// Drain any leftover preserve dir BEFORE renaming fresh
	// content into it. Two motivating scenarios:
	//
	//   - In-process: a prior CheckoutBranchFrom in this same
	//     daemon failed mid-restore (rename collision, FS
	//     error). The preserve dir stayed on disk; the cached
	//     *Clone never re-runs openOrClone, so its recovery
	//     hook never fires. Without this drain the next
	//     checkout's movePreserveNonTracked would rename fresh
	//     paths INTO the same dir, mixing two unrelated
	//     preservations — and `os.Rename` over an existing
	//     non-empty dir errors out unpredictably across OSes.
	//
	//   - Cross-process: the user reported preserve dirs
	//     surviving across daemon restarts when the next open
	//     went through OpenExisting (read-only path) rather
	//     than openOrClone. Belt-and-braces — OpenExisting now
	//     also drains, but the lock-held checkout path is the
	//     one that absolutely cannot proceed with a dirty
	//     preserve dir.
	//
	// Recovery is best-effort: it restores anything that
	// doesn't collide with current branch content. Files that
	// collide stay in the preserve dir — the new checkout
	// then fails fast (next block) rather than writing more
	// state into a dirty dir.
	if _, statErr := os.Stat(preserveDir); statErr == nil {
		if recErr := recoverLeftoverPreserve(p.workDir, p.logger); recErr != nil {
			return fmt.Errorf(
				"leftover preserve dir at %s couldn't be drained: %w — review files there and remove (or move aside) before retrying checkout",
				preserveDir, recErr,
			)
		}
		// recoverLeftoverPreserve cleans up empty dirs but
		// leaves files that collide with current branch
		// content. If anything remains, fail loud rather than
		// risk merging unrelated preservations.
		if _, statErr := os.Stat(preserveDir); statErr == nil {
			return fmt.Errorf(
				"leftover preserve dir at %s holds files that conflict with the current branch — review and remove (or move aside) before retrying checkout",
				preserveDir,
			)
		}
	}

	manifest, err := movePreserveNonTracked(p.repo, p.workDir, preserveDir)
	if err != nil {
		// Refuse the checkout on partial-preserve failure.
		// The preserve dir remains on disk for manual
		// inspection / next-session crash recovery. This is
		// the "don't touch git state if preservation is half-
		// done" safety default — rollback has its own failure
		// modes (rename-back might fail for the same reason
		// the forward rename did).
		return fmt.Errorf(
			"preserving non-tracked files before checkout failed: %w — partial state at %q; next workspace open will attempt recovery",
			err, preserveDir,
		)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{
		Branch: refName,
		Force:  true,
	}); err != nil {
		// Checkout failed — restore what we moved so the
		// workspace is usable again.
		_, _ = restoreFromPreserve(p.workDir, preserveDir, manifest)
		return err
	}
	conflicts, restoreErr := restoreFromPreserve(p.workDir, preserveDir, manifest)
	if restoreErr != nil {
		return fmt.Errorf("restoring non-tracked files after checkout: %w", restoreErr)
	}
	if len(conflicts) > 0 && p.logger != nil {
		p.logger.Warn(
			"branch switch preserved non-tracked paths, but some now conflict with tracked paths on the new branch; preserved copies remain in the preserve dir for manual review",
			"preserve_dir", preserveDir,
			"conflict_entries", len(conflicts),
			"conflict_files", countConflictFiles(preserveDir, conflicts),
		)
	}
	return nil
}

// resolveBaseBranchHash looks up the tip commit of a named base
// branch for the topic-branch flow. Prefers the LOCAL ref
// refs/heads/<baseBranch> over the origin-tracking ref because
// recent local commits (template auto-commits on first
// create_run, batch submits not yet pushed) live on the local
// branch BEFORE origin learns about them — and forking the
// topic from a stale origin/<baseBranch> would produce a
// branch whose history misses those commits, breaking the FF
// invariant when we later merge topic back onto the local run
// branch. Returns (hash, true) on success; (zero, false) when
// neither ref exists, letting the caller fall back to the
// default base resolution.
func (p *Clone) resolveBaseBranchHash(baseBranch string) (plumbing.Hash, bool) {
	if ref, err := p.repo.Reference(plumbing.NewBranchReferenceName(baseBranch), true); err == nil {
		return ref.Hash(), true
	}
	if ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", baseBranch), true); err == nil {
		return ref.Hash(), true
	}
	return plumbing.ZeroHash, false
}

// FastForwardBranchToCommit advances `branch` to `commitSHA`
// locally and on origin, refusing if the move isn't a fast-
// forward. Used by the living-workflow phase 6b foundational
// auto-merge: after a topic-branch commit lands and the
// coordinator marks the task ACCEPTED, the fat-client FF-pushes
// the topic SHA onto the run branch so all downstream readers
// see the canonical state on the run branch.
//
// FF-only is the v1 contract — under linear progression
// (depends_on must be merged before downstream starts) a
// non-FF here means a parallel iteration ran without proper
// gating, which is a workflow bug we want to surface loudly
// rather than silently rewrite history. v2 will introduce
// rebase / spawn-resolve_conflict on this path.
//
// Caller MUST hold the project lock.
func (p *Clone) FastForwardBranchToCommit(branch, commitSHA string) error {
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	if commitSHA == "" {
		return fmt.Errorf("commit SHA is required")
	}
	hash := plumbing.NewHash(commitSHA)
	if hash.IsZero() {
		return fmt.Errorf("invalid commit SHA %q", commitSHA)
	}

	// Verify FF locally first: the current branch ref (or its
	// origin-tracking ref, if the local ref doesn't exist yet)
	// must be an ancestor of the target commit. Refuses with a
	// clear message rather than letting the remote reject in a
	// less-readable form.
	refName := plumbing.NewBranchReferenceName(branch)
	var currentHash plumbing.Hash
	if ref, err := p.repo.Reference(refName, true); err == nil {
		currentHash = ref.Hash()
	} else if ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true); err == nil {
		currentHash = ref.Hash()
	}
	if !currentHash.IsZero() && currentHash != hash {
		isAncestor, ancErr := p.commitIsAncestor(currentHash, hash)
		if ancErr != nil {
			return fmt.Errorf("verifying fast-forward of %q: %w", branch, ancErr)
		}
		if !isAncestor {
			return fmt.Errorf(
				"refusing non-fast-forward update of %q from %s to %s — "+
					"the run branch has diverged from the topic branch, which under "+
					"linear progression should never happen. Investigate parallel "+
					"iterations or manual ref edits before retrying",
				branch, currentHash.String()[:8], hash.String()[:8])
		}
	}

	// Update the local ref so subsequent reads see the merged
	// state. Idempotent when already at target.
	if currentHash != hash {
		if err := p.repo.Storer.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
			return fmt.Errorf("updating local %q to %s: %w", branch, hash, err)
		}
	}

	// Push the same SHA to the remote branch ref. Using the
	// non-force form means the remote will reject any non-FF
	// update — a defense-in-depth check on top of the local
	// ancestry verify. The remote rejection message is less
	// readable than ours, so we treat the remote-side reject as
	// a separate hard error class.
	if p.remoteURL == "" {
		return nil
	}
	refSpec := fmt.Sprintf("%s:refs/heads/%s", hash.String(), branch)
	pushErr := p.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(refSpec)},
		Auth:       sshAuthMethod(p.remoteURL),
	})
	p.lastPushAt = time.Now()
	if pushErr != nil && pushErr != gogit.NoErrAlreadyUpToDate {
		p.lastPushError = pushErr.Error()
		return friendlyGitError("ff-push", p.remoteURL, pushErr)
	}
	p.lastPushError = ""
	return nil
}

// commitIsAncestor reports whether `ancestor` is reachable
// from `descendant` via parent links — the standard "is X a
// fast-forward of Y" check. Used by FastForwardBranchToCommit
// before any ref update.
func (p *Clone) commitIsAncestor(ancestor, descendant plumbing.Hash) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}
	descCommit, err := p.repo.CommitObject(descendant)
	if err != nil {
		return false, fmt.Errorf("loading descendant commit %s: %w", descendant, err)
	}
	ancCommit, err := p.repo.CommitObject(ancestor)
	if err != nil {
		return false, fmt.Errorf("loading ancestor commit %s: %w", ancestor, err)
	}
	// go-git semantics: X.IsAncestor(Y) reports whether X is
	// reachable from Y by walking parent links — i.e. "is X an
	// ancestor of Y." So to ask "is ancestor an ancestor of
	// descendant" we call ancCommit.IsAncestor(descCommit).
	return ancCommit.IsAncestor(descCommit)
}

// branchBaseHash picks the commit a fresh branch should fork
// from when the caller asks for a branch that doesn't exist
// yet. Preference order:
//
//  1. refs/heads/<default> locally — the project's actual default
//     branch tip. Local-first matters for solo projects (no
//     remote post-Option-B) where origin/* refs simply don't
//     exist; without this rung the fallback walked back to the
//     root commit and run branches forked from the very first
//     commit on the timeline, requiring manual `git merge main`
//     to recover. Also matters when a fresh local commit hasn't
//     been pushed yet — origin/* would be stale.
//  2. origin/<default> — for projects with a remote, the canonical
//     seed when the local ref is somehow absent (fresh clone with
//     no checkout yet).
//  3. origin/<remote HEAD symbolic ref> — when a repo was
//     init'd with a non-default-named default (e.g. git init
//     --initial-branch=trunk).
//  4. The repo's root commit — last-ditch fallback that
//     always yields a valid hash.
//
// Deliberately does NOT fall back to the caller's current HEAD,
// which would reintroduce the "silent inheritance" bug.
func (p *Clone) branchBaseHash() (plumbing.Hash, error) {
	defaultBranch := p.defaultBranchOr()
	// Local default branch first — covers solo projects (no
	// remote) AND the "haven't pushed the latest commit yet"
	// case where origin/* would be behind HEAD.
	if ref, err := p.repo.Reference(plumbing.NewBranchReferenceName(defaultBranch), true); err == nil {
		return ref.Hash(), nil
	}
	// Then origin/<default> — fresh clone where the local ref
	// hasn't been instantiated yet but the remote-tracking ref
	// is already populated by the initial fetch.
	if ref, err := p.repo.Reference(plumbing.NewRemoteReferenceName("origin", defaultBranch), true); err == nil {
		return ref.Hash(), nil
	}
	// Try the remote's HEAD symref — covers repos whose
	// default isn't named the same as p.defaultBranch (trunk,
	// master, etc.) and the project record hasn't caught up.
	if ref, err := p.repo.Reference(plumbing.NewRemoteHEADReferenceName("origin"), true); err == nil {
		return ref.Hash(), nil
	}
	// Root commit fallback: walk to the very first commit
	// reachable from any branch. The project always has a
	// seed commit, so this succeeds except for the empty-repo
	// edge case — which is exactly the "auto-local bare was
	// not seeded" regression path. Surface the workspace +
	// remote paths so the user can inspect what went wrong
	// instead of chasing "log: reference not found" in isolation.
	iter, err := p.repo.Log(&gogit.LogOptions{})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf(
			"local clone at %s has no refs to branch from (remote %q is empty or unseeded). "+
				"This typically means enju_create_project's auto-local bare init failed silently. "+
				"Check the coordinator's remote_url via enju_project_remote_status, or re-create the project",
			p.workDir, p.remoteURL)
	}
	defer iter.Close()
	var root plumbing.Hash
	for {
		c, err := iter.Next()
		if err != nil {
			break
		}
		root = c.Hash
	}
	if root.IsZero() {
		return plumbing.ZeroHash, fmt.Errorf(
			"local clone at %s has no commits to branch from (remote %q is empty). "+
				"enju_create_project is supposed to seed the bare repo with an initial commit — "+
				"this usually means that step failed silently. Re-create the project or set a "+
				"valid remote via enju_set_project_remote",
			p.workDir, p.remoteURL)
	}
	return root, nil
}

// pushBranchInternal is the branch-aware equivalent of
// pushInternal. It pushes only the named branch to origin.
// Empty `branch` resolves to the project default.
func (p *Clone) pushBranchInternal(branch string, force bool) error {
	if p.remoteURL == "" {
		return nil
	}
	b := p.resolveBranch(branch)
	refSpec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", b, b)
	err := p.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(refSpec)},
		Force:      force,
		Auth:       sshAuthMethod(p.remoteURL),
	})
	p.lastPushAt = time.Now()
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		p.lastPushError = err.Error()
		return friendlyGitError("push", p.remoteURL, err)
	}
	p.lastPushError = ""
	return nil
}

// rebaseOnRemote runs `git pull --rebase --autostash` via the
// system git binary so divergent histories are merged without
// discarding local commits. go-git's rebase support is too
// limited for this case — it doesn't handle arbitrary
// divergent-history replays, which is exactly what we need
// when the user has committed between submits.
//
// --autostash protects against an edge case where the caller
// left uncommitted changes in the working tree; they get
// stashed + reapplied around the rebase so we never surprise
// the user with a dirty-tree rejection.
//
// Pulls the specific branch the caller was pushing to — passing
// "" resolves to the project's configured default. No-op for
// local-only projects (no remoteURL).
func (p *Clone) rebaseOnRemote(branch string) error {
	if p.remoteURL == "" {
		return nil
	}
	b := p.resolveBranch(branch)
	cmd := exec.Command("git", "-C", p.workDir, "pull", "--rebase", "--autostash", "origin", b)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull --rebase origin %s: %s (%w)", b, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// isNonFastForwardError tells whether a push error is the
// "someone else pushed first" case (recoverable via rebase) vs
// a real network / auth / config failure (not recoverable,
// surface to the user). go-git surfaces non-FF via a few
// different phrasings depending on the transport; check them
// all.
func isNonFastForwardError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "non-fast-forward") ||
		strings.Contains(s, "fetch first") ||
		strings.Contains(s, "rejected") ||
		strings.Contains(s, "stale info")
}

// EnsureBundleOnDefault pins a template bundle directory to the
// project's default branch. This is the template-as-recipe
// invariant: templates live on the default branch, runs on any
// other branch read them from there.
//
// Flow: switch to default, pull so we commit onto the current
// tip, stage every file under bundleDir (recursively — go-git's
// AddGlob isn't always recursive so we walk), commit+push if
// anything staged. No-op when the bundle is already clean.
//
// Returns the HEAD SHA on default after the operation so
// callers can persist it as source_commit_sha — whether or not
// an auto-commit actually fired.
//
// The caller MUST hold the project lock.
func (p *Clone) EnsureBundleOnDefault(bundleDir, authorName, authorEmail, modelName string) (string, error) {
	if err := p.CheckoutBranch(""); err != nil {
		return "", fmt.Errorf("switching to default branch: %w", err)
	}
	if err := p.PullBranch(""); err != nil {
		return "", fmt.Errorf("pulling default branch: %w", err)
	}
	wt, err := p.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("getting worktree: %w", err)
	}
	// Walk the bundle dir and stage each file. Using per-file
	// Add keeps staging scoped to the bundle — a wider
	// AddGlob(".") would accidentally commit unrelated
	// worktree changes the user might have in progress.
	absBundle := filepath.Join(p.workDir, bundleDir)
	if _, err := os.Stat(absBundle); err != nil {
		return "", fmt.Errorf("bundle dir %q not found: %w", bundleDir, err)
	}
	walkErr := filepath.Walk(absBundle, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(p.workDir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if _, err := wt.Add(rel); err != nil {
			return fmt.Errorf("staging %s: %w", rel, err)
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	status, err := wt.Status()
	if err != nil {
		return "", fmt.Errorf("status: %w", err)
	}
	// Anything staged under the bundle path? If not, clean
	// exit — return the current HEAD.
	anyStaged := false
	for path, s := range status {
		if !strings.HasPrefix(filepath.ToSlash(path), bundleDir+"/") && filepath.ToSlash(path) != bundleDir {
			continue
		}
		if s.Staging != gogit.Unmodified {
			anyStaged = true
			break
		}
	}
	if !anyStaged {
		if h, herr := p.repo.Head(); herr == nil {
			return h.Hash().String(), nil
		}
		return "", nil
	}
	msg := fmt.Sprintf("Commit template bundle %s", bundleDir)
	if modelName != "" {
		msg += "\n\nAI-Model: " + modelName
	}
	if authorName == "" {
		authorName = "Enju Client"
	}
	if authorEmail == "" {
		authorEmail = "enju-client@localhost"
	}
	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("committing bundle: %w", err)
	}
	if err := p.pushBranchInternal("", false); err != nil {
		return "", fmt.Errorf("pushing default branch: %w", err)
	}
	return hash.String(), nil
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

// push pushes the local main branch to origin with safe (non-force)
// semantics. Uses ambient credentials via go-git's default AuthMethod
// (SSH agent for SSH URLs, credential helpers for HTTPS). Returns nil
// for local-only projects. Private helper used by SubmitTaskResult.
func (p *Clone) push() error {
	return p.pushInternal(false)
}

// LocalBranches returns the short names of every refs/heads/*
// in the local repository. Used when we need to enumerate
// branches without a coordinator round-trip — e.g. resetting
// scan cursors after a project's remote URL changes.
func (p *Clone) LocalBranches() ([]string, error) {
	iter, err := p.repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("listing branches: %w", err)
	}
	defer iter.Close()
	var names []string
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		names = append(names, ref.Name().Short())
		return nil
	})
	if err != nil {
		return names, fmt.Errorf("iterating branches: %w", err)
	}
	return names, nil
}

// PushAllLocalBranches pushes every refs/heads/* to origin in a
// single push, with explicit refspec so it doesn't depend on
// per-branch upstream tracking config. Used when a freshly
// configured (or freshly changed) origin needs to be seeded
// with the workspace's existing branch state — e.g. a late-add
// of an origin to a project that already has run-branch
// commits, where the regular Push() default refspec might miss
// branches without configured upstream.
//
// Caller MUST hold the project lock.
func (p *Clone) PushAllLocalBranches() error {
	if p.remoteURL == "" {
		return fmt.Errorf("no remote configured")
	}
	err := p.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"refs/heads/*:refs/heads/*"},
		Auth:       sshAuthMethod(p.remoteURL),
	})
	p.lastPushAt = time.Now()
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		p.lastPushError = err.Error()
		return friendlyGitError("push", p.remoteURL, err)
	}
	p.lastPushError = ""
	return nil
}

// Push is the public safe-push entry point for the MCP tool handler
// that performs on-demand sync (enju_project_sync). It pushes the
// local HEAD to origin with go-git's default (non-force) semantics,
// so a non-fast-forward state is rejected by the remote. The caller
// MUST hold the project lock.
func (p *Clone) Push() error { return p.pushInternal(false) }

// PushForce is the destructive counterpart to Push: overwrites the
// remote branch even when histories have diverged. Only called from
// the explicit force-sync path, never from the normal submit flow.
// The caller MUST hold the project lock.
func (p *Clone) PushForce() error { return p.pushInternal(true) }

// pushInternal is the shared implementation behind Push / PushForce /
// the private submit-time push. Returns nil for local-only projects
// so callers can uniformly call it regardless of whether a remote is
// configured.
//
// No RefSpecs — pushes every matching local branch to origin. In a
// branch-per-run world a project can have many branches with local
// commits that haven't shipped yet; a narrow default-branch-only
// push would silently leave run-branch work behind on
// enju_project_sync. Submit / CommitFiles paths that want to
// target a single specific branch use pushBranchInternal instead.
func (p *Clone) pushInternal(force bool) error {
	if p.remoteURL == "" {
		return nil
	}
	err := p.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		Force:      force,
		Auth:       sshAuthMethod(p.remoteURL),
	})
	// Record the outcome for the project_remote_status diagnostic,
	// regardless of whether this was a success or a failure.
	p.lastPushAt = time.Now()
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		p.lastPushError = err.Error()
		return friendlyGitError("push", p.remoteURL, err)
	}
	p.lastPushError = ""
	return nil
}

// LastPushAt returns the timestamp of the most recent push attempt
// (success or failure). When no push has happened during the
// current MCP client process lifetime — which includes fresh
// sessions where the user runs `enju_project_remote_status`
// before submitting anything — it falls back to the HEAD commit's
// timestamp as a proxy for "last known successful push." The HEAD
// commit time is a conservative lower bound: any push since the
// last local commit advanced origin/main to HEAD, so the HEAD
// time is at or before the actual push time.
//
// Returns the zero value only if there's no session push AND no
// HEAD commit (fresh empty-remote bootstrap case).
func (p *Clone) LastPushAt() time.Time {
	if !p.lastPushAt.IsZero() {
		return p.lastPushAt
	}
	ref, err := p.repo.Head()
	if err != nil {
		return time.Time{}
	}
	commit, err := p.repo.CommitObject(ref.Hash())
	if err != nil {
		return time.Time{}
	}
	return commit.Author.When
}

// LastPushError returns the error message from the most recent
// push attempt, or the empty string if the last push succeeded.
func (p *Clone) LastPushError() string { return p.lastPushError }

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
	if p.GitOriginURL() == "" {
		r.Status = RemoteNoRemote
		return r, nil
	}

	localHash, _ := p.HeadHash()
	r.LocalHead = localHash

	remoteHash, err := p.RemoteHeadHash()
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

// --- Artifact history (git log per file) ---

// CommitInfo describes a single commit for history-walking purposes.
// Returned by LogFile in reverse-chronological order (newest first).
type CommitInfo struct {
	Hash    string
	Message string
	Author  string
	Time    time.Time
}

// LogFile returns the commits that touched a specific file in the
// local clone, newest-first. Used by the MCP client's
// enju_get_artifact_history tool to render per-file provenance
// without any coordinator round-trip.
func (p *Clone) LogFile(relPath string) ([]CommitInfo, error) {
	iter, err := p.repo.Log(&gogit.LogOptions{FileName: &relPath})
	if err != nil {
		return nil, fmt.Errorf("opening log for %s: %w", relPath, err)
	}
	defer iter.Close()

	var out []CommitInfo
	err = iter.ForEach(func(c *object.Commit) error {
		out = append(out, CommitInfo{
			Hash:    c.Hash.String(),
			Message: c.Message,
			Author:  c.Author.Name,
			Time:    c.Author.When,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterating log: %w", err)
	}
	return out, nil
}

// SetRemote reconfigures the origin remote of the local clone to
// point at a new URL. Used when the coordinator's project record
// has a remote_url that differs from (or fills in) the local
// clone's origin — e.g., on first access to a project the client
// had cloned from somewhere else, or when the coordinator updates
// a project's remote_url post-hoc.
//
// Passing an empty string removes origin, turning the clone into
// a local-only project. The caller MUST hold the project lock.
func (p *Clone) SetRemote(url string) error {
	existing, err := p.repo.Remote("origin")
	if url == "" {
		if err != nil {
			return nil
		}
		if err := p.repo.DeleteRemote("origin"); err != nil {
			return fmt.Errorf("removing origin: %w", err)
		}
		p.remoteURL = ""
		return nil
	}
	if err == nil {
		if cfg := existing.Config(); cfg != nil && len(cfg.URLs) > 0 && cfg.URLs[0] == url {
			p.remoteURL = url
			return nil
		}
		if err := p.repo.DeleteRemote("origin"); err != nil {
			return fmt.Errorf("replacing origin: %w", err)
		}
	}
	if _, err := p.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{url},
	}); err != nil {
		return fmt.Errorf("creating remote: %w", err)
	}
	p.remoteURL = url
	return nil
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
func buildCommitMessage(taskID, username string, artifactPaths []string, modelName string, trailers EnjuTrailers) string {
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
	if rendered := RenderEnjuTrailers(trailers); rendered != "" {
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

// ArtifactPath returns the repo-relative path for a user-facing
// artifact. Artifacts live at their natural path in the repo root
// (no prefix), so `writes_artifacts: [figures/fig1.png]` writes
// directly to `figures/fig1.png`. Validation (no ../, no .git/,
// no enju/) is the caller's responsibility.
func ArtifactPath(userPath string) string {
	return userPath
}

// --- Named outputs with file specs ---
//
// Tasks may declare an `outputs:` schema in their YAML:
//
//   outputs:
//     gene_list:
//       description: "Top-scoring genes as CSV"
//       file: genes.csv
//       format: csv
//     pathways:
//       description: "Pathway graph as JSON"
//       file: pathways.json
//       format: json
//     summary:
//       description: "Human-readable summary"
//
// At submit time, each named output gets its own file in the task's
// result directory. Outputs with a `file:` spec use that exact
// filename; outputs without one fall back to `{name}.{format}`
// (default format `md`). The metadata.json for the submission gains
// an `output_files` index mapping output names to on-disk filenames
// so downstream template references like `{{task.gene_list}}` can
// find the right file via the index.
//
// Ported from the legacy coordinator-side writeMultiFileResult in
// internal/api/files.go during the iteration A orchestrator rewrite.

// NamedOutputSpec describes one named output's file layout. Mirrors
// enjuYaml.OutputSpec but lives here so clients don't need to
// import the YAML parser package.
type NamedOutputSpec struct {
	Description string
	File        string
	Format      string
}

// ParseNamedOutputSchema deserializes a task's outputs JSON (as
// stored in tasks.outputs by the coordinator) into a map of name
// to spec. Returns nil for empty or malformed input — callers
// should treat nil as "no schema, fall back to the single-file
// outputs path".
//
// The JSON shape the coordinator persists is:
//
//	{
//	  "gene_list": {"Description": "...", "File": "genes.csv", "Format": "csv"},
//	  "pathways":  {"Description": "...", "File": "pathways.json", "Format": "json"}
//	}
//
// Uppercase field names because Go's encoding/json defaults to the
// struct field name when there are no json tags (the YAML parser
// uses yaml tags but no json tags on OutputSpec).
func ParseNamedOutputSchema(schemaJSON string) map[string]NamedOutputSpec {
	if schemaJSON == "" {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(schemaJSON), &raw); err != nil {
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	schema := make(map[string]NamedOutputSpec, len(raw))
	for name, v := range raw {
		switch val := v.(type) {
		case string:
			// Short-form outputs (`name: "description"`) — no file
			// spec, no format. Render as name.md by default.
			schema[name] = NamedOutputSpec{Description: val}
		case map[string]interface{}:
			spec := NamedOutputSpec{}
			if d, ok := val["Description"].(string); ok {
				spec.Description = d
			}
			if f, ok := val["File"].(string); ok {
				spec.File = f
			}
			if fmt, ok := val["Format"].(string); ok {
				spec.Format = fmt
			}
			schema[name] = spec
		}
	}
	return schema
}

// BuildNamedOutputFiles constructs the FileWrite list for a named-
// outputs submission. Each output gets its own file under
// `resultDir/`, with the filename chosen from the schema's `file:`
// spec if present or built from `{name}.{format}` otherwise. Also
// returns the `output_files` index (output name → on-disk filename)
// that the caller should embed in metadata.json so downstream tasks
// can resolve `{{task.field_name}}` references via the index.
//
// Does NOT build metadata.json itself — the caller owns the full
// metadata map and should add the returned fileIndex to it under
// the `output_files` key before writing it.
//
// Ported from the legacy coordinator-side writeMultiFileResult.
func BuildNamedOutputFiles(resultDir string, schema map[string]NamedOutputSpec, values map[string]string) (files []FileWrite, fileIndex map[string]string) {
	fileIndex = make(map[string]string, len(values))
	for name, value := range values {
		spec, hasSpec := schema[name]
		var fileName string
		if hasSpec && spec.File != "" {
			fileName = spec.File
		} else {
			format := "md"
			if hasSpec && spec.Format != "" {
				format = spec.Format
			}
			fileName = name + "." + format
		}
		files = append(files, FileWrite{
			RepoRelPath: filepath.Join(resultDir, fileName),
			Content:     []byte(value),
		})
		fileIndex[name] = fileName
	}
	return files, fileIndex
}

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
