package git

// plumbing_commit.go — no-checkout commit primitives.
//
// PlumbingCommit and UpdateRef build commits + advance refs by
// writing directly to the object store, never touching HEAD,
// .git/index, or the working tree. This is the substrate for
// parallel compute: N goroutines on the same *Clone each build
// commits on their own topic branch independently — no fight
// over HEAD/index/worktree.
//
// Locking: both methods take c.lock(). Even though loose-object
// writes are content-addressed (distinct paths), they share the
// same .git/objects/ tree with shellout `git` invocations from
// other code paths (merge.go, rebase.go), and a half-written
// loose-object temp file from go-git can trip git's internal
// BUG() assertions during a concurrent merge → SIGABRT. The
// lock makes those mutually exclusive. The win we actually
// needed (no checkout, parallel topic branches, parallel script
// execution) is preserved — only the in-process object writes
// serialize, and a blob+tree+commit triple is microseconds.
//
// Compare to CommitFiles in commit.go which uses worktree.Add +
// worktree.Commit (porcelain) — that path moves HEAD and updates
// .git/index, which serializes across callers and exposes them
// to mid-checkout interference.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// PlumbingCommitRequest packages inputs for PlumbingCommit.
type PlumbingCommitRequest struct {
	// BaseSHA is the commit to fork from. Its tree becomes the
	// starting point that the Files overlay onto. Empty string
	// means "no parent" — the resulting commit becomes a root
	// commit with whatever Files alone form. Empty repos take
	// the empty-string path; normal flows always pass a SHA.
	BaseSHA string

	// Files overlay onto BaseSHA's tree. Each FileWrite's
	// RepoRelPath replaces (or adds) the entry at that path.
	// Paths NOT in Files keep their content from BaseSHA. To
	// delete an entry from BaseSHA's tree, this v1 doesn't
	// support it — extend with a FileDelete list when the need
	// surfaces.
	Files []FileWrite

	// Message is the complete commit message (subject + body +
	// trailers, all pre-composed by the caller). This layer
	// composes nothing.
	Message string

	// AuthorName / AuthorEmail populate Author + Committer.
	// Both empty falls back to a placeholder (matches CommitFiles).
	AuthorName  string
	AuthorEmail string

	// When sets Author.When + Committer.When. Zero means
	// time.Now() at call time.
	When time.Time
}

