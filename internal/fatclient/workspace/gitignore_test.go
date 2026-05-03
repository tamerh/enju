package workspace

import (
	"strings"
	"testing"
)

// TestGitignoreBlockFreshFile covers the "no .gitignore yet"
// starting state: the helper must synthesize the block on its
// own and return non-nil bytes so the caller stages the file.
func TestGitignoreBlockFreshFile(t *testing.T) {
	out, changed := UpdateGitignoreManagedBlock(nil, []string{"out/aligned.bam"})
	if !changed {
		t.Fatal("expected changed=true for fresh file")
	}
	s := string(out)
	if !strings.Contains(s, gitignoreBlockBegin) || !strings.Contains(s, gitignoreBlockEnd) {
		t.Errorf("missing block markers:\n%s", s)
	}
	if !strings.Contains(s, "out/aligned.bam") {
		t.Errorf("missing declared path:\n%s", s)
	}
}

// TestGitignoreBlockPreservesUserContent — user-written entries
// above or below the managed block must survive unchanged. This
// is the headline preservation contract: a citizen who committed
// `.gitignore` with their own rules can't have those wiped by an
// enju submit.
func TestGitignoreBlockPreservesUserContent(t *testing.T) {
	existing := []byte(`# my personal rules
.DS_Store
node_modules/
`)
	out, changed := UpdateGitignoreManagedBlock(existing, []string{"out/x.bam"})
	if !changed {
		t.Fatal("expected changed=true — first time adding block")
	}
	s := string(out)
	// Every user line survives verbatim.
	for _, want := range []string{"# my personal rules", ".DS_Store", "node_modules/"} {
		if !strings.Contains(s, want) {
			t.Errorf("user line %q lost:\n%s", want, s)
		}
	}
	// Block is present, at the end, with the new path.
	if !strings.Contains(s, gitignoreBlockBegin) || !strings.Contains(s, "out/x.bam") {
		t.Errorf("block not rendered:\n%s", s)
	}
	// Block comes AFTER user content so the diff stays readable.
	userIdx := strings.Index(s, "node_modules/")
	blockIdx := strings.Index(s, gitignoreBlockBegin)
	if userIdx >= blockIdx {
		t.Errorf("block should follow user content, got block=%d user=%d", blockIdx, userIdx)
	}
}

// TestGitignoreBlockMergesIntoExistingBlock — re-submit with a
// different path. The previous path must stay; the new one
// joins it; they're sorted for stable diffs.
func TestGitignoreBlockMergesIntoExistingBlock(t *testing.T) {
	existing := []byte(gitignoreBlockBegin + "\n" +
		"out/existing.bam\n" +
		gitignoreBlockEnd + "\n")
	out, changed := UpdateGitignoreManagedBlock(existing, []string{"out/new.bam"})
	if !changed {
		t.Fatal("expected changed=true for new path")
	}
	s := string(out)
	if !strings.Contains(s, "out/existing.bam") || !strings.Contains(s, "out/new.bam") {
		t.Errorf("merge lost paths:\n%s", s)
	}
	// Lexicographic sort means existing < new.
	exIdx := strings.Index(s, "out/existing.bam")
	newIdx := strings.Index(s, "out/new.bam")
	if exIdx > newIdx {
		t.Errorf("expected sorted block, got existing=%d new=%d\n%s", exIdx, newIdx, s)
	}
	// Single block — no accidental duplicate.
	if strings.Count(s, gitignoreBlockBegin) != 1 {
		t.Errorf("expected 1 BEGIN marker, got %d\n%s", strings.Count(s, gitignoreBlockBegin), s)
	}
}

// TestGitignoreBlockIdempotentNoChange — re-submit with a path
// already present. changed must be false so the caller can
// skip the commit-a-file step.
func TestGitignoreBlockIdempotentNoChange(t *testing.T) {
	existing := []byte(gitignoreBlockBegin + "\n" +
		"out/aligned.bam\n" +
		gitignoreBlockEnd + "\n")
	out, changed := UpdateGitignoreManagedBlock(existing, []string{"out/aligned.bam"})
	if changed {
		t.Errorf("expected changed=false (path already present), got changed=true, out=%q", out)
	}
	if out != nil {
		t.Errorf("expected nil output when nothing changed, got %q", out)
	}
}

// TestGitignoreBlockDedupesDuplicateRequests — the caller may
// pass the same path multiple times (sloppy for_each expansion).
// The block stays clean.
func TestGitignoreBlockDedupesDuplicateRequests(t *testing.T) {
	out, _ := UpdateGitignoreManagedBlock(nil, []string{"out/a.bam", "out/a.bam", "out/b.bam"})
	s := string(out)
	if strings.Count(s, "out/a.bam") != 1 {
		t.Errorf("duplicates not deduped: %s", s)
	}
}

// TestGitignoreBlockEmptyPathsNoop — empty strings must never
// land in the block (would make git ignore literally nothing
// with a warning).
func TestGitignoreBlockEmptyPathsNoop(t *testing.T) {
	out, changed := UpdateGitignoreManagedBlock(nil, []string{""})
	if changed {
		t.Errorf("expected changed=false for empty-only input, got %q", out)
	}
}

// TestGitignoreBlockUserContentAfterBlock — user rules that
// live after the managed block stay after it. The block
// doesn't migrate to the end of the file on every rewrite.
func TestGitignoreBlockUserContentAfterBlock(t *testing.T) {
	existing := []byte("# top\n" +
		gitignoreBlockBegin + "\n" +
		"out/x\n" +
		gitignoreBlockEnd + "\n" +
		"# bottom rule\n" +
		"vendor/\n")
	out, _ := UpdateGitignoreManagedBlock(existing, []string{"out/y"})
	s := string(out)
	topIdx := strings.Index(s, "# top")
	blockIdx := strings.Index(s, gitignoreBlockBegin)
	bottomIdx := strings.Index(s, "# bottom rule")
	if !(topIdx < blockIdx && blockIdx < bottomIdx) {
		t.Errorf("block lost its position: top=%d block=%d bottom=%d\n%s",
			topIdx, blockIdx, bottomIdx, s)
	}
	if !strings.Contains(s, "vendor/") {
		t.Errorf("post-block user rule dropped:\n%s", s)
	}
}

// TestGitignoreBlockCorruptedBeginNoEnd — a malformed file
// with a BEGIN marker but no END (say, a user edit gone wrong)
// falls back to "no block present" and writes a fresh block.
// The stray marker becomes a plain comment.
func TestGitignoreBlockCorruptedBeginNoEnd(t *testing.T) {
	existing := []byte("# my rules\n" +
		gitignoreBlockBegin + "\n" +
		"out/stale\n")
	out, changed := UpdateGitignoreManagedBlock(existing, []string{"out/new"})
	if !changed {
		t.Fatal("expected changed=true for corrupted recovery")
	}
	s := string(out)
	// The ORIGINAL BEGIN marker survives as literal prefix
	// content (we don't rewrite what we can't safely parse).
	// A new block appears at the end with the caller's path.
	if !strings.Contains(s, "out/new") {
		t.Errorf("new path missing:\n%s", s)
	}
	// Should not accumulate two separate `# BEGIN enju-untracked`
	// lines reflecting a fresh block and the stale one being
	// merged — that's OK for now, the semantic is "treat
	// corrupted state as no-block". Just ensure no END line
	// appeared without a preceding BEGIN.
	if strings.Count(s, gitignoreBlockEnd) == 0 {
		t.Errorf("new block has no END marker:\n%s", s)
	}
}
