package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/common/wire"
)

// cmdRuns lists runs for the active project. Wraps
// FatClient.ListRuns with operator-friendly filtering and a
// table renderer.
//
// Distinct from `enju status` — status shows a single project's
// active runs + recent terminal runs, capped at small counts for
// orientation. `enju runs` is the dump-everything view with
// filters: useful when triaging "show me every failed run this
// week" or "what's currently running across this project."
func cmdRuns(args []string) {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	projectID := fs.Int64("project", 0, "Override project resolution (numeric id)")
	statusFilter := fs.String("status", "",
		"Filter by state: literal coord states or convenience aliases. "+
			"Literals: ready, claimed, running, accepted, completed, failed, aborted, terminated, waiting, paused, etc. "+
			"Aliases: 'active' (covers active+waiting+idle — anything not terminal), 'done' (alias for completed). "+
			"Comma-separated for multiple; aliases and literals can mix. "+
			"Known limit: when 'active' appears in the list it always expands to the alias set — there's no way to ask for ONLY the literal 'active' state today. File an issue if you hit this.")
	last := fs.Int("last", 20, "Cap on rows rendered (0 = all)")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	asJSON := fs.Bool("json", false, "Emit the wire.Run array as JSON")
	terminateSeq := fs.Int("terminate", 0, "Irreversibly terminate run <seq> instead of listing (cascade-skips non-terminal tasks, abandons open claims). Same operation as MCP enju_terminate_run.")
	reason := fs.String("reason", "", "Audit reason recorded in the run_terminated event (used with -terminate).")
	fs.Parse(args)

	sess := openCLISession(*coordOverride)
	ctx := context.Background()

	entry, err := resolveActiveProject(sess, *projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs: %v\n", err)
		os.Exit(2)
	}

	// -terminate short-circuits the list path: a thin wrapper over
	// the same FatClient.TerminateRun the MCP tool calls. The
	// explicit flag is the confirmation (matches MCP — no extra
	// prompt, so it stays scriptable).
	if *terminateSeq > 0 {
		if err := sess.FC.TerminateRun(ctx, entry.ID, *terminateSeq, *reason); err != nil {
			fmt.Fprintf(os.Stderr, "terminate run %d:%d: %v\n", entry.ID, *terminateSeq, err)
			os.Exit(1)
		}
		fmt.Printf("⊘ run %d:%d terminated (irreversible; topic branches preserved in git)\n", entry.ID, *terminateSeq)
		return
	}

	// B-3: registry never stored the project name, so the table
	// header rendered "(unnamed)". Pull it from coord (source of
	// truth) before rendering; no-op when already known.
	backfillProjectName(ctx, sess, entry)

	runs, err := sess.FC.ListRuns(ctx, entry.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs: %v\n", err)
		os.Exit(1)
	}

	// L5: warn on --status tokens that match no run state or alias
	// (e.g. a typo'd "acvtive") rather than silently filtering to
	// nothing. Non-fatal — we still filter by any recognized tokens.
	if unknown := unknownStatusTokens(*statusFilter); len(unknown) > 0 {
		fmt.Fprintf(os.Stderr,
			"runs: ignoring unrecognized --status value(s): %s (valid: active, waiting, idle, paused, completed, failed, aborted, terminated; aliases: live, done)\n",
			strings.Join(unknown, ", "))
	}

	filtered := filterRuns(runs, parseStatusFilter(*statusFilter), *last)

	if *asJSON {
		b, _ := json.MarshalIndent(filtered, "", "  ")
		fmt.Println(string(b))
		return
	}
	renderRunsTable(os.Stdout, entry.Name, entry.ID, filtered)
}

// parseStatusFilter converts the comma-separated --status flag
// into a normalized set. "active" maps to the live states
// (active, waiting, claimed) so the operator can ask the
// natural question "what's still running?" without learning
// every internal state name. Empty filter returns nil → no
// filtering applied.
func parseStatusFilter(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, tok := range strings.Split(raw, ",") {
		t := strings.TrimSpace(strings.ToLower(tok))
		if t == "" {
			continue
		}
		switch t {
		case "active", "live":
			// Convenience: anything not yet terminal.
			out["active"] = true
			out["waiting"] = true
			out["idle"] = true
		case "done":
			// Convenience alias for the success terminal.
			out["completed"] = true
		default:
			out[t] = true
		}
	}
	return out
}

// knownRunStatusToken reports whether t (already lowercased) is a
// run state or a recognized --status alias.
func knownRunStatusToken(t string) bool {
	switch t {
	case "active", "live", "waiting", "idle", "paused",
		"completed", "done", "failed", "aborted", "terminated":
		return true
	}
	return false
}

// unknownStatusTokens returns the --status tokens that match no run
// state or alias, so cmdRuns can warn on a typo (L5) instead of
// letting it silently filter to an empty list.
func unknownStatusTokens(raw string) []string {
	var unknown []string
	for _, tok := range strings.Split(raw, ",") {
		t := strings.TrimSpace(strings.ToLower(tok))
		if t == "" {
			continue
		}
		if !knownRunStatusToken(t) {
			unknown = append(unknown, strings.TrimSpace(tok))
		}
	}
	return unknown
}

// filterRuns drops runs whose state isn't in `wanted` (nil = no
// filter), then caps the result at `limit`. Caller-visible
// ordering: most-recent first (descending seq), matching the
// row order operators expect from `git log`-style listings.
func filterRuns(runs []wire.Run, wanted map[string]bool, limit int) []wire.Run {
	sorted := make([]wire.Run, len(runs))
	copy(sorted, runs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq > sorted[j].Seq })

	if wanted != nil {
		out := sorted[:0]
		for _, r := range sorted {
			if wanted[strings.ToLower(r.State)] {
				out = append(out, r)
			}
		}
		sorted = out
	}
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	return sorted
}

// renderRunsTable prints the seq, name, state, branch, task count
// in a fixed-width table. Headers + dividers are intentionally
// minimal so the output stays grep-friendly.
func renderRunsTable(w io.Writer, projectName string, projectID int64, runs []wire.Run) {
	if projectName == "" {
		projectName = "(unnamed)"
	}
	fmt.Fprintf(w, "Project: %s (id=%d)\n\n", projectName, projectID)
	if len(runs) == 0 {
		fmt.Fprintln(w, "No runs match.")
		return
	}
	fmt.Fprintf(w, "%-4s  %-30s  %-10s  %-6s  %s\n", "SEQ", "NAME", "STATE", "TASKS", "BRANCH")
	for _, r := range runs {
		fmt.Fprintf(w, "%-4d  %-30s  %-10s  %-6d  %s\n",
			r.Seq, truncateRunes(r.Name, 30), r.State, r.TaskCount, r.Branch)
	}
}

// truncateRunes shortens s to at most max RUNES, suffixing "…"
// when it had to cut. Rune-aware (unlike s[:max-1]+"…") so
// CJK / emoji / combining-character project names don't get cut
// mid-rune into invalid UTF-8 that renders as a replacement
// character.
//
// What's pinned: the rune count. The output is at most max runes.
// What's NOT pinned: the visible column width. CJK / wide-emoji
// runes render as two terminal columns each, so a 4-rune CJK
// truncation followed by Go's %-30s padding produces a row that
// occupies more than 30 columns in a CJK-aware terminal. Cells
// drift visually but the rune-count invariant holds. Fixing
// column alignment needs a fwidth helper (golang.org/x/text/width)
// and is deferred. The byte-cut alternative produced strictly
// worse output (invalid UTF-8 in some terminals), so this is a
// strict improvement even without fwidth.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
