// Package mcpgit provides the client-side git operations used by the
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
//         Task {taskID} by @{username}: result
//         Task {taskID} by @{username}: result + N artifact(s)
//
//         Artifacts: path1, path2, ...
//
//     (Subject line matches `commitTaskSubjectRe` in the coordinator's
//     legacy rollback code; future iterations will replace the walker
//     with DB-only invalidation and drop this constraint, but for
//     the coexistence period the format stays stable.)
package mcpgit

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// Workspace manages a directory that holds one local clone per
// project. Callers create a Workspace once at MCP client startup and
// re-use it for the lifetime of the process.
type Workspace struct {
	rootDir string
	logger  *slog.Logger

	mu      sync.Mutex
	clients map[int64]*Project // projectID → open project clone
}

// NewWorkspace creates (or reuses) a workspace rooted at the given
// directory. The directory is created with 0755 perms if missing.
// Pass an empty string to default to `$HOME/.enju/workspaces`.
func NewWorkspace(rootDir string, logger *slog.Logger) (*Workspace, error) {
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
	return &Workspace{
		rootDir: rootDir,
		logger:  logger,
		clients: make(map[int64]*Project),
	}, nil
}

// RootDir returns the directory that holds per-project clones.
func (ws *Workspace) RootDir() string { return ws.rootDir }

// projectDir returns the on-disk path for one project's local clone.
func (ws *Workspace) projectDir(projectID int64) string {
	return filepath.Join(ws.rootDir, fmt.Sprintf("%d", projectID))
}

// HasLocalClone returns true if a clone for the given project
// already exists on disk in this workspace. Used by list-style
// callers (enju_list_projects) that want to decorate their output
// with local state WITHOUT triggering a fresh clone as a side
// effect of the listing call.
func (ws *Workspace) HasLocalClone(projectID int64) bool {
	stat, err := os.Stat(ws.projectDir(projectID))
	if err != nil || !stat.IsDir() {
		return false
	}
	// A valid clone has a .git subdirectory.
	gitStat, err := os.Stat(filepath.Join(ws.projectDir(projectID), ".git"))
	return err == nil && gitStat.IsDir()
}