// PlumbingCommit builds a commit object directly via the object
// store. Returns the commit SHA. Does NOT update any ref —
// caller follows up with UpdateRef.
//
// Steps:
//  1. Read BaseSHA's tree (or use empty if BaseSHA is "").
//  2. Flatten the base tree into a path→entry map.
//  3. For each FileWrite: store the blob, overlay path→blob into
//     the map.
//  4. Build nested tree objects bottom-up from the flat map,
//     return the root tree SHA.
//  5. Build a commit object pointing at the root tree, with
//     ParentHashes = [BaseSHA] (or empty if BaseSHA is "").
//  6. Store the commit, return its SHA.
//
// Concurrent safety: takes c.lock() for the duration. Even
// though loose-object writes are content-addressed (distinct
// paths), this method shares .git/objects/ with shellout `git`
// invocations elsewhere (merge.go, rebase.go) and a half-written
// loose-object temp file from go-git can trip git's internal
// BUG() assertions during a concurrent merge → SIGABRT. The
// lock makes the two mutually exclusive. Parallelism we
// actually need (script execution + per-task topic branches +
// independent pushes) is preserved.
func (c *Clone) PlumbingCommit(req PlumbingCommitRequest) (string, error) {
	if req.Message == "" {
		return "", fmt.Errorf("git: PlumbingCommit: Message required")
	}
	defer c.lock()()
	stor := c.repo.Storer

	// Step 1+2: flatten base tree into path map.
	pathToEntry := map[string]plumbingTreeEntry{}
	if req.BaseSHA != "" {
		baseHash := plumbing.NewHash(req.BaseSHA)
		commitObj, err := c.repo.CommitObject(baseHash)
		if err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: read base commit %s: %w", req.BaseSHA, err)
		}
		baseTree, err := commitObj.Tree()
		if err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: read base tree: %w", err)
		}
		if err := flattenTree(baseTree, "", pathToEntry); err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: walk base tree: %w", err)
		}
	}

	// Step 3: store blobs + overlay.
	for _, fw := range req.Files {
		path := normalizeRepoPath(fw.RepoRelPath)
		if path == "" {
			return "", fmt.Errorf("git: PlumbingCommit: empty RepoRelPath in FileWrite")
		}
		blobHash, err := storeBlob(stor, fw.Content)
		if err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: store blob for %s: %w", path, err)
		}
		mode := filemode.Regular
		if fw.Mode&0o111 != 0 {
			mode = filemode.Executable
		}
		pathToEntry[path] = plumbingTreeEntry{Hash: blobHash, Mode: mode}
	}

	// Step 4: build nested tree objects, return root tree SHA.
	rootTreeHash, err := buildNestedTree(stor, pathToEntry)
	if err != nil {
		return "", fmt.Errorf("git: PlumbingCommit: build tree: %w", err)
	}

	// Step 5+6: build + store commit.
	when := req.When
	if when.IsZero() {
		when = time.Now()
	}
	sig := object.Signature{
		Name:  req.AuthorName,
		Email: req.AuthorEmail,
		When:  when,
	}
	if sig.Name == "" {
		sig.Name = "Enju git layer"
	}
	if sig.Email == "" {
		sig.Email = "enju-git@localhost"
	}
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   req.Message,
		TreeHash:  rootTreeHash,
	}
	if req.BaseSHA != "" {
		commit.ParentHashes = []plumbing.Hash{plumbing.NewHash(req.BaseSHA)}
	}
	commitObj := stor.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return "", fmt.Errorf("git: PlumbingCommit: encode commit: %w", err)
	}
	commitHash, err := stor.SetEncodedObject(commitObj)
	if err != nil {
		return "", fmt.Errorf("git: PlumbingCommit: store commit: %w", err)
	}
	return commitHash.String(), nil
}

// UpdateRef atomically sets refs/heads/<name> to newSHA. When
// expectedOldSHA is non-empty, the update only succeeds if the
// current value matches — compare-and-swap semantics required
// for concurrent updates from sibling goroutines on the same
// branch. Pass "" for expectedOldSHA to skip the check (allow
// any current value, including non-existent ref).
//
// Acquires c.lock() because ref storage in go-git is not
// safe for concurrent SetReference calls without external
// serialization.
func (c *Clone) UpdateRef(name, newSHA, expectedOldSHA string) error {
	defer c.lock()()
	if name == "" {
		return fmt.Errorf("git: UpdateRef: name required")
	}
	if newSHA == "" {
		return fmt.Errorf("git: UpdateRef: newSHA required")
	}
	refName := plumbing.NewBranchReferenceName(name)
	newHash := plumbing.NewHash(newSHA)
	if expectedOldSHA != "" {
		existing, err := c.repo.Reference(refName, false)
		if err != nil {
			return fmt.Errorf("git: UpdateRef: read existing %s: %w", name, err)
		}
		if existing.Hash().String() != expectedOldSHA {
			return fmt.Errorf("git: UpdateRef: CAS check failed on %s: expected %s, got %s",
				name, expectedOldSHA, existing.Hash().String())
		}
	}
	return c.repo.Storer.SetReference(plumbing.NewHashReference(refName, newHash))
}

// --- internal helpers ---

type plumbingTreeEntry struct {
	Hash plumbing.Hash
	Mode filemode.FileMode
}

