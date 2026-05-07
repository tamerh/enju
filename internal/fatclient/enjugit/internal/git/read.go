package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ReadFileAtCommit reads a file's content at a specific commit.
//
// Lazy fetch: if the commit isn't in the local object DB, this
// method does ONE fetch from origin (with the full-branches
// refspec) and retries. Without that retry, cross-citizen reads
// fail when one bot pushes a topic and another bot tries to read
// it before its own clone has fetched. The retry self-heals.
//
// Returns:
//
//   - (content, true, nil) on hit.
//   - ("", false, nil) when the file isn't in the commit's tree.
//   - ("", false, ErrCommitNotFound) when the SHA can't be
//     resolved even after lazy fetch.
//   - ("", false, err) on any other I/O error.
//
// Read-only: does not acquire the project lock.
func (c *Clone) ReadFileAtCommit(sha, path string) ([]byte, bool, error) {
	hash := plumbing.NewHash(sha)
	commit, err := c.repo.CommitObject(hash)
	if err != nil {
		// Try lazy fetch and retry.
		if c.remoteURL != "" {
			if fetchErr := c.repo.Fetch(&gogit.FetchOptions{
				RemoteName: "origin",
				RefSpecs: []config.RefSpec{
					config.RefSpec("+refs/heads/*:refs/remotes/origin/*"),
				},
				Auth: sshAuthMethod(c.remoteURL),
			}); fetchErr != nil && !errors.Is(fetchErr, gogit.NoErrAlreadyUpToDate) {
				return nil, false, fmt.Errorf("%w (fetch failed: %v)", ErrCommitNotFound, fetchErr)
			}
			commit, err = c.repo.CommitObject(hash)
		}
		if err != nil {
			return nil, false, fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
		}
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, false, fmt.Errorf("git: load tree at %s: %w", sha, err)
	}
	file, err := tree.File(path)
	if errors.Is(err, object.ErrFileNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("git: lookup %s in tree: %w", path, err)
	}
	content, err := file.Contents()
	if err != nil {
		return nil, false, fmt.Errorf("git: read %s contents: %w", path, err)
	}
	return []byte(content), true, nil
}

// ResolveRef resolves a ref name to a commit SHA. Accepts:
//
//   - Branch names (e.g. "main") — resolved via refs/heads then
//     refs/remotes/origin.
//   - Full ref paths (e.g. "refs/heads/main").
//   - 40-hex SHAs (returned as-is after existence check).
//
// Returns ErrRefNotFound when the name doesn't resolve.
//
// Read-only: does not acquire the project lock.
func (c *Clone) ResolveRef(name string) (string, error) {
	// 40-hex SHA: return as-is if the commit exists.
	if isHexSHA(name) {
		if _, err := c.repo.CommitObject(plumbing.NewHash(name)); err == nil {
			return name, nil
		}
		return "", fmt.Errorf("%w: %s", ErrRefNotFound, name)
	}
	// Try as a full ref name first.
	if strings.HasPrefix(name, "refs/") {
		if ref, err := c.repo.Reference(plumbing.ReferenceName(name), true); err == nil {
			return ref.Hash().String(), nil
		}
	}
	// Try as a local branch name.
	if ref, err := c.repo.Reference(plumbing.NewBranchReferenceName(name), true); err == nil {
		return ref.Hash().String(), nil
	}
	// Try as a remote-tracking branch.
	if ref, err := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", name), true); err == nil {
		return ref.Hash().String(), nil
	}
	return "", fmt.Errorf("%w: %s", ErrRefNotFound, name)
}

// Head returns HEAD's commit SHA and the current branch name.
//
// When HEAD is detached, branch is "" and sha is the commit hash
// HEAD points at directly.
//
// Returns ErrRefNotFound when HEAD itself can't be resolved
// (empty repo, corrupt state).
//
// Read-only: does not acquire the project lock.
func (c *Clone) Head() (sha, branch string, err error) {
	head, err := c.repo.Head()
	if err != nil {
		return "", "", fmt.Errorf("%w: HEAD: %v", ErrRefNotFound, err)
	}
	branch = ""
	if head.Name().IsBranch() {
		branch = head.Name().Short()
	}
	return head.Hash().String(), branch, nil
}

// HeadCommitTime returns the author timestamp of HEAD's commit,
// or the zero Time when HEAD or the commit object can't be read
// (empty repo, detached state pointing at no object). Used by
// callers that need a "last activity" proxy when no explicit
// timestamp is recorded — e.g. surfacing LastPushAt with a
// HEAD-time fallback for projects that haven't pushed in this
// process.
//
// Read-only: does not acquire the project lock.
func (c *Clone) HeadCommitTime() time.Time {
	ref, err := c.repo.Head()
	if err != nil {
		return time.Time{}
	}
	commit, err := c.repo.CommitObject(ref.Hash())
	if err != nil {
		return time.Time{}
	}
	return commit.Author.When
}

