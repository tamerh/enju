package mcpgit

// Fetch-path scanner for Enju task-completion trailers.
//
// Lifecycle: after a fat-client fetches a branch from origin, it
// walks the new commits (cursor..tip) looking for Enju-Task-Complete
// trailers. Each match turns into a reconcile entry the client
// posts to the coordinator. The cursor advances to tip on success
// so the next scan is incremental.
//
// The three pieces here:
//
//  - FetchBranch: pure fetch (no worktree, no merge). Safe to run
//    frequently from any caller.
//  - ScanBranchSince: walk `cursor..tip` on `refs/remotes/origin/<branch>`
//    and return parsed trailer entries.
//  - Cursors: JSON-backed per-project state tracking the last-
//    scanned SHA per branch.
//
// Everything here stays client-side; the coordinator never fetches.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// cursorsFormatVersion tracks the on-disk schema for cursor files.
// Bumping this invalidates existing state; readers drop the file
// rather than interpret it under new rules.
const cursorsFormatVersion = 1

// cursorMutexes is the process-wide registry of per-(stateDir,
// projectID) mutexes serializing load-modify-save on cursor
// files. Submits, scanners, and any future trailer-writing
// surface all acquire the same mutex for the same project, so
// a concurrent load-modify-save from one path can't race-
// overwrite an advance from another. Keyed by stateDir so a
// test with its own temp state dir doesn't serialize against
// an unrelated in-process project.
//
// sync.Map allocates on miss but the registry is tiny (one
// entry per active project) and cheap to walk; mutex creation
// is a one-time cost per project.
var cursorMutexes sync.Map

// CursorMutexFor returns the process-wide mutex guarding
// cursor load-modify-save for the given (stateDir, projectID).
// Callers must Lock() before LoadCursors + Save and Unlock()
// after. First call per project creates the mutex; subsequent
// calls return the same pointer.
func CursorMutexFor(stateDir string, projectID int64) *sync.Mutex {
	key := fmt.Sprintf("%s|%d", stateDir, projectID)
	if existing, ok := cursorMutexes.Load(key); ok {
		return existing.(*sync.Mutex)
	}
	fresh := &sync.Mutex{}
	actual, _ := cursorMutexes.LoadOrStore(key, fresh)
	return actual.(*sync.Mutex)
}

// advanceCursorIfConfigured is the SubmitTaskResult / CommitFiles
// hook that marks a just-landed commit as "already processed" in
// the fat-client's scan cursor, so the next trailer scan doesn't
// replay it as a new event. Called INSIDE SubmitTaskResult so
// every caller — production MCP handlers, tests, future shell
// wrappers, anyone — benefits without having to remember a
// post-commit cursor update call.
//
// No-op when (projectID, stateDir) isn't configured: that's
// the "caller doesn't maintain a scanner cursor" case
// (coordinator-side code, store unit tests, the raw mcpgit
// helpers in tests). Runs under CursorMutexFor so a
// concurrent scanner save can't race-overwrite the advance.
// AdvanceScanCursor is the exported variant of
// advanceCursorIfConfigured. Same behavior (no-op on empty
// stateDir / zero projectID / empty branch-or-sha), suitable
// for batch submit's post-push cursor advance where the
// SubmitTaskResult path isn't used.
func AdvanceScanCursor(projectID int64, stateDir, branch, sha string) {
	advanceCursorIfConfigured(projectID, stateDir, branch, sha)
}

func advanceCursorIfConfigured(projectID int64, stateDir, branch, sha string) {
	if projectID == 0 || stateDir == "" || branch == "" || sha == "" {
		return
	}
	mu := CursorMutexFor(stateDir, projectID)
	mu.Lock()
	defer mu.Unlock()
	cursors, err := LoadCursors(stateDir, projectID)
	if err != nil {
		return
	}
	cursors.Set(branch, sha)
	_ = cursors.Save()
}

// CommitTrailer pairs a commit SHA with the parsed Enju trailers
// scanned out of its message. One entry per commit that carried
// an `Enju-Task-Complete` trailer; commits without it are skipped.
type CommitTrailer struct {
	CommitSHA string
	Trailers  EnjuTrailers
}

