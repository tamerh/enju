package yaml

// Working-tree expansion for writes_artifacts patterns. Lives
// in a separate file from expand.go so the pure string-match
// helpers are usable from coord-side packages that have no
// business touching a filesystem.
//
// ExpandAgainstWorkdir is the client-side primitive: after a
// task's script (or a bot's handler) has finished writing to
// the workspace, this walks the declared patterns and returns
// the concrete file set that should be staged. Optional entries
// fold silently into "no contribution"; required entries that
// produced nothing fold into the missing list so the caller
// can fail the iteration loudly (silent acceptance was the
// data-loss bug fixed earlier this session).
//
// The expanded entries carry the originating Track flag so the
// commit step can split them into "stage into git" vs
// "stat-only, .gitignore-managed" without re-classifying.
// Optional is dropped after expansion — by definition, a
// concrete file we've stat'd is not optional anymore.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExpandAgainstWorkdir walks declared patterns against the
// working tree and returns the concrete file list. Returns
// (entries, missing, error):
//
//   - entries  — concrete WriteArtifacts (each Path resolved to
//                a literal file, Track flag preserved). Sorted
//                by Path for deterministic ordering.
//   - missing  — paths/patterns that produced no files AND
//                were not flagged Optional. Caller surfaces
//                these to abort the iteration.
//   - error    — only for genuine IO problems (permission denied,
//                etc.). A pattern with zero matches is NOT an
//                error here — it lands in `missing`.
//
// workDir is the working-tree root the patterns resolve under.
// Each declared path is treated as repo-relative (POSIX-style
// slashes). The walk uses os.DirFS rooted at workDir so it
// can't escape via `..`.
//
// Symlinks: walked but not followed across the workDir
// boundary. Symlinks to within workDir are treated as regular
// files and their target paths land in the manifest.
//
// Hidden infrastructure dirs (`.git/`, `.bare.git/`, `.clone/`)
// are skipped during recursive directory walks so a declared
// `enju/` (rare but legal) doesn't accidentally sweep enju's
// own internals into the commit.
func (w WriteArtifacts) ExpandAgainstWorkdir(workDir string) ([]WriteArtifact, []string, error) {
	if len(w) == 0 {
		return nil, nil, nil
	}
	var (
		entries []WriteArtifact
		missing []string
		// Dedupe by repo-relative path so two declarations that
		// both cover the same file (a glob + a literal, or a
		// dir + a glob) land once in the commit. Track flag of
		// the first declaration wins — the user-visible promise
		// "this file is tracked" must be honored even if a
		// later declaration would have been untracked.
		seen = make(map[string]struct{})
	)
	add := func(repoRel string, decl WriteArtifact) {
		if _, dup := seen[repoRel]; dup {
			return
		}
		seen[repoRel] = struct{}{}
		entries = append(entries, WriteArtifact{
			Path:  repoRel,
			Track: decl.Track,
		})
	}

	for _, decl := range w {
		matched, err := expandOne(workDir, decl)
		if err != nil {
			return nil, nil, fmt.Errorf("expanding writes %q: %w", decl.Path, err)
		}
		if len(matched) == 0 {
			if !decl.Optional {
				missing = append(missing, decl.Path)
			}
			continue
		}
		for _, repoRel := range matched {
			add(repoRel, decl)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries, missing, nil
}

// expandOne resolves one declared pattern to a slice of repo-
// relative paths that exist under workDir. Returns nil with no
// error when no matches found — the caller decides whether
// that's a missing-required failure or an optional no-op.
func expandOne(workDir string, decl WriteArtifact) ([]string, error) {
	path := decl.Path
	switch {
	case IsDir(path):
		return expandDirectory(workDir, path)
	case IsGlob(path):
		return expandGlob(workDir, path)
	default:
		return expandLiteral(workDir, path)
	}
}

// expandLiteral stat()s the path. Returns one-element slice on
// success, empty on absence. Errors only on unexpected IO
// failures.
func expandLiteral(workDir, repoRel string) ([]string, error) {
	full := filepath.Join(workDir, filepath.FromSlash(repoRel))
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.IsDir() {
		// Literal path that resolves to a directory. The user
		// almost certainly meant the directory form — surface a
		// clear hint rather than committing nothing or the
		// directory entry itself.
		return nil, fmt.Errorf(
			"%q is a directory but the declaration has no trailing `/`; "+
				"use `%s/` to declare directory contents recursively, or "+
				"`%s/*` for shallow glob",
			repoRel, repoRel, repoRel,
		)
	}
	return []string{repoRel}, nil
}

// expandGlob runs filepath.Glob against the working tree. The
// returned paths are absolute (because workDir is absolute);
// we trim back to repo-relative POSIX form.
//
// filepath.Glob doesn't recurse — `src/**/*.go` is NOT
// supported. Callers wanting recursive coverage should use the
// directory form (`src/`) which DOES recurse.
func expandGlob(workDir, pattern string) ([]string, error) {
	full := filepath.Join(workDir, filepath.FromSlash(pattern))
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(workDir, m)
		if err != nil {
			return nil, err
		}
		// Skip directories matched by globs without a trailing
		// slash. `src/api/*` may match a subdirectory; we want
		// the file children of that subdirectory only via the
		// directory form, not via this glob.
		info, err := os.Lstat(m)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

// expandDirectory recursively walks the directory and collects
// every regular file (and symlink) under it. Skips Enju's own
// infrastructure dirs (`.git/`, `.bare.git/`, `.clone/`,
// preserve dirs) so a `enju/` declaration doesn't sweep them
// in. The walk is rooted under workDir so `..` escape isn't
// possible.
func expandDirectory(workDir, declared string) ([]string, error) {
	rel := strings.TrimSuffix(declared, "/")
	root := filepath.Join(workDir, filepath.FromSlash(rel))
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf(
			"%q has a trailing `/` but resolves to a file, not a directory",
			declared,
		)
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if isInfraDir(d.Name()) && path != root {
				return fs.SkipDir
			}
			return nil
		}
		// Regular files and symlinks. Sockets/fifos/devices
		// don't fit the artifact model; ignore them silently —
		// the same shape preserve.go uses.
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		repoRel, rerr := filepath.Rel(workDir, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(repoRel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isInfraDir returns true for directory names that hold Enju's
// own state and should never be swept into a writes_artifacts
// expansion. Matches the names a directory walker would see —
// not full paths — so order of `enju/.bare.git/` segments
// doesn't matter.
func isInfraDir(name string) bool {
	switch name {
	case ".git", ".bare.git", ".clone":
		return true
	}
	if strings.HasSuffix(name, ".preserve-in-progress") {
		return true
	}
	return false
}
