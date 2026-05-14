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
	statusFilter := fs.String("status", "", "Filter by state: active | done | failed | aborted | terminated | waiting | paused (comma-separated for multiple)")
	last := fs.Int("last", 20, "Cap on rows rendered (0 = all)")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	asJSON := fs.Bool("json", false, "Emit the wire.Run array as JSON")
	fs.Parse(args)

	sess := openCLISession(*coordOverride)
	ctx := context.Background()

	entry, err := resolveStatusProject(sess, *projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs: %v\n", err)
		os.Exit(2)
	}

	runs, err := sess.FC.ListRuns(ctx, entry.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runs: %v\n", err)
		os.Exit(1)
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
		name := r.Name
		if len(name) > 30 {
			name = name[:29] + "…"
		}
		fmt.Fprintf(w, "%-4d  %-30s  %-10s  %-6d  %s\n",
			r.Seq, name, r.State, r.TaskCount, r.Branch)
	}
}