// FetchBranch updates `refs/remotes/origin/<branch>` from the
// remote without touching the worktree or local branch refs.
// Returns nil on success, including the case where the branch
// doesn't exist remotely (a brand-new branch yet to be pushed).
//
// Distinct from PullBranch: Pull fetches AND merges into the
// local branch, which can conflict. Fetch is a read-only
// database operation on git objects. The scanner uses Fetch so
// it never races with in-flight writes and never rewrites the
// worktree.
func (p *Project) FetchBranch(branch string) error {
	if p.remoteURL == "" {
		return nil // local-only, nothing to fetch
	}
	b := p.resolveBranch(branch)
	// ls-remote first so a non-existent remote branch is a
	// no-op rather than a fetch error. Matches the PullBranch
	// convention used elsewhere.
	remoteSHA, err := p.RemoteBranchHash(b)
	if err != nil {
		return err
	}
	if remoteSHA == "" {
		return nil
	}
	refName := plumbing.NewBranchReferenceName(b)
	remoteRefName := plumbing.NewRemoteReferenceName("origin", b)
	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s", refName, remoteRefName))
	err = p.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{refSpec},
		Auth:       sshAuthMethod(p.remoteURL),
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return friendlyGitError("fetch", p.remoteURL, err)
	}
	return nil
}

// ScanBranchSince walks commits on `refs/remotes/origin/<branch>`
// newer than `since` (exclusive) back to tip, extracts Enju-*
// trailers from each commit message, and returns the results
// along with the new tip SHA to persist as the next cursor.
//
// Semantics:
//
//   - `since == ""` is the FIRST scan of this branch. Returns
//     tip + empty results. Rationale: we don't want to re-process
//     every historical commit the first time we see the branch;
//     the client has presumably already reconciled anything that
//     happened before it started watching. Callers who want a
//     retroactive scan can pass the branch's root (e.g.
//     origin/main's first commit) explicitly.
//
//     Escape hatch: a caller that needs a ONE-TIME full walk
//     (e.g. a new collaborator joining a project mid-flight
//     who wants to pick up every past trailer commit, or a
//     future wrapper protocol bump that adds new trailer
//     semantics to old commits) can call `cursors.Set(branch,
//     "")` between loads to force the next scan to restart
//     from baseline. The cursor stays empty only until the
//     next successful scan advances it.
//
//   - `since` matches tip: nothing new. Returns tip + empty.
//
//   - `since` is an ancestor of tip: walks every commit in
//     `since..tip` (exclusive of `since`, inclusive of tip),
//     emits those with Enju-Task-Complete trailers.
//
//   - `since` is NOT an ancestor of tip (force-push, rebase):
//     walks the entire tip history. Re-sends already-reconciled
//     commits. Fine — the reconcile endpoint is idempotent.
//
// A commit without Enju-Task-Complete is silently skipped. An
// unreachable `since` hash returns an error rather than a bogus
// walk.
func (p *Project) ScanBranchSince(branch, since string) (newTip string, found []CommitTrailer, err error) {
	b := p.resolveBranch(branch)
	remoteRef := plumbing.NewRemoteReferenceName("origin", b)
	ref, err := p.repo.Reference(remoteRef, true)
	if err != nil {
		// Local-only project (no origin) or branch not yet
		// fetched. Not an error — scanner moves on.
		return since, nil, nil
	}
	tip := ref.Hash().String()
	if since == "" {
		// First-time baseline: record tip, no scan.
		return tip, nil, nil
	}
	if since == tip {
		return tip, nil, nil
	}

	// If `since` doesn't resolve to a commit we have, fall back
	// to walking from tip without a stop condition. Scanner
	// will re-emit everything; the coordinator no-ops duplicates.
	_, sinceErr := p.repo.CommitObject(plumbing.NewHash(since))
	stopOnSince := sinceErr == nil

	iter, err := p.repo.Log(&gogit.LogOptions{From: ref.Hash()})
	if err != nil {
		return since, nil, fmt.Errorf("opening log for %s: %w", b, err)
	}
	defer iter.Close()

	// Walk in reverse chronological order (newest first). We
	// reverse at the end so callers see commits in
	// ancestor→tip order — matches what a human reader
	// following `git log --reverse` would see, and lets the
	// coordinator process upstream completions before
	// downstream ones when both land in one scan window.
	err = iter.ForEach(func(c *object.Commit) error {
		if stopOnSince && c.Hash.String() == since {
			return storer.ErrStop
		}
		t := ParseEnjuTrailers(c.Message)
		if t.TaskID != "" {
			found = append(found, CommitTrailer{
				CommitSHA: c.Hash.String(),
				Trailers:  t,
			})
		}
		return nil
	})
	if err != nil && err != storer.ErrStop {
		return since, nil, fmt.Errorf("walking log: %w", err)
	}
	// Reverse to chronological order.
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return tip, found, nil
}

