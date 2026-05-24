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
	"time"

	"github.com/enju-ai/enju/internal/bots"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
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
	runBranch := fs.String("run-branch", "", `The run's own branch — where this run's commits land before merging back per --sync. "auto" generates an isolated name (<slug>-N); empty commits on the base branch directly. Distinct from --base, which is the branch the run forks FROM.`)
	base := fs.String("base", "", `Fork this run from <branch> instead of the project default, reading the workflow from that branch's committed tree. "HEAD" (or ".") means the currently checked-out branch — "run the workflow from where I am." Distinct from --run-branch, which names the run's own branch. The workflow must be committed on the base branch.`)
	var paramsArg repeatableParams
	fs.Var(&paramsArg, "params", "k=v[,k=v...] template parameter values. list<string> uses pipes: k=a|b|c. A value beginning with [ or { is parsed as JSON (records/nested) — top-level commas inside it are not split. Repeatable: pass --params more than once and the sets merge (later flags win per key).")
	paramsFile := fs.String("params-file", "", "Path to a JSON object of typed params (the MCP-shaped params payload). Merged UNDER --params: inline keys win per key, even when the two forms differ (e.g. an inline string overrides a file list<record> of the same name). The clean route for list<record> and nested params the k=v grammar can't express.")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	asJSON := fs.Bool("json", false, "Stream per-task status as JSONL on stdout")
	maxTasks := fs.Int("max-tasks", 1000, "Cap on compute tasks drained in one go (safety net)")
	autoBots := fs.Bool("auto-agents", false, "Spin up every bot in the workflow's bots: section, wait for ready, hook auto-stop on run completion. Mirrors the MCP enju_create_run auto_agents flag.")
	dryRun := fs.Bool("dry-run", false, "Parse the workflow, substitute --params, render the resolved task DAG, and exit. No coord round-trip, no project mutation. Useful in CI and for previewing what `enju go` would create.")
	syncMode := fs.String("sync", "", `Sync mode at run completion: none | merge | push. Overrides the workflow YAML's sync: block. "merge" merges the run branch into base_branch locally; "push" also pushes base_branch to origin; "none" skips both. Defaults to the YAML setting, or "merge" if the YAML has no sync: block.`)
	fs.Parse(args)

	workflowArg, uerr := pickWorkflowArg(fs.Args())
	if uerr != nil {
		fmt.Fprintln(os.Stderr, uerr.Error())
		os.Exit(2)
	}

	if err := validateSyncFlag(*syncMode); err != nil {
		fmt.Fprintf(os.Stderr, "--sync: %v\n", err)
		os.Exit(2)
	}

	// --params is additive across repeated flags (M4): each
	// occurrence is parsed independently and merged, later flags
	// winning per key. The doc demonstrated this shape; the prior
	// single-string flag silently took only the last occurrence.
	var params map[string]interface{}
	for _, chunk := range paramsArg {
		p, perr := parseParamsArg(chunk)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "--params: %v\n", perr)
			os.Exit(2)
		}
		params = mergeParams(params, p)
	}
	if *paramsFile != "" {
		fileParams, ferr := loadParamsFile(*paramsFile)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "--params-file %s: %v\n", *paramsFile, ferr)
			os.Exit(2)
		}
		// File is the base; inline --params overrides per key.
		params = mergeParams(fileParams, params)
	}

	// --dry-run short-circuits before openCLISession. Doesn't
	// need credentials, doesn't need a registered project,
	// doesn't reach the coord. Pure YAML parse + render. The
	// workflow path doesn't have to be inside any project on
	// this machine.
	if *dryRun {
		os.Exit(runDryRun(workflowArg, params, *asJSON))
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

	// Validate the workflow BEFORE registering the project (M4).
	// Auto-registration used to run first, so a YAML that failed
	// to parse/validate still left a phantom project behind in
	// `enju status --all`. Parse from disk with the same two-pass
	// param coercion --dry-run uses; a failure here exits before
	// any project mutation.
	parsed, verr := parseWorkflowForGo(workflowAbs, params)
	if verr != nil {
		fmt.Fprintf(os.Stderr, "workflow %s: %v\n", workflowArg, verr)
		os.Exit(4)
	}

	// --auto-agents on a workflow that declares no agents (M1):
	// there's nothing to manage, so degrade to a plain run instead
	// of hard-failing. Detected from the validated parse above.
	autoAgents := *autoBots
	if autoAgents {
		if m, perr := bots.FromInlineNode(parsed.Run.Bots); perr != nil || m == nil || len(m.Bots) == 0 {
			fmt.Fprintln(os.Stderr, "ℹ --auto-agents: workflow declares no agents; running without agent management")
			autoAgents = false
		}
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

	runSeq, runID, err := createRun(ctx, sess, projectID, templatePath, params, *runBranch, *base, autoAgents, *syncMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create run: %v\n", err)
		os.Exit(1)
	}
	logf(*asJSON, "▶ run #%d created (id=%d)", runSeq, runID)

	if autoAgents {
		// Bot-driven runs cycle until the run reaches a
		// terminal state. ExecuteRun drains compute until a
		// citizen gate; the bot daemons resolve those gates;
		// new compute becomes ready; we re-enter ExecuteRun.
		// The loop exits when isRunTerminal reports
		// completed/failed/terminated, at which point the
		// supervisor's tailer has fired auto-stop on every
		// auto-managed bot.
		if exit := driveAutoBotsRun(ctx, sess, int(projectID), runSeq, *maxTasks, *asJSON); exit != 0 {
			os.Exit(exit)
		}
		return
	}

	// ExecuteRun resolves the run by per-project seq (every coord
	// run endpoint is /projects/{pid}/runs/{seq}). Passing the
	// global runID here was the long-standing bug that made every
	// `enju go` invocation die with "run P:ID not found".
	res, err := sess.FC.ExecuteRun(ctx, service.ExecuteRunParams{
		ProjectID: int(projectID),
		RunSeq:    runSeq,
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

// driveAutoBotsRun is the --auto-agents execution loop. Cycles
// ExecuteRun + terminal-state polling until the coord reports
// the run completed / failed / terminated. Returns the shell
// exit code (0 = success, 1 = compute or git failure).
//
// Why this exists: the supervisor's auto-stop tailer reads
// live.jsonl on a goroutine owned by THIS process. If the
// CLI exits before the run reaches a terminal state, the
// goroutine is reaped and no auto-stop event ever fires for
// the workflow's bots. The synchronous loop keeps the
// process alive long enough for the terminal event to arrive.
//
// Poll interval is conservative (2s) — long enough that idle
// runs don't hammer coord, short enough that humans don't
// perceive lag between bot work completing and the next
// compute round kicking off.
func driveAutoBotsRun(ctx context.Context, sess *cliSession, projectID, runSeq, maxTasks int, asJSON bool) int {
	const (
		pollInterval     = 2 * time.Second
		coordWarnEvery   = 10 // emit a warning every Nth consecutive failure
	)
	// lastStopReason de-duplicates the noisy "stopped: X" /
	// "next gate: Y" block when nothing changed between
	// polling iterations. The render still fires when entries
	// are non-empty (real work happened) or when the blocker
	// shifted (operator gets a fresh signal).
	var lastStopReason string
	var lastBlockerID string
	consecutiveCoordErrors := 0

	for {
		res, err := sess.FC.ExecuteRun(ctx, service.ExecuteRunParams{
			ProjectID: projectID,
			RunSeq:    runSeq,
			MaxTasks:  maxTasks,
			Parallel:  1,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "execute run: %v\n", err)
			return 1
		}
		if shouldRenderPoll(res, lastStopReason, lastBlockerID) {
			renderExecuteResult(os.Stdout, res, asJSON)
		}
		lastStopReason = res.StopReason
		if res.Blocker != nil {
			lastBlockerID = res.Blocker.TaskID
		} else {
			lastBlockerID = ""
		}
		if res.StopReason == service.StopComputeFailed ||
			res.StopReason == service.StopComputeErrored ||
			res.StopReason == service.StopGitOperationFailed {
			return 1
		}

		// Check terminal state via coord. Cheap (one GET); the
		// auto-managed bots are independently working through
		// citizen tasks between our compute drains.
		terminal, terr := sess.isRunTerminal(ctx, int64(projectID), int64(runSeq))
		if terr != nil {
			// Persistent coord outage (network partition, coord
			// crash) would otherwise spin forever silently. Warn
			// every N consecutive failures so the operator can
			// distinguish "bots working, coord healthy" from
			// "coord unreachable, only Ctrl-C exits."
			consecutiveCoordErrors++
			if consecutiveCoordErrors%coordWarnEvery == 0 {
				fmt.Fprintf(os.Stderr,
					"⚠ coord unreachable for %d consecutive polls (last: %v) — Ctrl-C to abort\n",
					consecutiveCoordErrors, terr)
			}
		} else {
			consecutiveCoordErrors = 0
			if terminal {
				// Both mechanisms are now wired: the live.jsonl
				// tailer (started by HookRunSeq) fires auto-stop
				// based on terminal events, and the CLI process
				// exit takes any subprocess bots down via
				// stdin-EOF. Operator-owned bots that rode along
				// as AlreadyRunning survive both paths.
				logf(asJSON, "▶ run terminal — auto-stop firing on any auto-managed bots")
				return 0
			}
		}

		// Wait a beat, then re-enter. ctx.Done lets the
		// operator Ctrl-C out cleanly.
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "interrupted; auto-managed bots may still be running — use `enju agent list` and `enju_agent_stop` if needed")
			return 1
		case <-time.After(pollInterval):
		}
	}
}

// shouldRenderPoll filters out no-op iterations where ExecuteRun
// found nothing new to do and the stop reason / blocker are
// unchanged from the prior poll. Long bot turns (5+ minutes on a
// review) would otherwise spam stdout with 150 identical "next
// gate:" lines.
//
// Render fires when ANY of:
//   - Entries non-empty (real work happened this iteration).
//   - Stop reason changed (e.g. citizen_task_ready → no_ready_compute
//     after the bot resolved the gate).
//   - Blocker identity changed (one citizen gate cleared,
//     another raised).
func shouldRenderPoll(res *service.ExecuteRunResult, lastStop, lastBlocker string) bool {
	if len(res.Entries) > 0 {
		return true
	}
	if res.StopReason != lastStop {
		return true
	}
	curBlocker := ""
	if res.Blocker != nil {
		curBlocker = res.Blocker.TaskID
	}
	return curBlocker != lastBlocker
}

// pickWorkflowArg implements Snakemake-style workflow discovery:
//
//   1 arg → use it verbatim (relative paths resolve against cwd).
//   0 args → look for ./enju.yaml in cwd. Use it if present.
//   0 args + no ./enju.yaml → usage error naming what was tried.
//   2+ args → usage error (single workflow per invocation).
//
// The convention matches Snakemake's "run me from a project
// directory" ergonomics and the spec's discovery table (item 3).
// Lives at the cmdGo boundary so dry-run, regular runs, and any
// future tier-1 verb that takes a workflow can share the same
// rule by calling it.
func pickWorkflowArg(args []string) (string, error) {
	switch len(args) {
	case 1:
		return args[0], nil
	case 0:
		const defaultName = "enju.yaml"
		if _, err := os.Stat(defaultName); err == nil {
			return defaultName, nil
		}
		return "", fmt.Errorf(
			"no workflow path supplied and ./%s not found.\n"+
				"  usage: enju go [--name X] [--run-branch X] [--params k=v,k=v] [--params-file f.json] [--dry-run] [--json] <workflow.yaml>\n"+
				"  (flags must precede the workflow path), or run from a directory that contains an %s file.",
			defaultName, defaultName)
	default:
		return "", fmt.Errorf("usage: enju go [--name X] [--run-branch X] [--params k=v,k=v] [--params-file f.json] [--dry-run] [--json] <workflow.yaml>  (flags must precede the path; got %d positional args, expected 0 or 1 — a flag placed AFTER the workflow path is the usual cause)", len(args))
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
	entry, err := reg.FindContaining(workflowAbs)
	if err != nil {
		return 0, "", fmt.Errorf("read registry: %w", err)
	}
	if entry != nil {
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
// PrepareRunTemplate → (optional auto_agents Preflight) → POST →
// EnsureRunBranch → MaterializeRunFromData → (optional HookRunSeq) →
// TouchProject. Mirrors mcphandlers/run.go:handleCreateRun for
// the path= branch (the snapshot writeback uses the modern
// MaterializeRunFromData, not the older CommitRunTemplateSnapshot
// service helper).
//
// PARITY: this is the second site implementing the same path=
// create_run sequence. The first is mcphandlers/run.go's
// handleCreateRun. The auto_agents state machine is shared via
// bots.AutoRunManager so both sites can't drift on bot-lifecycle
// fixes; the surrounding POST + snapshot + ensure-branch
// sequence is still duplicated (a future
// FatClient.CreateRunFromPath could unify it).
//
// Returns the run's per-project seq and the global run_id from
// the coord response. Surfaces ensure-branch / snapshot
// warnings to stderr as the MCP handler does.
func createRun(ctx context.Context, sess *cliSession, projectID int64, templatePath string, params map[string]interface{}, branch, base string, autoBots bool, syncMode string) (int, int64, error) {
	fc := sess.FC
	authorName, authorEmail := fc.CommitAuthor(ctx)
	prep, err := fc.PrepareRunTemplate(ctx, projectID, templatePath, base, authorName, authorEmail)
	if err != nil {
		return 0, 0, err
	}

	// Coerce string-typed CLI params to the types declared in
	// the workflow's params: block. The MCP path receives
	// numbers/bools natively via JSON decode; the CLI's
	// `--params k=v` syntax has no type tagging so every value
	// arrives as a string. Without this step, any param of
	// type int / bool / list<string> would fail the validator's
	// checkParamValueType. See params.go for the coercion table.
	if len(params) > 0 && prep != nil && prep.LoadedTemplate != nil {
		coerced, cerr := coerceCLIParams(params, prep.LoadedTemplate.Parsed.Run.Params)
		if cerr != nil {
			return 0, 0, cerr
		}
		params = coerced
	}

	// Warn when the workflow declares bots but the operator
	// didn't opt in to --auto-agents. Without this, `enju go
	// workflow.yaml` on a bots-using workflow stops at the first
	// citizen gate with "next gate: <task_id>" and an operator
	// may not realize the workflow's own bots could have
	// resolved it — they were just never started.
	if !autoBots && prep != nil && prep.LoadedTemplate != nil {
		if m, perr := bots.FromInlineNode(prep.LoadedTemplate.Parsed.Run.Bots); perr == nil && m != nil && len(m.Bots) > 0 {
			fmt.Fprintf(os.Stderr,
				"⚠ workflow declares %d agent(s); pass --auto-agents to start them automatically (or `enju agent run` per agent for manual control)\n",
				len(m.Bots))
		}
	}

	// auto_agents preflight (mirrors handleCreateRun). Same
	// AutoRunManager type so any bug fix in bot lifecycle
	// lands in both places automatically.
	var autoRunMgr *bots.AutoRunManager
	if autoBots {
		if prep == nil || prep.LoadedTemplate == nil {
			return 0, 0, fmt.Errorf("auto_agents: workflow prep is empty (internal error)")
		}
		manifest, perr := bots.FromInlineNode(prep.LoadedTemplate.Parsed.Run.Bots)
		if perr != nil {
			return 0, 0, fmt.Errorf("auto_agents: parsing agents: %w", perr)
		}
		if manifest == nil || len(manifest.Bots) == 0 {
			// No agents to manage — degrade to a plain run rather
			// than failing (M1). cmdGo normally downgrades autoBots
			// before reaching here; this guard is the defensive twin
			// so a direct createRun call behaves the same way.
			autoBots = false
		} else {
			sup, perr := sess.Supervisor()
			if perr != nil {
				return 0, 0, fmt.Errorf("auto_agents: supervisor init: %w", perr)
			}
			absWorkflow := filepath.Join(prep.Workflow.WorkDir(), prep.LoadedTemplate.Path)
			autoRunMgr = bots.NewAutoRunManager(sup, absWorkflow, fc.Coord().BaseURL(), projectID, bots.AutoRunReadyTimeout())
			if perr := autoRunMgr.Preflight(ctx, manifest); perr != nil {
				autoRunMgr.Rollback(ctx)
				return 0, 0, fmt.Errorf("auto_agents: %w", perr)
			}
		}
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
		// "auto" is a client-side convenience the coord refuses
		// raw — resolve it via the shared FatClient allocator
		// (same one the MCP create_run handler uses) so the coord
		// only ever sees a concrete name. Without this, `enju go
		// -branch auto` died at the coord (Finding F).
		if branch == "auto" {
			resolved, rerr := fc.ResolveAutoBranch(ctx, projectID, prep.YAMLContent, templatePath)
			if rerr != nil {
				return 0, 0, fmt.Errorf("resolving branch=auto: %w", rerr)
			}
			branch = resolved
		}
		body["branch"] = branch
	}
	if syncMode != "" {
		body["sync_mode_override"] = syncMode
	}

	data, err := fc.Coord().Post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs", projectID), body)
	if err != nil {
		// POST failed after preflight already spun up the
		// fleet. Roll back so a coord-side failure doesn't
		// leak bots that no run will ever reference. The
		// manager's Rollback only touches freshStarts —
		// operator-owned bots survive.
		if autoRunMgr != nil {
			autoRunMgr.Rollback(ctx)
		}
		return 0, 0, err
	}
	if msg := errorFromCoord(data); msg != "" {
		if autoRunMgr != nil {
			autoRunMgr.Rollback(ctx)
		}
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

	// Post-POST hook: mark each preflighted bot's pid file with
	// the new run's seq and start the live.jsonl tailer so
	// terminal events fire auto-stop. Mirrors handleCreateRun's
	// HookRunSeq call. Without this, auto-managed bots in CLI
	// runs lacked the tailer entirely — they'd only ever stop
	// via stdin-EOF when the CLI process exits, never via the
	// run-completed event. (Functionally fresh-started bots
	// still got cleaned up via parent-death, but operator bots
	// that came back AlreadyRunning lacked the run-seq ref and
	// the tailer was missing for cross-process inspection.)
	if autoRunMgr != nil {
		if prep == nil {
			fmt.Fprintf(os.Stderr, "⚠ auto_agents: prep is nil, tailer will not start (internal error)\n")
		} else if prep.Workflow == nil {
			fmt.Fprintf(os.Stderr, "⚠ auto_agents: prep.Workflow is nil, tailer will not start (internal error)\n")
		} else {
			unhooked := autoRunMgr.HookRunSeq(ctx, int64(seq), prep.Workflow.WorkDir())
			if len(unhooked) > 0 {
				fmt.Fprintf(os.Stderr,
					"⚠ auto_agents: %d of %d bot(s) lost their auto-stop hook (%s) — likely crashed between WaitForReady and pid-file write. They will NOT auto-stop on run completion; use `enju agent stop` manually if they're still running.\n",
					len(unhooked), len(autoRunMgr.AutoBotNames()), strings.Join(unhooked, ", "))
			}
		}
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

// parseWorkflowForGo parses + validates the workflow YAML at path
// using the same two-pass param-coercion the --dry-run path uses,
// so `enju go` catches a malformed workflow BEFORE registering a
// project (M4). Returns the parsed run on success. The error is
// already user-readable (it's the validator's own message); callers
// prefix it with the workflow path.
func parseWorkflowForGo(path string, params map[string]interface{}) (*enjuYaml.ParsedRun, error) {
	data, err := enjuYaml.FlattenFile(path)
	if err != nil {
		return nil, err
	}
	// Two-pass when params present: parse bare to learn declared
	// types, coerce string CLI values, then parse for real.
	if len(params) > 0 {
		bare, perr := enjuYaml.Parse(data)
		if perr != nil {
			return nil, perr
		}
		coerced, cerr := coerceCLIParams(params, bare.Run.Params)
		if cerr != nil {
			return nil, cerr
		}
		params = coerced
	}
	return enjuYaml.ParseWithParams(data, params)
}

// validateSyncFlag returns an error when v is not one of the valid
// sync modes. Empty string (flag not set) is accepted. Extracted so
// tests can cover the validation logic independent of cmdGo's
// flag.Parse / os.Exit plumbing.
func validateSyncFlag(v string) error {
	switch v {
	case "", "none", "merge", "push":
		return nil
	default:
		return fmt.Errorf("invalid value %q (must be none, merge, or push)", v)
	}
}

// repeatableParams is a flag.Value that accumulates every --params
// occurrence so the flag is additive (M4): `--params a=1 --params
// b=2` collects ["a=1", "b=2"], which cmdGo parses + merges. Pre-fix
// --params was a plain string flag and the last occurrence silently
// won, contradicting the doc that demonstrated the additive form.
type repeatableParams []string

func (r *repeatableParams) String() string { return strings.Join(*r, " ") }

func (r *repeatableParams) Set(v string) error {
	*r = append(*r, v)
	return nil
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
	for _, tok := range splitTopLevelCommas(arg) {
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
		v, isJSON, jerr := decodeJSONValue(val)
		if jerr != nil {
			return nil, fmt.Errorf("param %q looks like JSON but did not parse: %w", key, jerr)
		}
		if isJSON {
			out[key] = v
		} else {
			out[key] = val
		}
	}
	return out, nil
}

// splitTopLevelCommas splits on commas that are NOT inside a JSON
// array/object or a double-quoted string. Plain "k=a,k2=b" splits
// exactly like strings.Split; an embedded record value such as
// entries=[{"slug":"a","label":"A,B"}] keeps its own commas intact
// so the JSON survives to decodeJSONValue. ASCII-only structural
// runes, so byte-index slicing over the range loop is safe.
func splitTopLevelCommas(s string) []string {
	var parts []string
	depth, start := 0, 0
	inStr, esc := false, false
	for i, r := range s {
		switch {
		case esc:
			esc = false
		case inStr && r == '\\':
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
			// structural runes inside a string literal don't count
		case r == '[' || r == '{':
			depth++
		case r == ']' || r == '}':
			depth--
		case r == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// decodeJSONValue parses values that begin with '[' or '{' as
// JSON (records, lists of records, nested objects). Returns
// (decoded, true, nil) on success; (nil, false, nil) when the
// value is not JSON-shaped — the caller keeps the raw string and
// the declared-type coercer handles string/int/bool/list<string>;
// (nil, false, err) when it looked like JSON but is malformed.
// Failing loudly on broken JSON beats silently shipping it as a
// string and letting the validator emit a type error three layers
// down with no hint that the JSON itself was the problem.
func decodeJSONValue(val string) (interface{}, bool, error) {
	if val == "" || (val[0] != '[' && val[0] != '{') {
		return nil, false, nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(val), &v); err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// loadParamsFile reads a JSON object of typed params. Values
// arrive already-typed (JSON numbers → float64, arrays → slices,
// records → maps) exactly as the MCP transport produces them, so
// they bypass the CLI's string→type coercer and flow straight to
// the validator. This is what makes list<record> expressible
// from the CLI at all — the k=v string grammar cannot.
func loadParamsFile(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil, fmt.Errorf("file is empty")
	}
	if s[0] != '{' {
		return nil, fmt.Errorf("expected a JSON object at the top level (params are name→value); got a leading %q", s[:1])
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return m, nil
}

// mergeParams overlays hi onto lo (hi wins per key); either may
// be nil/empty. Returns nil only when both are empty so the
// downstream "len(params) > 0" guards behave unchanged.
func mergeParams(lo, hi map[string]interface{}) map[string]interface{} {
	if len(lo) == 0 && len(hi) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(lo)+len(hi))
	for k, v := range lo {
		out[k] = v
	}
	for k, v := range hi {
		out[k] = v
	}
	return out
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
		switch service.ClassifyEntryStatus(e.Status) {
		case service.EntryClassSuccess:
			fmt.Fprintf(w, "  ✓ %s (%dms)\n", e.TaskID, e.ElapsedMS)
		case service.EntryClassSkipped:
			fmt.Fprintf(w, "  · %s skipped\n", e.TaskID)
		case service.EntryClassPending:
			fmt.Fprintf(w, "  … %s started (async)\n", e.TaskID)
		default: // Failed, GitFailed, Error, Unknown
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
