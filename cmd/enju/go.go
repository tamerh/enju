package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// cmdGo wires register-if-needed + create_run + execute_run into
// the Snakemake-style entry point spec'd in
// docs/cli-commands.md § enju go.
//
// Out of scope for this revision (deferred per scope-narrowing
// discussion):
//
//	--watch       requires the event-log subscription primitive
//	              that lands with the sync-model work.
//	--dry-run     useful but additive; ship after the core flow.
//	--no-bots     no-op today since `enju go` doesn't start bots
//	              at all (the bot auto-lifecycle is in-flight
//	              in a sibling spec). Compute tasks drain;
//	              citizen-action gates surface as Blocker.
//	--parallel    ExecuteRun supports it but the multi-task
//	              commit-author / scratch-dir contention story
//	              isn't worth surfacing on the CLI yet.
func cmdGo(args []string) {
	fs := flag.NewFlagSet("go", flag.ExitOnError)
	name := fs.String("name", "", "Project name when auto-registering (default: cwd basename)")
	branch := fs.String("base-branch", "", "Override the run's base branch (passed through to create_run; default: project default)")
	paramsArg := fs.String("params", "", "k=v[,k=v...] template parameter values")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	asJSON := fs.Bool("json", false, "Stream per-task status as JSONL on stdout")
	maxTasks := fs.Int("max-tasks", 1000, "Cap on compute tasks drained in one go (safety net)")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: enju go <workflow.yaml> [--name X] [--base-branch X] [--params k=v,k=v] [--json]")
		os.Exit(2)
	}
	workflowArg := fs.Arg(0)

	params, perr := parseParamsArg(*paramsArg)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "--params: %v\n", perr)
		os.Exit(2)
	}

	sess := openCLISession(*coordOverride)
	ctx := context.Background()
	logf(*asJSON, "▶ coord %s (as @%s)", sess.URL, sess.Creds.Username)

	// Resolve the workflow YAML to an absolute path so project
	// discovery can compare cwd vs ancestors safely. The string
	// the user typed may be relative; the registry stores
	// absolute paths.
	workflowAbs, err := filepath.Abs(workflowArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving workflow path: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(workflowAbs); err != nil {
		fmt.Fprintf(os.Stderr, "workflow not found: %s\n", workflowAbs)
		os.Exit(2)
	}

	projectID, projectRoot, err := resolveOrRegisterProject(ctx, sess, workflowAbs, *name, *asJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve project: %v\n", err)
		os.Exit(1)
	}
	templatePath, err := filepath.Rel(projectRoot, workflowAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workflow %s is outside project root %s: %v\n", workflowAbs, projectRoot, err)
		os.Exit(2)
	}

	logf(*asJSON, "▶ project %d at %s", projectID, projectRoot)
	logf(*asJSON, "▶ workflow %s", templatePath)

	runSeq, runID, err := createRun(ctx, sess.FC, projectID, templatePath, params, *branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create run: %v\n", err)
		os.Exit(1)
	}
	logf(*asJSON, "▶ run #%d created (id=%d)", runSeq, runID)

	res, err := sess.FC.ExecuteRun(ctx, service.ExecuteRunParams{
		ProjectID: int(projectID),
		RunID:     int(runID),
		MaxTasks:  *maxTasks,
		Parallel:  1,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "execute run: %v\n", err)
		os.Exit(1)
	}
	renderExecuteResult(os.Stdout, res, *asJSON)

	if res.StopReason == service.StopComputeFailed ||
		res.StopReason == service.StopComputeErrored ||
		res.StopReason == service.StopGitOperationFailed {
		os.Exit(1)
	}
}

// resolveOrRegisterProject finds the project that owns
// workflowAbs by walking the project registry's local_path
// entries. Returns (projectID, projectRoot). If no entry covers
// the workflow, registers a fresh project rooted at the
// workflow's nearest .git ancestor (or its containing directory
// if no .git is found).
//
// Naming intent vs. implementation: the spec's prose talks
// about "cwd's project" — in the common case (operator runs
// `enju go workflow.yaml` from inside the project) cwd and the
// workflow's directory coincide and the choice doesn't matter.
// When the operator passes an absolute path to a workflow under
// a different tree, this implementation prefers the workflow's
// ancestry over cwd: the workflow's repo is what the run will
// execute against, so registering THAT repo is the right
// anchor. Operators who don't want auto-registration can
// pre-register via `enju mcp` + enju_create_project first.
//
// asJSON routes the auto-register informational line through
// logf so JSON-mode consumers see only structured output.
func resolveOrRegisterProject(ctx context.Context, sess *cliSession, workflowAbs, nameOverride string, asJSON bool) (int64, string, error) {
	reg := sess.FC.ProjectRegistry()
	if reg == nil {
		return 0, "", fmt.Errorf("no project registry configured")
	}
	entries, err := reg.List()
	if err != nil {
		return 0, "", fmt.Errorf("read registry: %w", err)
	}
	if entry := pickContainingEntry(entries, workflowAbs); entry != nil {
		return entry.ID, entry.LocalPath, nil
	}

	root := projectRootCandidate(workflowAbs)
	projectName := nameOverride
	if projectName == "" {
		projectName = filepath.Base(root)
	}
	logf(asJSON, "▶ no registered project covers %s; registering %q at %s", workflowAbs, projectName, root)
	res, err := sess.FC.CreateProject(ctx, service.CreateProjectParams{
		Name: projectName,
		Path: root,
	})
	if err != nil {
		return 0, "", fmt.Errorf("create project: %w", err)
	}
	if res.InitWarning != "" {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", res.InitWarning)
	}
	return res.ProjectID, root, nil
}