// ForProject returns a handle to the local clone of the given project,
// cloning it from remoteURL on first access. Subsequent calls for the
// same projectID return the cached handle. If remoteURL is empty, the
// project is treated as a local-only clone (no origin, no push) and
// must already exist on disk — this supports a future degenerate mode
// where a self-hosted user keeps everything in a local working tree
// without a remote at all.
func (ws *Workspace) ForProject(projectID int64, remoteURL string) (*Project, error) {
	if projectID == 0 {
		return nil, fmt.Errorf("projectID is required")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	if p, ok := ws.clients[projectID]; ok {
		return p, nil
	}

	workDir := ws.projectDir(projectID)
	p, err := openOrClone(workDir, remoteURL, ws.logger)
	if err != nil {
		return nil, fmt.Errorf("opening project %d: %w", projectID, err)
	}
	// openOrClone is project-agnostic; stamp the projectID in so
	// path helpers (ArtifactPath, etc.) can namespace by project.
	p.projectID = projectID
	ws.clients[projectID] = p
	return p, nil
}

// Project is a handle to one project's local clone. All writes for
// this project are serialized through the per-project mutex.
type Project struct {
	projectID int64
	workDir   string
	remoteURL string
	repo      *gogit.Repository
	logger    *slog.Logger

	mu sync.Mutex // serializes writes within this project

	// Push status bookkeeping — in-memory only, per process
	// lifetime. Updated by pushInternal on both success and
	// failure so the project_remote_status tool can report
	// "last push at / last push error". Resets on MCP client
	// restart, which is fine because the information is purely
	// diagnostic.
	lastPushAt    time.Time
	lastPushError string
}

// ProjectID returns the coordinator-assigned project ID this clone
// belongs to.
func (p *Project) ProjectID() int64 { return p.projectID }

// WorkDir returns the local working-tree path.
func (p *Project) WorkDir() string { return p.workDir }

// RemoteURL returns the configured origin URL, or an empty string
// for local-only clones.
func (p *Project) RemoteURL() string { return p.remoteURL }

// Lock acquires the per-project write mutex. Callers performing a
// sequence of WriteFile + Commit + Push operations should hold this
// across the whole sequence.
func (p *Project) Lock()   { p.mu.Lock() }
func (p *Project) Unlock() { p.mu.Unlock() }

// openOrClone opens an existing project clone or creates a fresh one
// by cloning from remoteURL. If workDir exists and is a valid git
// repo, it's opened in place. If it doesn't exist, it's cloned from
// remoteURL. If workDir exists but isn't a repo (or the clone is
// corrupted), it's removed and re-cloned.
func openOrClone(workDir, remoteURL string, logger *slog.Logger) (*Project, error) {
	if repo, err := gogit.PlainOpen(workDir); err == nil {
		// Clone exists and is valid.
		p := &Project{
			workDir: workDir,
			repo:    repo,
			logger:  logger,
		}
		// Pick up the configured remote URL if the caller didn't
		// specify one (e.g. when reopening an existing clone after
		// a restart).
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

	// Clone doesn't exist — either clone from remote or init empty.
	if remoteURL == "" {
		// Degenerate local-only mode: init an empty repo with one
		// commit so future writes have a base.
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
		logger.Info("initialized local-only repo", "path", workDir)
		return &Project{
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
			return &Project{
				workDir:   workDir,
				remoteURL: remoteURL,
				repo:      repo2,
				logger:    logger,
			}, nil
		}
		return nil, fmt.Errorf("cloning %s: %w", remoteURL, err)
	}
	logger.Info("cloned project", "url", remoteURL, "path", workDir)
	return &Project{
		workDir:   workDir,
		remoteURL: remoteURL,
		repo:      repo,
		logger:    logger,
	}, nil
}

// Pull fetches the latest state of origin/main and fast-forwards the
// local branch to match. If the working tree has uncommitted local
// changes (shouldn't happen in normal flow — clients should always
// commit what they wrote before yielding), Pull returns an error.
// The caller MUST hold the project lock.
func (p *Project) Pull() error {
	if p.remoteURL == "" {
		return nil // local-only, nothing to pull
	}
	wt, err := p.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}
	err = wt.Pull(&gogit.PullOptions{
		RemoteName:    "origin",
		ReferenceName: plumbing.ReferenceName("refs/heads/main"),
		SingleBranch:  true,
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return fmt.Errorf("pulling: %w", err)
	}
	return nil
}

// HeadHash returns the SHA of the current local HEAD.
func (p *Project) HeadHash() (string, error) {
	ref, err := p.repo.Head()
	if err != nil {
		return "", fmt.Errorf("reading HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

// RemoteHeadHash contacts the remote via ls-remote and returns the
// SHA of refs/heads/main there, or empty string if the remote has
// no such ref. Used by CompareToRemote to compare local HEAD against
// the authoritative remote state without a full fetch.
func (p *Project) RemoteHeadHash() (string, error) {
	if p.remoteURL == "" {
		return "", fmt.Errorf("no remote configured")
	}
	rem, err := p.repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("no origin remote: %w", err)
	}
	refs, err := rem.List(&gogit.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("ls-remote: %w", err)
	}
	for _, r := range refs {
		if r.Name() == plumbing.ReferenceName("refs/heads/main") {
			return r.Hash().String(), nil
		}
	}
	return "", nil
}

// ReadFile reads a file from the working tree at a repo-relative path.
// Used by the client-side template resolver to read upstream task
// results and artifact contents.
func (p *Project) ReadFile(repoRelPath string) ([]byte, error) {
	full := filepath.Join(p.workDir, repoRelPath)
	return os.ReadFile(full)
}

// ReadFileAtCommit reads a file's contents as of a specific commit.
// Used by the template resolver when the caller wants the exact
// version associated with an upstream task's submitted commit SHA,
// rather than whatever happens to be in the working tree today.
func (p *Project) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
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
// the full fetch → reset → overlay → commit → push cycle with retries
// on non-fast-forward rejections. Returns the commit SHA that
// actually landed so the caller can report it to the coordinator.
//
// The caller MUST hold the project lock for the duration of the call.
//
// Behavior details:
//
//   - Before writing any files, the function fetches origin and
//     fast-forwards the local branch to origin/main. This ensures
//     the commit is built on top of the latest remote state, not a
//     stale local state.
//
//   - Files are written into the working tree at their declared
//     paths (creating parent directories as needed). Any existing
//     files at those paths are overwritten.
//
//   - A single commit is created with the standard enju subject
//     format. All files in the request land in that one commit.
//
//   - The commit is pushed. If the remote rejected the push as
//     non-fast-forward (another client committed in the meantime),
//     the cycle repeats: reset local state to the new origin/main,
//     re-apply the files, commit, push. This is safer than
//     rebasing because we can't know what the remote added.
//
//   - If we hit MaxRetries without succeeding, the most recent
//     push error is returned. The caller should surface this so
//     the citizen knows their submit didn't land.
func (p *Project) SubmitTaskResult(req SubmitRequest) (*SubmitResult, error) {
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("no files to write")
	}
	if req.TaskID == "" || req.Username == "" {
		return nil, fmt.Errorf("TaskID and Username are required")
	}
	maxRetries := req.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	commitMsg := buildCommitMessage(req.TaskID, req.Username, req.ArtifactPaths)

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Sync to latest remote state before applying our changes.
		if err := p.resetToRemote(); err != nil {
			lastErr = fmt.Errorf("sync before submit: %w", err)
			continue
		}

		// Overlay the new files onto the working tree.
		for _, f := range req.Files {
			full := filepath.Join(p.workDir, f.RepoRelPath)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				return nil, fmt.Errorf("creating dir for %s: %w", f.RepoRelPath, err)
			}
			if err := os.WriteFile(full, f.Content, 0644); err != nil {
				return nil, fmt.Errorf("writing %s: %w", f.RepoRelPath, err)
			}
		}

		// Stage, commit, push.
		sha, err := p.commit(commitMsg, req.AuthorName, req.AuthorEmail)
		if err != nil {
			lastErr = err
			continue
		}
		if err := p.push(); err != nil {
			lastErr = err
			// Push failed — could be non-ff. Loop and try again.
			continue
		}
		return &SubmitResult{CommitSHA: sha, Attempts: attempt}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("push failed after %d attempts (no error captured)", maxRetries)
	}
	return nil, fmt.Errorf("submit failed after %d attempts: %w", maxRetries, lastErr)
}

// resetToRemote fetches origin and hard-resets the local branch to
// origin/main so we have a clean base for new writes. Used before
// each retry of SubmitTaskResult. For local-only projects (no
// remote), this is a no-op. For bootstrapping empty remotes
// (iteration A.5 fix), the fetch will return
// transport.ErrEmptyRemoteRepository — treat that as "nothing to
// catch up to" and proceed with the write.
func (p *Project) resetToRemote() error {
	if p.remoteURL == "" {
		return nil
	}
	err := p.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
	})
	if err != nil {
		switch {
		case errors.Is(err, gogit.NoErrAlreadyUpToDate):
			// Up to date, carry on.
		case errors.Is(err, transport.ErrEmptyRemoteRepository):
			// Empty remote — we're bootstrapping it. Nothing
			// to fetch, nothing to reset to. Proceed with
			// the write; the first push will populate the
			// remote's main branch.
			return nil
		default:
			return fmt.Errorf("fetching: %w", err)
		}
	}
	// Look up origin/main and reset HEAD to it.
	ref, err := p.repo.Reference(plumbing.ReferenceName("refs/remotes/origin/main"), true)
	if err != nil {
		// No remote main yet (fresh repo or bootstrapped empty
		// remote) — skip reset.
		return nil
	}
	wt, err := p.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}
	if err := wt.Reset(&gogit.ResetOptions{
		Mode:   gogit.HardReset,
		Commit: ref.Hash(),
	}); err != nil {
		return fmt.Errorf("resetting to origin/main: %w", err)
	}
	return nil
}