// RescanSentinelSHA is the cursor value that forces
// ScanBranchSince to walk a branch's full history from tip on
// the next scan. The 40-zero string is non-empty (so it
// doesn't trigger the first-scan baseline early return) and
// guaranteed not to resolve to any real commit (so
// stopOnSince=false in the walk path, the iter goes back to
// the root). The coordinator's reconcile endpoint is
// idempotent, so re-emitting already-seen trailers is safe —
// the cursor advances to the actual tip after the first full
// walk and subsequent scans return to incremental behavior.
//
// Used by enju_set_project_remote to recover from the
// late-remote-add case: a project that ran async compute with
// no origin configured has commits stranded on local
// refs/heads/* that the scanner never saw. Setting a remote
// + pushing makes refs/remotes/origin/* exist for the first
// time, but cursor entries (if any) baselined empty. Setting
// each branch's cursor to this sentinel forces a one-shot
// retroactive scan that picks up the historical trailers.
const RescanSentinelSHA = "0000000000000000000000000000000000000000"

// Cursors tracks per-branch scan position for one project. On-
// disk form: `~/.enju/state/project-<id>-cursors.json`. Readers
// load, scan, and atomically save via Cursors.Save().
type Cursors struct {
	// Version pins the on-disk schema. Mismatch ⇒ treat as
	// empty state and start fresh; writing will bump it.
	Version int `json:"version"`

	// Branches maps branch name → last successfully-scanned
	// commit SHA. Empty map for a fresh project.
	Branches map[string]string `json:"branches"`

	// path is the absolute file this Cursors was loaded from /
	// will save back to. Not serialized.
	path string `json:"-"`

	mu sync.Mutex `json:"-"`
}

// NewCursors returns an empty Cursors bound to the on-disk file
// for the given project. Does NOT load — call LoadCursors if
// you want to read existing state. Useful when you're about to
// overwrite whatever's there (tests, reset).
func NewCursors(stateDir string, projectID int64) *Cursors {
	return &Cursors{
		Version:  cursorsFormatVersion,
		Branches: map[string]string{},
		path:     cursorsPath(stateDir, projectID),
	}
}

// LoadCursors reads the cursors file for the given project from
// `stateDir`. Returns an empty-but-valid Cursors when the file
// doesn't exist yet (first-run case). Corrupted files are
// treated as empty with a best-effort log — never an error that
// would wedge the scanner.
func LoadCursors(stateDir string, projectID int64) (*Cursors, error) {
	c := NewCursors(stateDir, projectID)
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("reading cursors %q: %w", c.path, err)
	}
	var raw Cursors
	if err := json.Unmarshal(data, &raw); err != nil {
		// Malformed — fall back to empty. Logging is the
		// caller's job (we don't have a logger here).
		return c, nil
	}
	if raw.Version != cursorsFormatVersion {
		// Schema bump: throw away.
		return c, nil
	}
	if raw.Branches != nil {
		c.Branches = raw.Branches
	}
	return c, nil
}

// Get returns the last-scanned SHA for the given branch, or
// empty string if this branch has no cursor yet.
func (c *Cursors) Get(branch string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Branches[branch]
}

// Set updates the in-memory cursor for a branch. Call Save to
// persist. The Set/Save split lets callers batch updates from
// a multi-branch scan and commit them all atomically.
func (c *Cursors) Set(branch, sha string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Branches == nil {
		c.Branches = map[string]string{}
	}
	c.Branches[branch] = sha
}

// Save writes the cursors atomically: temp file + rename. A
// crash mid-scan leaves either the previous state (rename didn't
// happen) or the new state (rename succeeded) — never a partial
// write. The parent directory is created on demand.
func (c *Cursors) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		return fmt.Errorf("cursors path not set")
	}
	if c.Version == 0 {
		c.Version = cursorsFormatVersion
	}
	// Copy into a serialization struct to avoid exporting the
	// mutex / path fields via json's zero-tag fallthrough on
	// some marshaler versions.
	serial := struct {
		Version  int               `json:"version"`
		Branches map[string]string `json:"branches"`
	}{
		Version:  c.Version,
		Branches: sortedCopy(c.Branches),
	}
	data, err := json.MarshalIndent(serial, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding cursors: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".cursors-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := io.WriteString(tmp, string(data)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp cursors: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming cursors: %w", err)
	}
	return nil
}

// cursorsPath returns the canonical state-file path for a
// project. Kept separate so callers that just need the path
// (tests, migration helpers) don't have to construct a Cursors.
func cursorsPath(stateDir string, projectID int64) string {
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".enju", "state")
	}
	return filepath.Join(stateDir, fmt.Sprintf("project-%d-cursors.json", projectID))
}

func sortedCopy(m map[string]string) map[string]string {
	// json.Marshal of a map has non-deterministic key order
	// pre-1.12 and deterministic since — but we pass through a
	// sorted copy anyway so `diff` between cursor-file versions
	// stays sane across Go versions and platforms.
	if len(m) == 0 {
		return map[string]string{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}
