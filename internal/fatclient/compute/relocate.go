package compute

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// relocateUntrackedToBigfiles implements the Option-A write
// contract for track:false outputs: the script writes them
// cwd-relative (exactly like tracked outputs), and enju moves
// each into the per-branch bigfiles dir after the script exits.
//
// A declared entry the script instead wrote straight to
// $ENJU_BIGFILES is simply absent from outputDir, so it is not
// resolved here and is left in place — the caller's existing
// ExpandAgainstWorkdir(bigfilesDir) finds it there. That is the
// deliberate escape hatch for huge outputs on a cross-mount
// scratch where the post-exit copy would be unacceptable.
// Genuinely-missing entries are likewise left for that same
// call to report, so missing-artifact semantics are unchanged.
//
// Must run before the commit/checkout step: it clears untracked
// files out of the script CWD (scratch) so the worktree is clean
// of untracked task files at merge time (the Phase-2.6 invariant
// that parallel-merge fan-out relies on).
func relocateUntrackedToBigfiles(outputDir, bigfilesDir string, decls enjuYaml.WriteArtifacts) error {
	if len(decls) == 0 {
		return nil
	}
	// Resolve declarations against the script's CWD. Missing-in-
	// cwd is expected and fine (escape-hatch / true-missing are
	// both the caller's concern); we only move what the script
	// actually produced cwd-relative.
	resolved, _, err := decls.ExpandAgainstWorkdir(outputDir)
	if err != nil {
		return fmt.Errorf("resolving untracked writes in cwd: %w", err)
	}
	for _, e := range resolved {
		src := filepath.Join(outputDir, e.Path)
		dst := filepath.Join(bigfilesDir, e.Path)
		if src == dst {
			// outputDir == bigfilesDir (degenerate/legacy) —
			// the file is already where pickup looks.
			continue
		}
		if _, statErr := os.Stat(src); statErr != nil {
			// Not actually in CWD (symlink/TOCTOU/escape-hatch);
			// leave it for the caller's bigfiles resolve.
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("preparing bigfiles dir for %q: %w", e.Path, err)
		}
		if err := moveFileCrossDevice(src, dst); err != nil {
			return fmt.Errorf("relocating untracked %q to bigfiles: %w", e.Path, err)
		}
	}
	return nil
}

// moveFileCrossDevice renames src→dst, falling back to copy+remove
// when the two live on different filesystems (os.Rename → EXDEV;
// the HPC scratch-vs-shared-store case). Same-filesystem is an
// O(1) atomic rename; the copy path is the documented slow case
// that authors of very large untracked outputs avoid by writing
// $ENJU_BIGFILES directly.
func moveFileCrossDevice(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	// EXDEV is only the trigger; the actual cross-device work is
	// copyThenRemove — extracted so the load-bearing path is
	// directly unit-testable without needing two filesystems.
	return copyThenRemove(src, dst)
}

// copyThenRemove copies src→dst preserving src's permission bits,
// then removes src. The fallback moveFileCrossDevice takes when
// rename can't cross a filesystem boundary (HPC scratch vs shared
// store). Tested directly because EXDEV is awkward to provoke in
// a unit test but this logic is the whole reason the fallback
// exists.
func copyThenRemove(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	in.Close()
	return os.Remove(src)
}