// flattenTree walks a go-git Tree recursively, populating `out`
// with path→entry for every blob (regular file, executable, or
// symlink). Subtrees are descended into, not recorded as entries
// in the flat map (the map is by leaf path).
func flattenTree(t *object.Tree, prefix string, out map[string]plumbingTreeEntry) error {
	for _, e := range t.Entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == filemode.Dir {
			sub, err := t.Tree(e.Name)
			if err != nil {
				return fmt.Errorf("descend %s: %w", path, err)
			}
			if err := flattenTree(sub, path, out); err != nil {
				return err
			}
			continue
		}
		out[path] = plumbingTreeEntry{Hash: e.Hash, Mode: e.Mode}
	}
	return nil
}

// normalizeRepoPath cleans a caller-supplied repo-relative path
// for use as a tree-map key: forward slashes, no leading slash,
// no empty/dot segments.
func normalizeRepoPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, "/")
}

// storeBlob writes content as a blob object and returns its hash.
func storeBlob(s storer.EncodedObjectStorer, content []byte) (plumbing.Hash, error) {
	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob writer: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, fmt.Errorf("blob write: %w", err)
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob close: %w", err)
	}
	return s.SetEncodedObject(obj)
}

// buildNestedTree takes a flat path → entry map and produces
// nested git Tree objects bottom-up. Returns the root tree SHA.
//
// Algorithm:
//  1. Group entries by parent directory.
//  2. Synthesize all intermediate dirs (a/b/c.txt → entries for
//     "" knows about subdir "a", "a" knows about subdir "b",
//     "b" has file "c.txt").
//  3. Sort dirs by depth descending (deep dirs first).
//  4. For each dir, build a Tree object: file entries from the
//     map + subdir entries pointing at already-built sub-tree
//     hashes. Encode + store.
//  5. Return the root tree's stored hash.
func buildNestedTree(s storer.EncodedObjectStorer, paths map[string]plumbingTreeEntry) (plumbing.Hash, error) {
	type dirContents struct {
		files   map[string]plumbingTreeEntry
		subdirs map[string]bool
	}
	dirs := map[string]*dirContents{}
	ensureDir := func(d string) *dirContents {
		if _, ok := dirs[d]; !ok {
			dirs[d] = &dirContents{
				files:   map[string]plumbingTreeEntry{},
				subdirs: map[string]bool{},
			}
		}
		return dirs[d]
	}
	ensureDir("") // root always exists, even if empty

	for path, entry := range paths {
		dir, name := splitDirName(path)
		ensureDir(dir).files[name] = entry
		// Walk parent chain to register subdir membership.
		for d := dir; d != ""; {
			parent, child := splitDirName(d)
			ensureDir(parent).subdirs[child] = true
			d = parent
		}
	}

	// Sort dirs by depth descending so children are built first.
	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.SliceStable(dirList, func(i, j int) bool {
		return dirDepth(dirList[i]) > dirDepth(dirList[j])
	})

	treeHashes := map[string]plumbing.Hash{}
	for _, d := range dirList {
		dc := dirs[d]
		entries := make([]object.TreeEntry, 0, len(dc.files)+len(dc.subdirs))
		for name, e := range dc.files {
			entries = append(entries, object.TreeEntry{
				Name: name,
				Mode: e.Mode,
				Hash: e.Hash,
			})
		}
		for name := range dc.subdirs {
			childPath := name
			if d != "" {
				childPath = d + "/" + name
			}
			childHash, ok := treeHashes[childPath]
			if !ok {
				return plumbing.ZeroHash, fmt.Errorf("buildNestedTree: missing subtree %q", childPath)
			}
			entries = append(entries, object.TreeEntry{
				Name: name,
				Mode: filemode.Dir,
				Hash: childHash,
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name < entries[j].Name
		})
		tree := &object.Tree{Entries: entries}
		treeObj := s.NewEncodedObject()
		if err := tree.Encode(treeObj); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("encode tree %q: %w", d, err)
		}
		h, err := s.SetEncodedObject(treeObj)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("store tree %q: %w", d, err)
		}
		treeHashes[d] = h
	}

	return treeHashes[""], nil
}

func dirDepth(p string) int {
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}

func splitDirName(p string) (dir, name string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}
