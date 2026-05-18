package compute

import (
	"os"
	"path/filepath"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

func writeF(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Script wrote a track:false output cwd-relative (Option A) — it
// must end up in the bigfiles dir, gone from CWD, no error.
func TestRelocateUntracked_CwdRelativeMoved(t *testing.T) {
	cwd, big := t.TempDir(), t.TempDir()
	writeF(t, cwd, "out/big.bam", "DATA")
	decls := enjuYaml.WriteArtifacts{{Path: "out/big.bam", Track: false}}

	if err := relocateUntrackedToBigfiles(cwd, big, decls); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(big, "out/big.bam")); err != nil {
		t.Errorf("expected file in bigfiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "out/big.bam")); !os.IsNotExist(err) {
		t.Errorf("expected file gone from cwd, stat err = %v", err)
	}
}

// Escape hatch: script wrote straight to $ENJU_BIGFILES (file not
// in CWD). relocate must be a no-op and not error — the caller's
// bigfiles resolve finds it in place.
func TestRelocateUntracked_EscapeHatchPreserved(t *testing.T) {
	cwd, big := t.TempDir(), t.TempDir()
	writeF(t, big, "out/huge.bam", "ALREADY-THERE")
	decls := enjuYaml.WriteArtifacts{{Path: "out/huge.bam", Track: false}}

	if err := relocateUntrackedToBigfiles(cwd, big, decls); err != nil {
		t.Fatalf("relocate must not error when nothing in cwd: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(big, "out/huge.bam"))
	if err != nil || string(b) != "ALREADY-THERE" {
		t.Errorf("escape-hatch file disturbed: %q err=%v", b, err)
	}
}

// Glob track:false declaration: every matched file relocates.
func TestRelocateUntracked_Glob(t *testing.T) {
	cwd, big := t.TempDir(), t.TempDir()
	writeF(t, cwd, "out/a.bam", "A")
	writeF(t, cwd, "out/b.bam", "B")
	decls := enjuYaml.WriteArtifacts{{Path: "out/*.bam", Track: false}}

	if err := relocateUntrackedToBigfiles(cwd, big, decls); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	for _, f := range []string{"out/a.bam", "out/b.bam"} {
		if _, err := os.Stat(filepath.Join(big, f)); err != nil {
			t.Errorf("%s not relocated: %v", f, err)
		}
	}
}

// Same-filesystem move uses the rename fast path and preserves bytes.
func TestMoveFileCrossDevice_SameFS(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "s")
	dst := filepath.Join(d, "sub", "x")
	if err := os.WriteFile(src, []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := moveFileCrossDevice(src, dst); err != nil {
		t.Fatalf("move: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "payload" {
		t.Errorf("dst content: %q err=%v", b, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src should be gone, err=%v", err)
	}
}

// copyThenRemove is the EXDEV fallback's actual work (the reason
// moveFileCrossDevice exists). EXDEV is awkward to force in a unit
// test, but the copy logic is plain I/O — pin it directly:
// permission bits preserved, bytes identical, source removed.
func TestCopyThenRemove(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	dst := filepath.Join(d, "dst", "out.bin")
	if err := os.WriteFile(src, []byte("cross-device payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyThenRemove(src, dst); err != nil {
		t.Fatalf("copyThenRemove: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "cross-device payload" {
		t.Errorf("content: %q err=%v", b, err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("perm not preserved: got %o want 640", fi.Mode().Perm())
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src must be removed, stat err=%v", err)
	}
}

// No declarations / empty → no-op, no error.
func TestRelocateUntracked_EmptyNoop(t *testing.T) {
	if err := relocateUntrackedToBigfiles(t.TempDir(), t.TempDir(), nil); err != nil {
		t.Fatalf("empty relocate must be a no-op: %v", err)
	}
}
