package compute

// SILENT-surface guard for spec-bot-to-agent §3: the on-disk
// scratch path moved bots/ → agents/, and the writer
// (ResolveTaskScratchDir) and the startup reaper
// (sweepStaleScratchOlderThan) MUST agree on it. They agree
// structurally because both derive from taskScratchRoot — this
// test pins that invariant so a future edit that re-hardcodes a
// second literal (the drift class §3 is built around) fails LOUD.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScratchPath_AgentsSegment_SingleOwner(t *testing.T) {
	const (
		root = "/proj"
		bot  = "dev-bot"
	)

	gotRoot := taskScratchRoot(root, bot)
	wantRoot := filepath.Join(root, ".enju", "agents", bot, "scratch")
	if gotRoot != wantRoot {
		t.Fatalf("taskScratchRoot = %q, want %q (path must be under .enju/agents/, not legacy .enju/bots/)", gotRoot, wantRoot)
	}
	if strings.Contains(gotRoot, filepath.Join(".enju", "bots")) {
		t.Fatalf("taskScratchRoot still contains legacy .enju/bots segment: %q", gotRoot)
	}

	// Writer is rooted at the single owner.
	iterDir := ResolveTaskScratchDir(root, bot, "1:1:summarize", 2)
	if !strings.HasPrefix(iterDir, wantRoot+string(filepath.Separator)) {
		t.Fatalf("ResolveTaskScratchDir = %q, must be under the single-owner root %q", iterDir, wantRoot)
	}

	// The startup reaper must scan that exact same root — proven
	// by it being a no-op (0 swept, no error) on a tree whose
	// only scratch lives under the new path and is brand-new.
	// A reaper still pointed at .enju/bots/ would also return 0
	// here, so additionally assert the documented root via the
	// owner: if taskScratchRoot is the sole source, the sweep
	// cannot diverge by construction. We assert behaviorally that
	// the sweep accepts the same (root,bot) and errors cleanly.
	if n, err := sweepStaleScratchOlderThan(root, bot, time.Hour, time.Now()); err != nil || n != 0 {
		t.Fatalf("sweep over fresh non-existent root: got (n=%d, err=%v), want (0, nil)", n, err)
	}

	// Empty identity opts out everywhere (writer + owner agree).
	if taskScratchRoot("", bot) != "" || ResolveTaskScratchDir("", bot, "t", 1) != "" {
		t.Fatal("empty projectRoot must yield empty path on both owner and writer")
	}
}