// pickContainingEntry returns the registry entry whose LocalPath
// is workflowAbs or an ancestor directory, preferring the
// deepest match so nested project layouts resolve to the
// closest project. Returns nil when no entry contains the file.
func pickContainingEntry(entries []projectreg.Entry, workflowAbs string) *projectreg.Entry {
	var best *projectreg.Entry
	for i := range entries {
		root := entries[i].LocalPath
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, workflowAbs)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if best == nil || len(entries[i].LocalPath) > len(best.LocalPath) {
			best = &entries[i]
		}
	}
	return best
}

// projectRootCandidate is the directory we hand to CreateProject
// when no registry match exists. Walks up from the workflow's
// directory looking for a `.git`; falls back to the workflow's
// containing directory if none is found (CreateProject's smart-
// detect handles "no git yet" by initializing one).
func projectRootCandidate(workflowAbs string) string {
	dir := filepath.Dir(workflowAbs)
	cur := dir
	for {
		if st, err := os.Stat(filepath.Join(cur, ".git")); err == nil && st.IsDir() {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		cur = parent
	}
}

// createRun runs the path= flavor of enju_create_run from CLI:
// PrepareRunTemplate → POST → EnsureRunBranch → MaterializeRunFromData
// → TouchProject. Mirrors mcphandlers/run.go:handleCreateRun for
// the path= branch (the snapshot writeback uses the modern
// MaterializeRunFromData, not the older CommitRunTemplateSnapshot
// service helper).
//
// PARITY: this is the second site implementing the same path=
// create_run sequence. The first is mcphandlers/run.go's
// handleCreateRun. Any fix to one MUST be mirrored to the
// other until the shared bits are promoted into a service
// helper (e.g. FatClient.CreateRunFromPath that takes the
// snapshot-mode flag). Two-site duplication is deliberate —
// service.CreateRunFromTemplate still uses the older
// CommitRunTemplateSnapshot path and isn't reusable for path=
// mode yet. Tracked follow-up: unify both call sites.
//
// GAP (auto_bots): handleCreateRun added an auto_bots=true
// preflight (start every workflow bot, WaitForReady, MarkAutoRun
// post-POST, register the live.jsonl tailer for auto-stop on
// run completion). This CLI path does NOT yet honor it — `enju
// go` runs always behave as auto_bots=false. Mirroring the
// logic here is non-trivial (it owns Supervisor construction +
// bot lifecycle), so the cleaner path is the shared helper
// promotion: extract auto_bots preflight / hookup / rollback
// into a service or bots-package helper that BOTH this CLI
// path and handleCreateRun call. Until then, operators who
// want auto_bots use the MCP create_run tool, not `enju go`.
//
// Returns the run's per-project seq and the global run_id from
// the coord response. Surfaces ensure-branch / snapshot
// warnings to stderr as the MCP handler does.
func createRun(ctx context.Context, fc *service.FatClient, projectID int64, templatePath string, params map[string]interface{}, branch string) (int, int64, error) {
	authorName, authorEmail := fc.CommitAuthor(ctx)
	prep, err := fc.PrepareRunTemplate(ctx, projectID, templatePath, authorName, authorEmail)
	if err != nil {
		return 0, 0, err
	}

	body := map[string]interface{}{
		"yaml":              prep.YAMLContent,
		"username":          fc.Coord().Username(),
		"source_path":       templatePath,
		"source_commit_sha": prep.SourceCommit,
	}
	if len(params) > 0 {
		body["params"] = params
	}
	if branch != "" {
		body["branch"] = branch
	}

	data, err := fc.Coord().Post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs", projectID), body)
	if err != nil {
		return 0, 0, err
	}
	if msg := errorFromCoord(data); msg != "" {
		return 0, 0, fmt.Errorf("%s", msg)
	}

	if w := fc.EnsureRunBranch(ctx, projectID, data); w != "" {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
	}
	if w := fc.MaterializeRunFromData(ctx, projectID, data); w != "" {
		fmt.Fprintf(os.Stderr, "  ⚠ snapshot %s\n", w)
	}
	fc.TouchProject(projectID)

	seq, runID, idErr := runIdentityFromCreateResponse(data)
	if idErr != nil {
		return 0, 0, fmt.Errorf("decoding coord response: %w: %s", idErr, string(data))
	}
	if seq == 0 || runID == 0 {
		return 0, 0, fmt.Errorf("coord response missing seq/id: %s", string(data))
	}
	return seq, runID, nil
}

// runIdentityFromCreateResponse extracts (seq, run_id) from the
// /runs POST response. Coord returns JSON-number values; encoded
// through encoding/json those land as float64 in a generic map.
// Returns a non-nil error only when JSON parsing fails; missing
// seq/id fields produce zeros + nil error so the caller can
// distinguish "malformed wire" from "decoded but absent."
func runIdentityFromCreateResponse(data []byte) (int, int64, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, 0, err
	}
	seq, _ := m["seq"].(float64)
	id, _ := m["id"].(float64)
	return int(seq), int64(id), nil
}