// LocalBranches returns the names (short form) of every local
// branch ref. Order is iteration-order from the ref store; not
// stable across calls.
//
// Read-only: does not acquire the project lock.
func (c *Clone) LocalBranches() ([]string, error) {
	refs, err := c.repo.References()
	if err != nil {
		return nil, fmt.Errorf("git: list refs: %w", err)
	}
	defer refs.Close()
	var out []string
	for {
		ref, err := refs.Next()
		if err != nil {
			break
		}
		if ref.Name().IsBranch() {
			out = append(out, ref.Name().Short())
		}
	}
	return out, nil
}

// State inspects the worktree and returns its current state. Used
// by verbs to validate pre-state and by callers to decide whether
// recovery is needed.
//
// Order of checks:
//   1. Preserve dir present → StateMidCheckout.
//   2. HEAD detached → StateDetached.
//   3. Tracked file modifications → StateDirtyTracked.
//   4. Untracked files in tree → StateDirtyUntracked.
//   5. Otherwise → StateClean.
//
// Read-only: does not acquire the project lock.
func (c *Clone) State() WorktreeState {
	if _, err := os.Stat(c.workDir + preserveDirSuffix); err == nil {
		return StateMidCheckout
	}
	if head, err := c.repo.Head(); err == nil && !head.Name().IsBranch() {
		return StateDetached
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		// Best-effort: return clean and let the verb's other
		// checks fail loudly.
		return StateClean
	}
	status, err := wt.Status()
	if err != nil {
		return StateClean
	}
	hasTracked := false
	hasUntracked := false
	for path, fs := range status {
		// Skip our own infrastructure.
		base := filepath.Base(path)
		if base == ".bare.git" || base == ".clone" {
			continue
		}
		if fs.Worktree == gogit.Untracked {
			hasUntracked = true
		} else if fs.Worktree != gogit.Unmodified || fs.Staging != gogit.Unmodified {
			hasTracked = true
		}
	}
	if hasTracked {
		return StateDirtyTracked
	}
	if hasUntracked {
		return StateDirtyUntracked
	}
	return StateClean
}

// ReadTreeEntriesAtCommit returns the direct entries of a
// directory at a specific commit's tree. ok=false when dirPath
// doesn't resolve to a subtree (missing dir, or path resolves
// to a blob); ok=true with empty entries means an empty dir.
//
// Lazy fetch on missing commit, same as ReadFileAtCommit — if
// sha can't be resolved locally, one fetch from origin is
// attempted before giving up with ErrCommitNotFound.
//
// Read-only: does not acquire the project lock.
func (c *Clone) ReadTreeEntriesAtCommit(sha, dirPath string) ([]TreeEntry, bool, error) {
	tree, err := c.treeAtCommit(sha)
	if err != nil {
		return nil, false, err
	}
	sub, ok, err := treeSubTree(tree, dirPath)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	out := make([]TreeEntry, 0, len(sub.Entries))
	for _, e := range sub.Entries {
		entry := TreeEntry{
			Name:  e.Name,
			IsDir: !e.Mode.IsFile() && e.Mode != 0,
			Mode:  modeFromTreeEntry(e.Mode),
		}
		out = append(out, entry)
	}
	return out, true, nil
}

// WalkSubtreeBlobsAtCommit recursively walks every blob under
// dirPath and invokes visit(relPath, mode, content) for each.
// relPath is forward-slash separated and rooted at dirPath
// (i.e. doesn't include dirPath itself). Hidden entries (any
// path component starting with ".") are skipped — callers
// don't want .git, .DS_Store, etc. inside a snapshot.
//
// dirPath missing or resolving to a blob is a no-op (visitor
// not called); returns nil. Lazy fetch on missing commit,
// same as ReadFileAtCommit.
//
// Read-only: does not acquire the project lock.
func (c *Clone) WalkSubtreeBlobsAtCommit(sha, dirPath string, visit BlobVisitor) error {
	tree, err := c.treeAtCommit(sha)
	if err != nil {
		return err
	}
	sub, ok, err := treeSubTree(tree, dirPath)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	walker := object.NewTreeWalker(sub, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if err != nil {
			break
		}
		if !entry.Mode.IsFile() {
			continue
		}
		// Skip hidden segments anywhere along the path.
		hidden := false
		for _, seg := range strings.Split(name, "/") {
			if strings.HasPrefix(seg, ".") {
				hidden = true
				break
			}
		}
		if hidden {
			continue
		}
		blob, err := c.repo.BlobObject(entry.Hash)
		if err != nil {
			return fmt.Errorf("git: load blob for %s: %w", name, err)
		}
		reader, err := blob.Reader()
		if err != nil {
			return fmt.Errorf("git: open blob for %s: %w", name, err)
		}
		body, rerr := readAllFromBlob(reader, blob.Size)
		reader.Close()
		if rerr != nil {
			return fmt.Errorf("git: read blob for %s: %w", name, rerr)
		}
		mode := modeFromTreeEntry(entry.Mode)
		if err := visit(name, mode, body); err != nil {
			return err
		}
	}
	return nil
}

