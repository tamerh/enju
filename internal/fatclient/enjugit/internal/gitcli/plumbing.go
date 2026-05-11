package gitcli

// plumbing.go — PlumbingCommit (Phase 6). Builds a commit
// object directly via git plumbing commands without touching
// HEAD, the real .git/index, or the working tree. Parallel-safe
// because the index manipulation happens in a temp-file index
// (via GIT_INDEX_FILE) — siblings on the same clone don't
// fight over the shared real index.
//
// Pipeline:
//   1. hash-object -w to store blobs (writes loose objects /
//      pack — content-addressed, naturally idempotent).
//   2. Use a temp index file (GIT_INDEX_FILE points at it):
//      - read-tree <BaseSHA> to seed with base tree's content,
//        OR start empty when BaseSHA is "".
//      - update-index --add --cacheinfo for each overlay file.
//   3. write-tree against the temp index → root tree SHA.
//   4. commit-tree <tree> [-p <BaseSHA>] -m <msg> with
//      author/committer env vars → commit SHA.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PlumbingCommit builds a commit object directly via the
// object store. Returns the commit SHA. Does NOT update any ref
// — caller follows up with UpdateRef. Does NOT touch HEAD, the
// real .git/index, or the working tree.
//
// Concurrent safety: takes c.lock() for the duration. The temp
// index approach means siblings on the same Clone could in
// theory go fully parallel, but loose-object writes still share
// .git/objects/, and the lock keeps us conservative for now.
// Parallelism that matters (script execution, downstream
// pushes) lives outside this verb.
func (c *Clone) PlumbingCommit(req PlumbingCommitRequest) (string, error) {
	if req.Message == "" {
		return "", fmt.Errorf("git: PlumbingCommit: Message required")
	}
	defer c.lock()()

	// Validate BaseSHA exists locally if provided. Mirrors
	// gitv6's CommitObject(baseHash) pre-check so callers get a
	// clean error before any side effect.
	if req.BaseSHA != "" {
		if _, err := runGit(c.workDir, []string{"cat-file", "-e", req.BaseSHA + "^{commit}"}, runOpts{}); err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: read base commit %s: %w", req.BaseSHA, err)
		}
	}

	// Temp index file. GIT_INDEX_FILE overrides the real
	// .git/index for any index-touching git command we run with
	// this env. Anchor under .git/ so the file is in the same
	// fs/inode space as the real index (some FS won't tolerate
	// cross-volume operations).
	idxFile, err := os.CreateTemp(filepath.Join(c.workDir, ".git"), "plumbing-index-*")
	if err != nil {
		return "", fmt.Errorf("git: PlumbingCommit: temp index: %w", err)
	}
	idxPath := idxFile.Name()
	idxFile.Close()
	defer os.Remove(idxPath)

	idxEnv := []string{"GIT_INDEX_FILE=" + idxPath}

	// Initialize the temp index. CreateTemp produced an empty
	// zero-byte file which git would read as a truncated index;
	// we need a valid empty-index header first. `read-tree
	// --empty` writes one. Then, if BaseSHA is given, overlay
	// its tree on top.
	if _, err := runGit(c.workDir, []string{"read-tree", "--empty"}, runOpts{extraEnv: idxEnv}); err != nil {
		return "", fmt.Errorf("git: PlumbingCommit: init temp index: %w", err)
	}
	if req.BaseSHA != "" {
		if _, err := runGit(c.workDir, []string{"read-tree", req.BaseSHA}, runOpts{extraEnv: idxEnv}); err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: read-tree %s: %w", req.BaseSHA, err)
		}
	}

	// Store each overlay blob and stage it in the temp index.
	for _, fw := range req.Files {
		path := normalizeRepoPath(fw.RepoRelPath)
		if path == "" {
			return "", fmt.Errorf("git: PlumbingCommit: empty RepoRelPath in FileWrite")
		}
		// hash-object -w writes a blob object and prints its
		// SHA. --stdin reads content from our stdin pipe so
		// we don't have to materialize fw.Content on disk.
		blobOut, err := runGit(c.workDir,
			[]string{"hash-object", "-w", "--stdin"},
			runOpts{stdin: fw.Content})
		if err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: hash-object for %s: %w", path, err)
		}
		blobSHA := strings.TrimSpace(string(blobOut))

		// Pick the cacheinfo mode. gitv6 maps fw.Mode &
		// 0o111 → executable; everything else → regular. We
		// don't support symlinks / submodules here (matches
		// gitv6's filemode.Regular-or-Executable scope).
		mode := "100644"
		if fw.Mode&0o111 != 0 {
			mode = "100755"
		}

		// update-index --add --cacheinfo <mode>,<sha>,<path>
		// inserts/replaces an entry in the temp index. --add
		// allows new paths; without it, update-index errors on
		// untracked paths.
		cacheArg := mode + "," + blobSHA + "," + path
		if _, err := runGit(c.workDir,
			[]string{"update-index", "--add", "--cacheinfo", cacheArg},
			runOpts{extraEnv: idxEnv}); err != nil {
			return "", fmt.Errorf("git: PlumbingCommit: update-index for %s: %w", path, err)
		}
	}

	// Materialize the tree from temp index.
	treeOut, err := runGit(c.workDir, []string{"write-tree"}, runOpts{extraEnv: idxEnv})
	if err != nil {
		return "", fmt.Errorf("git: PlumbingCommit: write-tree: %w", err)
	}
	rootTreeSHA := strings.TrimSpace(string(treeOut))

	// Build the commit object via commit-tree. Authorship comes
	// from env vars; no GIT_INDEX_FILE needed (commit-tree
	// doesn't touch the index).
	when := req.When
	if when.IsZero() {
		when = time.Now()
	}
	commitEnv := authorEnvVars(req.AuthorName, req.AuthorEmail, when)

	args := []string{"commit-tree", rootTreeSHA, "-m", req.Message}
	if req.BaseSHA != "" {
		args = []string{"commit-tree", rootTreeSHA, "-p", req.BaseSHA, "-m", req.Message}
	}
	commitOut, err := runGit(c.workDir, args, runOpts{extraEnv: commitEnv})
	if err != nil {
		return "", fmt.Errorf("git: PlumbingCommit: commit-tree: %w", err)
	}
	return strings.TrimSpace(string(commitOut)), nil
}

// normalizeRepoPath cleans a caller-supplied repo-relative path
// for use as a git cacheinfo path: forward slashes, no leading
// slash, no empty/dot segments.
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