// commit stages everything in the working tree and creates a
// commit with the given message and author identity. If authorName
// or authorEmail is empty, a generic `Enju Client` placeholder is
// used — this is only the case for unit tests that don't care
// about author attribution. Production callers (the MCP server's
// submit handler) always pass the citizen's real name and email so
// pushes to the user's own git host attribute correctly on
// contributor graphs, blame, and CODEOWNERS.
func (p *Project) commit(message, authorName, authorEmail string) (string, error) {
	wt, err := p.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("getting worktree: %w", err)
	}
	if err := wt.AddGlob("."); err != nil {
		return "", fmt.Errorf("staging: %w", err)
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
func (p *Project) push() error {
	return p.pushInternal(false)
}

// Push is the public safe-push entry point for the MCP tool handler
// that performs on-demand sync (enju_project_sync). It pushes the
// local HEAD to origin with go-git's default (non-force) semantics,
// so a non-fast-forward state is rejected by the remote. The caller
// MUST hold the project lock.
func (p *Project) Push() error { return p.pushInternal(false) }

// PushForce is the destructive counterpart to Push: overwrites the
// remote branch even when histories have diverged. Only called from
// the explicit force-sync path, never from the normal submit flow.
// The caller MUST hold the project lock.
func (p *Project) PushForce() error { return p.pushInternal(true) }

// pushInternal is the shared implementation behind Push / PushForce /
// the private submit-time push. Returns nil for local-only projects
// so callers can uniformly call it regardless of whether a remote is
// configured.
func (p *Project) pushInternal(force bool) error {
	if p.remoteURL == "" {
		return nil
	}
	err := p.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		Force:      force,
	})
	// Record the outcome for the project_remote_status diagnostic,
	// regardless of whether this was a success or a failure.
	p.lastPushAt = time.Now()
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		p.lastPushError = err.Error()
		return fmt.Errorf("pushing: %w", err)
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
func (p *Project) LastPushAt() time.Time {
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
func (p *Project) LastPushError() string { return p.lastPushError }

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
func (p *Project) CompareToRemote() (*RemoteComparison, error) {
	r := &RemoteComparison{}

	if p.remoteURL == "" {
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
func (p *Project) LogFile(relPath string) ([]CommitInfo, error) {
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
func (p *Project) SetRemote(url string) error {
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
func buildCommitMessage(taskID, username string, artifactPaths []string) string {
	if len(artifactPaths) == 0 {
		return fmt.Sprintf("Task %s by @%s: result", taskID, username)
	}
	return fmt.Sprintf("Task %s by @%s: result + %d artifact(s)\n\nArtifacts: %s",
		taskID, username, len(artifactPaths), strings.Join(artifactPaths, ", "))
}

// --- standard path helpers ---
//
// A.5 note: all project-scoped content lives under the per-project
// prefix `projects/{project_id}/...` so two projects sharing the
// same git remote can coexist without stepping on each other's
// result/artifact paths. Before A.5, runs were namespaced only by
// run sequence, which collided whenever two projects pointed at
// the same remote. The prefix is the project's coordinator-assigned
// integer id — unique per coordinator instance, stable across the
// lifetime of the project.

// ProjectDir returns the repo-relative root directory for all of a
// project's content. Used by callers that want to know where a
// project's data lives in the shared remote.
func ProjectDir(projectID int64) string {
	return filepath.Join("projects", fmt.Sprintf("%d", projectID))
}

// ResultDir returns the repo-relative directory for a task's result
// files. Layout:
//
//	projects/{projectID}/runs/{runSeq}/{taskDefID}/                 (no for_each)
//	projects/{projectID}/runs/{runSeq}/{instanceKey}/{taskDefID}/   (with for_each)
//
// Kept in this package so the client, coordinator, and test helpers
// share one source of truth for layout.
func ResultDir(projectID int64, runSeq int, instanceKey, taskDefID string) string {
	base := filepath.Join(ProjectDir(projectID), "runs", fmt.Sprintf("%d", runSeq))
	if instanceKey != "" {
		return filepath.Join(base, instanceKey, taskDefID)
	}
	return filepath.Join(base, taskDefID)
}

// ArtifactPath returns the repo-relative path for a user-facing
// artifact path under a given project. Validation (no ../, no
// .git, etc.) is the caller's responsibility; this is just path
// concatenation with the standard `projects/{id}/artifacts/`
// prefix.
func ArtifactPath(projectID int64, userPath string) string {
	return filepath.Join(ProjectDir(projectID), "artifacts", userPath)
}

// LegacyArtifactPath returns the pre-iteration-A.5 artifact path
// (without the per-project namespace — just `artifacts/...`).
// Callers use this as a fallback when the primary ArtifactPath
// lookup fails, so projects created under the old layout can
// still be read. The fallback is read-only; new writes always
// use the namespaced path.
func LegacyArtifactPath(userPath string) string {
	return filepath.Join("artifacts", userPath)
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
