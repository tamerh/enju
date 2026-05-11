package gitv6

// Regression test for the "bot stop/start leaves a corrupt
// clone" bug. When a bot's fetch is interrupted (kill signal
// during bot_stop), git's fetch protocol leaves a half-written
// pack file at .git/objects/pack/tmp_pack_<n>. On the next
// fetch, git tries to read these as real packs and fails with
// "malformed pack file: bad signature".
//
// The fix: at OpenClone time, sweep any stale tmp_pack_* and
// tmp_idx_* files from the pack dir. Cheap (one readdir),
// idempotent, no false positives — git's normal lifecycle
// renames temp files into pack-<sha>.* on success, so any
// surviving tmp_* file is by definition leftover.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenCloneSweepsStaleTempPackFiles pins the recovery
// behavior. Set up a clone with a stale tmp_pack_* file (the
// signature of an interrupted fetch); OpenClone should remove
// it.
func TestOpenCloneSweepsStaleTempPackFiles(t *testing.T) {
	// Make a minimal valid clone via the existing helper so
	// OpenClone has something to attach to. InitLocal seeds
	// it with a single commit.
	workDir := t.TempDir()
	if _, err := InitLocal(workDir, "", nil); err != nil {
		t.Fatalf("InitLocal: %v", err)
	}

	// Plant a stale tmp_pack file — the exact filename pattern
	// git uses when a fetch is interrupted mid-receive.
	packDir := filepath.Join(workDir, ".git", "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleTmpPack := filepath.Join(packDir, "tmp_pack_1234567890")
	if err := os.WriteFile(staleTmpPack, []byte("garbage from interrupted fetch"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleTmpIdx := filepath.Join(packDir, "tmp_idx_9876543210")
	if err := os.WriteFile(staleTmpIdx, []byte("garbage idx"), 0o644); err != nil {
		t.Fatal(err)
	}

	// OpenClone — must succeed AND must have swept the stale
	// temp files.
	if _, err := OpenClone(workDir, "", nil); err != nil {
		t.Fatalf("OpenClone: %v", err)
	}

	if _, err := os.Stat(staleTmpPack); !os.IsNotExist(err) {
		t.Errorf("stale tmp_pack file not swept: %v (err=%v)", staleTmpPack, err)
	}
	if _, err := os.Stat(staleTmpIdx); !os.IsNotExist(err) {
		t.Errorf("stale tmp_idx file not swept: %v (err=%v)", staleTmpIdx, err)
	}
}

// TestOpenCloneLeavesRealPacksAlone is the safety guard: the
// sweep must only touch tmp_* files, not the legitimate
// pack-<sha>.{idx,pack,rev} files that git creates on
// successful fetch. Without this guard a too-aggressive sweep
// would delete real packs and make the clone unusable.
func TestOpenCloneLeavesRealPacksAlone(t *testing.T) {
	workDir := t.TempDir()
	if _, err := InitLocal(workDir, "", nil); err != nil {
		t.Fatalf("InitLocal: %v", err)
	}

	// Plant fake-but-correctly-named pack files (the sweep
	// should NOT touch these).
	packDir := filepath.Join(workDir, ".git", "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realLooking := []string{
		"pack-abcdef0123456789.pack",
		"pack-abcdef0123456789.idx",
		"pack-abcdef0123456789.rev",
	}
	for _, name := range realLooking {
		if err := os.WriteFile(filepath.Join(packDir, name), []byte("legit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := OpenClone(workDir, "", nil); err != nil {
		t.Fatalf("OpenClone: %v", err)
	}

	for _, name := range realLooking {
		if _, err := os.Stat(filepath.Join(packDir, name)); err != nil {
			t.Errorf("real pack file %s was swept (err=%v) — the sweep is too aggressive", name, err)
		}
	}
}
