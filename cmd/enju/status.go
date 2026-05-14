package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

// cmdStatus prints a snapshot of the operator's enju state. v1
// surface (per scope-narrow agreement):
//   - Active project (id, name, local path)
//   - Coord URL + reachability ping
//   - Runs for the active project (state + branch + task count)
//
// Deferred to follow-ups:
//
//	--watch       event-log subscription primitive lands later
//	bot list      blocked on the bot-supervisor CLI wiring (sibling spec)
//	recent commits  would shell out to git; not load-bearing for v1
func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	projectID := fs.Int64("project", 0, "Override project resolution (numeric id)")
	all := fs.Bool("all", false, "List every registered project on this machine instead of one")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	asJSON := fs.Bool("json", false, "Emit machine-readable JSON")
	fs.Parse(args)

	sess := openCLISession(*coordOverride)
	ctx := context.Background()

	if *all {
		if err := renderAllProjects(ctx, sess, *asJSON); err != nil {
			fmt.Fprintf(os.Stderr, "status: %v\n", err)
			os.Exit(1)
		}
		return
	}

	entry, err := resolveActiveProject(sess, *projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(2)
	}
	if err := renderProjectStatus(ctx, sess, entry, *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(1)
	}
}

// resolveActiveProject returns the registry entry the CLI verb
// should operate on. If the operator passed --project, look up
// by id; otherwise walk cwd → ancestors looking for a registered
// LocalPath. A miss here is a usage error: there's no useful
// default for a per-project view.
//
// Shared across status, runs, dag (and any future verb that
// takes --project). Originally named resolveStatusProject when
// only status used it; renamed because the scope is now broader
// than the status command.
func resolveActiveProject(sess *cliSession, override int64) (*projectreg.Entry, error) {
	reg := sess.FC.ProjectRegistry()
	if reg == nil {
		return nil, fmt.Errorf("no project registry configured")
	}
	entries, err := reg.List()
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	if override > 0 {
		for i := range entries {
			if entries[i].ID == override {
				return &entries[i], nil
			}
		}
		return nil, fmt.Errorf("project %d not registered on this machine; run `enju status --all` to list known projects", override)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	if e := pickContainingEntry(entries, cwd); e != nil {
		return e, nil
	}
	return nil, fmt.Errorf("no registered project covers %s; pass --project ID or run `enju status --all`", cwd)
}

type statusReport struct {
	ProjectID     int64      `json:"project_id"`
	ProjectName   string     `json:"project_name"`
	ProjectPath   string     `json:"project_path"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	Coordinator   string     `json:"coordinator"`
	CoordOK       bool       `json:"coordinator_ok"`
	CoordError    string     `json:"coordinator_error,omitempty"`
	As            string     `json:"as,omitempty"`
	Runs          []wire.Run `json:"runs,omitempty"`
}

func renderProjectStatus(ctx context.Context, sess *cliSession, entry *projectreg.Entry, asJSON bool) error {
	rep := statusReport{
		ProjectID:     entry.ID,
		ProjectName:   entry.Name,
		ProjectPath:   entry.LocalPath,
		DefaultBranch: entry.DefaultBranch,
		Coordinator:   sess.URL,
	}
	if sess.Creds != nil {
		rep.As = sess.Creds.Username
	}

	// Use ListProjects as the cheapest reachability probe; it
	// returns 200 + array on success regardless of whether the
	// caller is a member of the active project (membership is
	// enforced per-resource, not on the list endpoint).
	if _, perr := sess.FC.ListProjects(ctx); perr != nil {
		rep.CoordError = perr.Error()
	} else {
		rep.CoordOK = true
		// Only attempt the per-project run list when coord is
		// reachable — saves a guaranteed-failing round trip
		// when the coord is down.
		runs, rerr := sess.FC.ListRuns(ctx, entry.ID)
		if rerr != nil {
			rep.CoordError = rerr.Error()
			rep.CoordOK = false
		} else {
			rep.Runs = runs
		}
	}

	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	renderProjectStatusHuman(os.Stdout, rep)
	return nil
}

func renderProjectStatusHuman(w io.Writer, r statusReport) {
	fmt.Fprintf(w, "Project: %s (id=%d)\n", r.ProjectName, r.ProjectID)
	fmt.Fprintf(w, "Path:    %s\n", r.ProjectPath)
	if r.DefaultBranch != "" {
		fmt.Fprintf(w, "Default branch: %s\n", r.DefaultBranch)
	}
	// Render As: before the coord status. Identity is most
	// useful precisely when coord is unreachable ("am I about
	// to retry as the wrong user?") — surfacing it outside the
	// CoordOK branch keeps the line visible in the failure path
	// too.
	if r.As != "" {
		fmt.Fprintf(w, "As:      @%s\n", r.As)
	}
	if r.CoordOK {
		fmt.Fprintf(w, "Coord:   %s (✓)\n", r.Coordinator)
	} else {
		fmt.Fprintf(w, "Coord:   %s (✗ %s)\n", r.Coordinator, r.CoordError)
		return
	}

	if len(r.Runs) == 0 {
		fmt.Fprintln(w, "\nNo runs yet.")
		return
	}

	active, terminal := splitRunsByState(r.Runs)
	if len(active) > 0 {
		fmt.Fprintln(w, "\nActive runs:")
		for _, run := range active {
			fmt.Fprintf(w, "  #%d %-30s %-10s tasks=%d branch=%s\n",
				run.Seq, run.Name, run.State, run.TaskCount, run.Branch)
		}
	}
	if len(terminal) > 0 {
		fmt.Fprintf(w, "\nRecent (last %d):\n", len(terminal))
		for _, run := range terminal {
			fmt.Fprintf(w, "  #%d %-30s %-10s tasks=%d branch=%s\n",
				run.Seq, run.Name, run.State, run.TaskCount, run.Branch)
		}
	}
}

// splitRunsByState partitions a run list into (active, recent
// terminal). Active = anything not in a terminal state. Recent
// is capped at 5 so a project with hundreds of completed runs
// doesn't drown the active set; sort is by seq descending so
// "most recent" wins regardless of input order.
func splitRunsByState(runs []wire.Run) (active, recent []wire.Run) {
	sorted := make([]wire.Run, len(runs))
	copy(sorted, runs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq > sorted[j].Seq })
	for _, r := range sorted {
		if isTerminalRunState(r.State) {
			if len(recent) < 5 {
				recent = append(recent, r)
			}
		} else {
			active = append(active, r)
		}
	}
	// Preserve seq-ascending order for the active set to match
	// what operators expect ("oldest in-flight first" reads
	// naturally as "what should I unblock next").
	sort.Slice(active, func(i, j int) bool { return active[i].Seq < active[j].Seq })
	return active, recent
}

func isTerminalRunState(s string) bool {
	switch strings.ToLower(s) {
	case "completed", "failed", "aborted", "terminated":
		return true
	}
	return false
}

// renderAllProjects is the --all branch: a flat listing of every
// project the registry knows about, with the local path. Useful
// when the operator forgot which project ids are registered on
// this machine.
func renderAllProjects(ctx context.Context, sess *cliSession, asJSON bool) error {
	reg := sess.FC.ProjectRegistry()
	if reg == nil {
		return fmt.Errorf("no project registry configured")
	}
	entries, err := reg.List()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].LastTouched.After(entries[j].LastTouched) })

	if asJSON {
		b, _ := json.MarshalIndent(entries, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stdout, "No registered projects on this machine.")
		fmt.Fprintln(os.Stdout, "  Run `enju go <workflow.yaml>` in a project directory to register.")
		return nil
	}
	fmt.Fprintf(os.Stdout, "Coordinator: %s\n\n", sess.URL)
	fmt.Fprintf(os.Stdout, "%-4s %-30s %s\n", "ID", "NAME", "PATH")
	for _, e := range entries {
		display := e.LocalPath
		if rel, err := filepath.Rel(mustHome(), e.LocalPath); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				display = "~"
			} else {
				display = "~/" + rel
			}
		}
		name := e.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(os.Stdout, "%-4d %-30s %s\n", e.ID, name, display)
	}
	return nil
}

func mustHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
