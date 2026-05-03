package mcpgit

// .gitignore managed-block helpers. The untracked-artifacts
// feature declares per-task paths whose content must NOT land
// in git. A managed block in the project's root .gitignore
// enforces that contract at the git layer itself — any
// accidental `git add`/`git commit -a`/`git stash push -u`
// that would otherwise pick the file up hits the ignore rule
// and stays out of the tree.
//
// Block format:
//
//	# BEGIN enju-untracked (managed by enju — do not edit)
//	out/aligned.bam
//	out/scratch/
//	# END enju-untracked
//
// The helper is pure byte-in, byte-out so tests can exercise
// every transition (no block → add; block exists → merge; stale
// block → rewrite) without touching a working tree. Callers
// (the compute wrapper, the fat-client submit helper) plug the
// output bytes into their FileWrite list and commit as usual.

import (
	"bytes"
	"sort"
	"strings"
)

const (
	// gitignoreBlockBegin / gitignoreBlockEnd mark the
	// Enju-managed region of .gitignore. Keep the markers
	// stable — changing them would orphan the block in every
	// existing project and create a second, duplicate block
	// on the next submit.
	gitignoreBlockBegin = "# BEGIN enju-untracked (managed by enju — do not edit)"
	gitignoreBlockEnd   = "# END enju-untracked"
)

// UpdateGitignoreManagedBlock returns a new .gitignore body
// that contains the union of (existing managed-block entries,
// additional untracked paths). Everything outside the managed
// block is preserved verbatim.
//
//   - `existing` is the current .gitignore contents (may be
//     nil/empty if the file doesn't exist yet).
//   - `addPaths` is the set of paths to guarantee are listed in
//     the managed block. Duplicates and empty strings are
//     filtered; order in the output is lexicographic for stable
//     diffs across submits.
//
// When the result's managed block matches the existing block's
// contents byte-for-byte the function still returns the full
// file bytes — callers decide whether to skip the write by
// comparing against what's already on disk.
//
// Returns (nil, false) when addPaths adds nothing new — lets
// callers cheaply short-circuit the commit-a-.gitignore step
// in the common "all declared paths already listed" case.
func UpdateGitignoreManagedBlock(existing []byte, addPaths []string) ([]byte, bool) {
	// Filter + dedupe incoming paths. Empty strings (can creep
	// in from unguarded for_each expansions) stay out of the
	// block so we don't emit a blank ignore line.
	add := make(map[string]struct{}, len(addPaths))
	for _, p := range addPaths {
		if p == "" {
			continue
		}
		add[p] = struct{}{}
	}

	// Early exit: no meaningful input AND no pre-existing
	// block. Nothing to do — don't synthesize an empty block.
	if len(add) == 0 && !existingHasBlock(existing) {
		return nil, false
	}

	preText, blockPaths, postText := splitGitignore(existing)

	// Merge: start with whatever the block already has, then
	// add the caller's paths. Post-dedup, the set is the full
	// union.
	blockSet := make(map[string]struct{}, len(blockPaths)+len(add))
	for _, p := range blockPaths {
		blockSet[p] = struct{}{}
	}
	changed := false
	for p := range add {
		if _, ok := blockSet[p]; !ok {
			blockSet[p] = struct{}{}
			changed = true
		}
	}
	if !changed && len(preText) == 0 && len(postText) == 0 && existingHasBlock(existing) {
		// No new paths AND the existing file is exactly a
		// well-formed block. Nothing to write.
		return nil, false
	}
	if !changed && existingHasBlock(existing) {
		// No new paths, but the file has pre/post content
		// around the block. Still nothing to change — callers
		// shouldn't rewrite.
		return nil, false
	}

	sortedPaths := make([]string, 0, len(blockSet))
	for p := range blockSet {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)

	var out bytes.Buffer
	if len(preText) > 0 {
		out.Write(preText)
		// Ensure a blank line separator between existing
		// content and the managed block, so the generated
		// .gitignore reads cleanly for humans.
		if !bytes.HasSuffix(preText, []byte("\n\n")) {
			if !bytes.HasSuffix(preText, []byte("\n")) {
				out.WriteByte('\n')
			}
			out.WriteByte('\n')
		}
	}
	out.WriteString(gitignoreBlockBegin)
	out.WriteByte('\n')
	for _, p := range sortedPaths {
		out.WriteString(p)
		out.WriteByte('\n')
	}
	out.WriteString(gitignoreBlockEnd)
	out.WriteByte('\n')
	if len(postText) > 0 {
		// Same separator rule on the trailing side — one
		// blank line between the block and whatever follows.
		if !bytes.HasPrefix(postText, []byte("\n")) {
			out.WriteByte('\n')
		}
		out.Write(postText)
	}
	return out.Bytes(), true
}

// existingHasBlock reports whether the raw .gitignore bytes
// contain the managed block marker pair. Used to distinguish
// "nothing to change" from "file doesn't exist yet".
func existingHasBlock(data []byte) bool {
	return bytes.Contains(data, []byte(gitignoreBlockBegin)) &&
		bytes.Contains(data, []byte(gitignoreBlockEnd))
}

// splitGitignore partitions the current .gitignore into the
// bytes before the managed block, the ordered list of paths
// inside the block, and the bytes after the block. When the
// block is absent, preText returns the whole file (no
// markers found) and blockPaths / postText are empty.
//
// Trailing whitespace outside the block is preserved — the
// pre/post segments are byte-for-byte faithful so user
// comments and orderings survive the round-trip.
func splitGitignore(data []byte) (preText []byte, blockPaths []string, postText []byte) {
	begin := bytes.Index(data, []byte(gitignoreBlockBegin))
	if begin < 0 {
		// No managed block yet. Everything is prefix; new
		// block gets appended by the caller.
		return trimTrailingEmptyLines(data), nil, nil
	}
	end := bytes.Index(data[begin:], []byte(gitignoreBlockEnd))
	if end < 0 {
		// Corrupted block — begin marker but no end. Treat
		// as "no block": preserve as prefix, caller writes
		// a fresh block at the end. Harmless in practice
		// (the old begin marker becomes a plain comment).
		return trimTrailingEmptyLines(data), nil, nil
	}
	end += begin
	// Advance end past the end-marker line so postText starts
	// on the next line.
	endLine := end + len(gitignoreBlockEnd)
	if endLine < len(data) && data[endLine] == '\n' {
		endLine++
	}

	preText = trimTrailingEmptyLines(data[:begin])

	// Block body sits between begin-marker EOL and end-marker.
	blockStart := begin + len(gitignoreBlockBegin)
	if blockStart < len(data) && data[blockStart] == '\n' {
		blockStart++
	}
	body := data[blockStart:end]
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		blockPaths = append(blockPaths, trimmed)
	}

	postText = bytes.TrimLeft(data[endLine:], "\n")
	return preText, blockPaths, postText
}

// trimTrailingEmptyLines removes trailing "\n"-only padding so
// the caller can reconstruct the file with controlled spacing
// between segments without compounding blank lines across many
// submits.
func trimTrailingEmptyLines(b []byte) []byte {
	return bytes.TrimRight(b, "\n")
}