// CommitInfo describes a single commit for history-walking
// purposes. Returned by LogFile in reverse-chronological order
// (newest first). Includes author Name + Time so the MCP tool
// can render `who, when` provenance without a second lookup.
type CommitInfo struct {
	Hash    string
	Message string
	Author  string
	Time    time.Time
}

// LogFile returns commits that touched a specific file in the
// local clone, newest-first. Used by enju_get_artifact_history
// to render per-file provenance without a coordinator round-trip.
//
// Read-only: does not acquire the project lock.
func (c *Clone) LogFile(relPath string) ([]CommitInfo, error) {
	iter, err := c.repo.Log(&gogit.LogOptions{FileName: &relPath})
	if err != nil {
		return nil, fmt.Errorf("git: log %s: %w", relPath, err)
	}
	defer iter.Close()
	var out []CommitInfo
	err = iter.ForEach(func(commit *object.Commit) error {
		out = append(out, CommitInfo{
			Hash:    commit.Hash.String(),
			Message: commit.Message,
			Author:  commit.Author.Name,
			Time:    commit.Author.When,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("git: log %s: iterate: %w", relPath, err)
	}
	return out, nil
}

// WalkRecentCommits walks HEAD backward (newest-first) for up to
// maxWalk commits, calling visit(sha, message) on each. visit
// returns false to stop early (e.g. once all wanted task ids
// have been resolved). maxWalk <= 0 walks the whole history.
//
// Used by enjugit's batch submit to map task_id → SHA via the
// Enju-Task-Complete trailer. The trailer is preserved across a
// rebase, so this is the stable post-push key.
//
// Read-only: does not acquire the project lock.
func (c *Clone) WalkRecentCommits(maxWalk int, visit func(sha, message string) bool) error {
	iter, err := c.repo.Log(&gogit.LogOptions{})
	if err != nil {
		return fmt.Errorf("git: log walk: %w", err)
	}
	defer iter.Close()
	for i := 0; maxWalk <= 0 || i < maxWalk; i++ {
		commit, err := iter.Next()
		if err != nil {
			break
		}
		if !visit(commit.Hash.String(), commit.Message) {
			return nil
		}
	}
	return nil
}

// treeAtCommit loads the root tree for a commit, with the same
// lazy-fetch retry as ReadFileAtCommit.
func (c *Clone) treeAtCommit(sha string) (*object.Tree, error) {
	hash := plumbing.NewHash(sha)
	commit, err := c.repo.CommitObject(hash)
	if err != nil {
		if c.remoteURL != "" {
			if fetchErr := c.repo.Fetch(&gogit.FetchOptions{
				RemoteName: "origin",
				RefSpecs: []config.RefSpec{
					config.RefSpec("+refs/heads/*:refs/remotes/origin/*"),
				},
				Auth: sshAuthMethod(c.remoteURL),
			}); fetchErr != nil && !errors.Is(fetchErr, gogit.NoErrAlreadyUpToDate) {
				return nil, fmt.Errorf("%w (fetch failed: %v)", ErrCommitNotFound, fetchErr)
			}
			commit, err = c.repo.CommitObject(hash)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
		}
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("git: load tree at %s: %w", sha, err)
	}
	return tree, nil
}

// treeSubTree descends `tree` along the slash-separated `path`
// and returns the subtree. Returns (nil, false, nil) when any
// segment is missing or resolves to a blob — callers use the
// bool to distinguish "directory absent" from real errors.
func treeSubTree(tree *object.Tree, path string) (*object.Tree, bool, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return tree, true, nil
	}
	sub, err := tree.Tree(path)
	if errors.Is(err, object.ErrDirectoryNotFound) {
		return nil, false, nil
	}
	if err != nil {
		// ErrEntryNotFound (entry exists but isn't a subtree):
		// treat as absent so callers don't have to special-case it.
		return nil, false, nil
	}
	return sub, true, nil
}

// modeFromTreeEntry normalizes a git tree-entry filemode into
// an os.FileMode. Tree modes are octal: 0o100644 (regular file),
// 0o100755 (executable), 0o40000 (subtree). We strip the type
// bits and keep just the permission bits as a regular FileMode.
func modeFromTreeEntry(m filemode.FileMode) os.FileMode {
	bits, err := m.ToOSFileMode()
	if err != nil {
		return 0o644
	}
	return bits.Perm()
}

// readAllFromBlob reads exactly size bytes from r. go-git's
// blob Reader doesn't always satisfy io.ReadFull cleanly, so
// we hand-roll the loop.
func readAllFromBlob(r interface {
	Read([]byte) (int, error)
}, size int64) ([]byte, error) {
	buf := make([]byte, size)
	total := 0
	for total < int(size) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			if total == int(size) {
				return buf, nil
			}
			return nil, err
		}
	}
	return buf, nil
}

// isHexSHA returns true when s is a 40-character hex string (a
// git object SHA-1 in canonical form).
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// preserveDirSuffix is declared in preserve.go.