func errorFromCoord(data []byte) string {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	if s, ok := m["error"].(string); ok {
		return s
	}
	return ""
}

// parseParamsArg parses the --params shorthand: a comma-separated
// list of k=v tokens. Whitespace around tokens is tolerated. The
// CLI keeps every value as a string; templates that need typed
// params (int/float) can still parse them server-side via
// substituteParamsInPlace's type coercion path.
func parseParamsArg(arg string) (map[string]interface{}, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, nil
	}
	out := map[string]interface{}{}
	for _, tok := range strings.Split(arg, ",") {
		t := strings.TrimSpace(tok)
		if t == "" {
			continue
		}
		eq := strings.IndexByte(t, '=')
		if eq < 0 {
			return nil, fmt.Errorf("expected k=v, got %q", t)
		}
		key := strings.TrimSpace(t[:eq])
		val := strings.TrimSpace(t[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("empty key in %q", t)
		}
		out[key] = val
	}
	return out, nil
}

// renderExecuteResult prints the per-task lines + the trailing
// stop-reason summary. JSON mode emits NDJSON: one record per
// task entry followed by a single summary record. Every record
// carries a `type` discriminator ("entry" or "summary") so a
// stream consumer can dispatch on a stable field rather than
// guessing from field presence.
func renderExecuteResult(w io.Writer, res *service.ExecuteRunResult, asJSON bool) {
	if asJSON {
		// COUPLING: this map mirrors service.ExecuteRunEntry's
		// JSON shape one field at a time so the "type"
		// discriminator can live alongside the entry fields.
		// When ExecuteRunEntry gains a field (RetryCount,
		// WorkerID, etc.), add it here too — otherwise the
		// new field silently disappears from `enju go --json`
		// output while still appearing in MCP responses.
		for _, e := range res.Entries {
			rec := map[string]any{
				"type":       "entry",
				"task_id":    e.TaskID,
				"status":     e.Status,
				"script":     e.Script,
				"elapsed_ms": e.ElapsedMS,
				"commit_sha": e.CommitSHA,
				"reason":     e.Reason,
				"artifacts":  e.Artifacts,
			}
			b, _ := json.Marshal(rec)
			fmt.Fprintln(w, string(b))
		}
		summary := map[string]any{
			"type":              "summary",
			"stop_reason":       res.StopReason,
			"blocker":           res.Blocker,
			"self_stuck_claims": res.SelfStuckClaims,
		}
		b, _ := json.Marshal(summary)
		fmt.Fprintln(w, string(b))
		return
	}
	for _, e := range res.Entries {
		switch e.Status {
		case "ok", "accepted":
			fmt.Fprintf(w, "  ✓ %s (%dms)\n", e.TaskID, e.ElapsedMS)
		case "skipped":
			fmt.Fprintf(w, "  · %s skipped\n", e.TaskID)
		default:
			fmt.Fprintf(w, "  ✗ %s — %s\n", e.TaskID, e.Reason)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "stopped: %s\n", res.StopReason)
	if res.Blocker != nil {
		fmt.Fprintf(w, "  next gate: %s (%s)\n", res.Blocker.TaskID, res.Blocker.Action)
	}
	if len(res.SelfStuckClaims) > 0 {
		fmt.Fprintln(w, "  self-held stuck claims:")
		for _, id := range res.SelfStuckClaims {
			fmt.Fprintf(w, "    %s  (release with `enju mcp` → enju_release_task)\n", id)
		}
	}
}

// logf is a tiny shim that suppresses informational stderr noise
// when --json is set so machine consumers see only the JSONL on
// stdout. Errors still print regardless of mode.
func logf(asJSON bool, format string, args ...interface{}) {
	if asJSON {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
