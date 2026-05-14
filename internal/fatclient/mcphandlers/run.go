package mcphandlers

// Run-lifecycle handlers. enju_create_run instantiates a DAG
// (from inline YAML, a saved template path, or either + a
// params map); enju_list_runs / enju_run_status surface state;
// enju_export_* persist run artifacts as git commits;
// enju_export_run assembles every task result in DAG order
// into one markdown document. Workspace-heavy bodies live in
// internal/fatclient/service/run_ops.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/common/format"
	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/common/types"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

// autoBotsReadyTimeout is how long create_run waits for each
// bot to reach PhaseReady. Defaults to 30s; tunable via env
// for first-touch demos with cold claude-CLI subprocesses that
// need longer warmup, and for tests that want to fail fast
// without waiting for the production timeout.
func autoBotsReadyTimeout() time.Duration {
	if v := os.Getenv("ENJU_AUTO_BOTS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// rollbackAutoStarts stops every bot in freshStarts. Used by
// both the preflight-failure and post-POST-failure paths in
// handleCreateRun.
//
// CRITICAL contract — pre-REV.1 bug: callers MUST pass ONLY the
// bots this auto_bots call freshly spawned (StartedFresh outcome
// from Supervisor.Start), never the wider autoBotNames list
// (which includes AlreadyRunning operator-started bots that rode
// along). The helper itself can't enforce this — it just stops
// whatever it's given — so a future refactor that mistakenly
// passes the wide list reintroduces the bug silently. If you're
// changing this code: the variable named autoFreshStarts is the
// one that goes here. If you're looking at the variable named
// autoBotNames and thinking "those are the bots, I'll stop
// them," you've found the REV.1 bug — back away.
func rollbackAutoStarts(ctx context.Context, sup *bots.Supervisor, freshStarts []string) {
	for _, name := range freshStarts {
		if _, err := sup.Stop(ctx, name); err != nil {
			slog.Default().Warn("auto_bots rollback: stop failed", "bot", name, "error", err)
		}
	}
}

func (c *apiClient) handleListRuns(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var data []byte
	var err error
	if pid := req.GetInt("project_id", 0); pid != 0 {
		data, err = c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs", pid))
	} else {
		data, err = c.get(ctx, "/api/v1/runs")
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(format.RunList(data)), nil
}

func (c *apiClient) handleRunStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}

	base := fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID)
	run, err := c.get(ctx, base)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Opportunistic reconcile: any async compute tasks that
	// committed + pushed since the last scan on this run's
	// branch get picked up here, so the status we're about to
	// render reflects the freshest coordinator state. Best-
	// effort — a fetch or reconcile failure just means stale
	// status this cycle, not a tool error.
	c.fc.ReconcileRunBranch(ctx, int64(projectID), run)

	tasks, err := c.get(ctx, base+"/tasks")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Inject project name into the run data for the header.
	_, projName, _ := c.fc.FetchProjectMetaFull(ctx, int64(projectID))
	if projName != "" {
		var runMap map[string]interface{}
		if json.Unmarshal(run, &runMap) == nil {
			runMap["_project_name"] = projName
			run, _ = json.Marshal(runMap)
		}
	}

	// Format switch: default = textual summary + tree; mermaid =
	// flowchart TD syntax for pasting into mermaid.live / README
	// / preprint. Invalid values fall back to default silently so
	// older clients that don't know the param never error.
	switch req.GetString("format", "default") {
	case "mermaid":
		return mcp.NewToolResultText(format.RunStatusMermaid(run, tasks)), nil
	default:
		return mcp.NewToolResultText(format.RunStatus(run, tasks, c.username())), nil
	}
}

// handlePauseRun moves a run into the `paused` state.
func (c *apiClient) handlePauseRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/pause", projectID, runID), map[string]string{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.PauseRunResult(data)), nil
}

// handleTerminateRun is the human-pulled-the-plug operation.
// Thin pass-through to POST
// /projects/{id}/runs/{seq}/terminate. The schema description
// covers what terminate means and its limitations.
func (c *apiClient) handleTerminateRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	body := map[string]string{}
	if reason := req.GetString("reason", ""); reason != "" {
		body["reason"] = reason
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/terminate", projectID, runID), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.TerminateRunResult(data)), nil
}

// handleResumeRun lifts a paused run back to active or idle.
// Lands on idle when no ready work exists, active when ready
// tasks are present; the response carries the resolved state.
func (c *apiClient) handleResumeRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/resume", projectID, runID), map[string]string{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.ResumeRunResult(data)), nil
}

// handleShowEvents queries the project event log and returns
// JSONL — one event per line, newest-first. Filters: run_id,
// citizen, event_types (comma-separated), since (RFC3339),
// limit (default 100, max 1000).
func (c *apiClient) handleShowEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	q := url.Values{}
	if rs := req.GetInt("run_id", 0); rs != 0 {
		q.Set("run_seq", fmt.Sprintf("%d", rs))
	}
	if u := req.GetString("citizen", ""); u != "" {
		q.Set("citizen", u)
	}
	if et := req.GetString("event_types", ""); et != "" {
		q.Set("event_types", et)
	}
	if since := req.GetString("since", ""); since != "" {
		q.Set("since", since)
	}
	if limit := req.GetInt("limit", 0); limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}

	endpoint := fmt.Sprintf("/api/v1/projects/%d/events", projectID)
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	data, err := c.get(ctx, endpoint)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// The events endpoint normally returns a JSON array. When
	// the server hits an error path (e.g. run_seq names a run
	// that doesn't exist) it returns a single error object
	// like {"error": "run not found"} with a 4xx status. Try
	// to decode that shape first so the caller gets a usable
	// message instead of the raw "cannot unmarshal object
	// into []map" decoder error.
	if errMsg := errorFromResponse(data); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	var events []map[string]interface{}
	if err := json.Unmarshal(data, &events); err != nil {
		return mcp.NewToolResultError("decoding events: " + err.Error()), nil
	}
	return mcp.NewToolResultText(format.EventListJSONL(events)), nil
}

// handleRecentEvents is the assistant-side counterpart to
// handleShowEvents — same underlying endpoint, smaller default
// limit, human-readable output (one line per event).
//
// for_me filtering happens client-side (post-fetch): we filter
// events where event.citizen == self OR event.assign_to == self.
// Limit applies pre-filter, so the result with for_me=true may
// be smaller than the requested limit. Coord-side filtering
// would be more efficient but requires a json_extract on the
// metadata column; deferred until projects with high event
// volume actually need it.
func (c *apiClient) handleRecentEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	limit := req.GetInt("limit", 20)
	if limit > 100 {
		limit = 100
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if since := req.GetString("since", ""); since != "" {
		q.Set("since", since)
	}
	if sinceSeq := req.GetInt("since_seq", 0); sinceSeq > 0 {
		q.Set("since_seq", fmt.Sprintf("%d", sinceSeq))
	}
	forMe := req.GetBool("for_me", false)

	endpoint := fmt.Sprintf("/api/v1/projects/%d/events?%s", projectID, q.Encode())
	data, err := c.get(ctx, endpoint)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if errMsg := errorFromResponse(data); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	var events []map[string]interface{}
	if err := json.Unmarshal(data, &events); err != nil {
		return mcp.NewToolResultError("decoding events: " + err.Error()), nil
	}
	if forMe {
		events = filterEventsForCitizen(events, c.username())
	}
	return mcp.NewToolResultText(format.EventListRecent(events)), nil
}

// filterEventsForCitizen keeps events where the calling citizen
// is named in event.citizen (the actor) or event.assign_to (the
// task assignee that the coord hoists out of metadata at
// emit-time). Both are top-level string fields on the wire.
//
// Limitations are documented on the for_me parameter in the
// schema: events on tasks the citizen submitted but didn't claim
// (branch_merged after approval, task_completed where the closer
// is the reviewer), self-filed issues without explicit
// assignment, and project-wide events without a citizen
// (run_completed) are NOT surfaced. The honest "events about
// entities I authored" join is a future refinement.
func filterEventsForCitizen(events []map[string]interface{}, username string) []map[string]interface{} {
	if username == "" {
		return events
	}
	out := events[:0]
	for _, e := range events {
		if c, _ := e["citizen"].(string); c == username {
			out = append(out, e)
			continue
		}
		if a, _ := e["assign_to"].(string); a == username {
			out = append(out, e)
		}
	}
	return out
}

// handleRequestClarification is the bot-asks-human idiom — a
// thin wrapper over /spawn with sensible defaults locked in:
// action=answer, citizens=1, trigger=bot, single human assignee.
func (c *apiClient) handleRequestClarification(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	taskDefID, err := req.RequireString("task_def_id")
	if err != nil {
		return mcp.NewToolResultError("task_def_id is required"), nil
	}
	prompt, err := req.RequireString("prompt")
	if err != nil {
		return mcp.NewToolResultError("prompt is required"), nil
	}
	assignTo, err := req.RequireString("assign_to")
	if err != nil {
		return mcp.NewToolResultError("assign_to is required"), nil
	}

	// Validate assign_to is a member of the project. The
	// underlying spawn endpoint accepts any string and creates
	// the task; an unmembered or typo'd assignee produces an
	// unclaimable task that the bot waits on indefinitely. The
	// bot-asks-human idiom is exactly the case where this
	// "silent unclaimable" failure mode hurts most.
	membersData, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/members", projectID))
	if err != nil {
		return mcp.NewToolResultError("validating assign_to: " + err.Error()), nil
	}
	if errMsg := errorFromResponse(membersData); errMsg != "" {
		return mcp.NewToolResultError("validating assign_to: " + errMsg), nil
	}
	var members []map[string]interface{}
	if err := json.Unmarshal(membersData, &members); err == nil {
		matched := false
		usernames := make([]string, 0, len(members))
		for _, m := range members {
			u, _ := m["username"].(string)
			if u == "" {
				continue
			}
			usernames = append(usernames, u)
			if u == assignTo {
				matched = true
			}
		}
		if !matched && len(usernames) > 0 {
			return mcp.NewToolResultError(fmt.Sprintf(
				"@%s is not a member of project %d. Members: %s. Add a citizen with enju_add_project_member, or pick one from the list.",
				assignTo, projectID, strings.Join(usernames, ", "),
			)), nil
		}
		// Empty members → legacy "open project" mode (zero-member
		// gating bucket). Skip validation; spawn will accept
		// anyone, matching pre-Phase-3 behavior.
	}

	// Trigger reflects who asked — derived from the calling
	// citizen's kind. Bots calling this idiom emit trigger=bot
	// (the common case, audit-clear "bot needed clarification").
	// Humans calling are valid too (a reviewer pinging the
	// author for context); we don't want their spawn event
	// mislabeled.
	trigger := string(types.CitizenKindHuman)
	if c.citizenKind(ctx) == string(types.CitizenKindBot) {
		trigger = string(types.CitizenKindBot)
	}

	body := map[string]interface{}{
		"task_def_id":    taskDefID,
		"action":         "answer",
		"prompt":         prompt,
		"assign_to":      []string{assignTo},
		"citizens":       1,
		"trigger":        trigger,
		"parent_task_id": req.GetString("parent_task_id", ""),
	}

	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/spawn", projectID, runID), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	taskID, _ := resp["task_id"].(string)
	out := fmt.Sprintf(
		"✓ Clarification requested from @%s — task %s\n"+
			"  Question: %s\n"+
			"  Watch for task_completed on this task to know when answered.",
		assignTo, taskID, prompt,
	)
	return mcp.NewToolResultText(out), nil
}

// handleSpawnTask creates a new task in an in-flight run.
func (c *apiClient) handleSpawnTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	taskDefID, err := req.RequireString("task_def_id")
	if err != nil {
		return mcp.NewToolResultError("task_def_id is required"), nil
	}
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError("action is required"), nil
	}

	body := map[string]interface{}{
		"task_def_id":    taskDefID,
		"action":         action,
		"prompt":         req.GetString("prompt", ""),
		"user_prompt":    req.GetString("user_prompt", ""),
		"parent_task_id": req.GetString("parent_task_id", ""),
		"trigger":        req.GetString("trigger", "human"),
		"require_role":   req.GetString("require_role", ""),
		"result_type":    req.GetString("result_type", ""),
		"citizens":       req.GetInt("citizens", 1),
	}
	if dep := req.GetString("depends_on", ""); dep != "" {
		body["depends_on"] = strings.Split(dep, ",")
	}
	if as := req.GetString("assign_to", ""); as != "" {
		body["assign_to"] = strings.Split(as, ",")
	}

	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/spawn", projectID, runID), body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	taskID, _ := resp["task_id"].(string)
	out := fmt.Sprintf("✓ Spawned %s", taskID)
	if budget, ok := resp["cycle_budget"].(map[string]interface{}); ok {
		used, _ := budget["used"].(float64)
		max, _ := budget["max"].(float64)
		out += fmt.Sprintf(" (cycle_budget: %d/%d)", int(used), int(max))
	}
	return mcp.NewToolResultText(out), nil
}

// handleSetCycleBudget bumps the per-run spawn cap.
func (c *apiClient) handleSetCycleBudget(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	max, err := req.RequireInt("max")
	if err != nil {
		return mcp.NewToolResultError("max is required"), nil
	}
	data, err := c.post(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/cycle_budget", projectID, runID), map[string]int{"max": max})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decoding: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Cycle budget set to %d for run %d:%d", max, projectID, runID)), nil
}

// runBranchFromData and runSlugFromData are local forwarders so
// existing callers + tests in the mcphandlers package keep
// reading these as package-level functions. The actual
// implementations live on the service side.
func runBranchFromData(runData []byte) string { return service.RunBranchFromData(runData) }
func runSlugFromData(runData []byte) string   { return service.RunSlugFromData(runData) }

// PARITY: this handler's path= branch (PrepareRunTemplate →
// POST → EnsureRunBranch → MaterializeRunFromData) is mirrored
// in cmd/enju/go.go:createRun, which the CLI's `enju go` uses.
// Any fix to the sequence below MUST be mirrored there until
// the shared bits are promoted into a service helper (e.g.
// FatClient.CreateRunFromPath). The existing
// service.CreateRunFromTemplate still uses the older
// CommitRunTemplateSnapshot path and isn't reusable for the
// path= flow yet.
func (c *apiClient) handleCreateRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required — create a project first with enju_create_project"), nil
	}

	// Three input shapes —
	//   1. yaml (inline definition, no params)
	//   2. path (workflow YAML file in the repo — anywhere)
	//   3. yaml + params (inline definition with a declared params: block)
	//
	// Exactly one of (yaml, path) must be set. Params are optional in
	// all cases; if set, the coordinator calls ParseWithParams and
	// substitutes before validating.
	yamlContent := req.GetString("yaml", "")
	templatePath := req.GetString("path", "")
	params := req.GetArguments()["params"]
	var paramMap map[string]interface{}
	if params != nil {
		if m, ok := params.(map[string]interface{}); ok {
			paramMap = m
		} else {
			return mcp.NewToolResultError("params must be an object mapping parameter names to values"), nil
		}
	}

	if yamlContent == "" && templatePath == "" {
		return mcp.NewToolResultError("either 'yaml' (inline definition) or 'path' (workflow YAML anywhere in the project repo, e.g. 'enju.yaml' or 'workflows/scan-deps/enju.yaml') is required"), nil
	}
	if yamlContent != "" && templatePath != "" {
		return mcp.NewToolResultError("'yaml' and 'path' are mutually exclusive — pass one or the other"), nil
	}

	// Template-mode prep: open project, pull, load bundle, pin
	// to default branch. Returns the YAML body to POST and the
	// state needed for the post-create snapshot commit.
	var prep *service.RunTemplatePrep
	var sourceCommitSHA string
	if templatePath != "" {
		authorName, authorEmail := c.commitAuthor(ctx)
		p, err := c.fc.PrepareRunTemplate(ctx, int64(projectID), templatePath, authorName, authorEmail)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// Pre-validate the inline bots block at create_run time.
		// bots.FromInlineNode runs the bot-roster schema check
		// (name, model, handler enum, replicas cap, credentials
		// path shape) so malformed inline declarations surface
		// at create_run rather than at first daemon start.
		if p.LoadedTemplate != nil && p.LoadedTemplate.Parsed != nil {
			if _, berr := bots.FromInlineNode(p.LoadedTemplate.Parsed.Run.Bots); berr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("template %s: %v", templatePath, berr)), nil
			}
		}
		prep = p
		yamlContent = prep.YAMLContent
		sourceCommitSHA = prep.SourceCommit
	}

	body := map[string]interface{}{
		"yaml":     yamlContent,
		"username": c.username(),
	}
	if paramMap != nil {
		body["params"] = paramMap
	}
	if templatePath != "" {
		body["source_path"] = templatePath
		if sourceCommitSHA != "" {
			body["source_commit_sha"] = sourceCommitSHA
		}
	}
	if branch := req.GetString("branch", ""); branch != "" {
		// "auto" is a fat-client-side convenience. Coord owns
		// state/events/DAG, not git plumbing — so when the
		// caller asks for "auto", we resolve to a concrete name
		// here by consulting the project's bare repo. Coord
		// then sees an explicit branch name and the picker
		// can't drift from on-disk reality (e.g., post-DB-wipe
		// when on-disk refs survive but DB is empty).
		if branch == "auto" {
			resolved, err := c.resolveAutoBranch(ctx, int64(projectID), yamlContent, templatePath)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("resolving branch=auto: %v", err)), nil
			}
			branch = resolved
		}
		body["branch"] = branch
	}

	// auto_bots preflight (NDA.3). Spin up every bot declared
	// in the workflow's inline bots: section before the run is
	// created, and block until each reports PhaseReady. On
	// partial failure, roll back the fresh starts (leave
	// already-running bots alone — they may be doing other work)
	// and return an error rather than creating a half-served run.
	autoBots := req.GetBool("auto_bots", false)
	// autoBotNames lists every bot that completed preflight
	// (used for MarkAutoRun after the run is created so terminal
	// events decrement them). autoFreshStarts is the strict
	// subset that THIS call freshly spawned — only these are
	// safe to Stop on rollback; bots that came back as
	// AlreadyRunning are operator-owned (manual wins) and must
	// be left alone even when the surrounding run fails.
	var autoBotNames, autoFreshStarts []string
	if autoBots {
		if templatePath == "" {
			return mcp.NewToolResultError("auto_bots=true requires path= mode — inline yaml= has no on-disk workflow file for the bot daemons to read"), nil
		}
		if prep == nil || prep.LoadedTemplate == nil {
			return mcp.NewToolResultError("auto_bots=true: workflow prep is empty (internal error — should not happen with path= set)"), nil
		}
		manifest, perr := bots.FromInlineNode(prep.LoadedTemplate.Parsed.Run.Bots)
		if perr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("auto_bots: parsing bots: %v", perr)), nil
		}
		if manifest == nil || len(manifest.Bots) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("auto_bots=true but workflow at %s declares no bots in its inline bots: section", templatePath)), nil
		}
		sup, perr := c.botSupervisor()
		if perr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("auto_bots: supervisor init: %v", perr)), nil
		}
		absWorkflow := filepath.Join(prep.Workflow.WorkDir(), prep.LoadedTemplate.Path)
		coordURL := c.fc.Coord().BaseURL()
		readyTimeout := autoBotsReadyTimeout()

		preflight := func() error {
			for _, b := range manifest.Bots {
				var allow []string
				if b.MCPTools != nil {
					allow = b.MCPTools.Allow
				}
				_, outcome, serr := sup.Start(ctx, bots.StartParams{
					BotName:      b.Name,
					WorkflowPath: absWorkflow,
					Coordinator:  coordURL,
					ProjectID:    int64(projectID),
					AllowTools:   allow,
					StartedBy:    "auto_run",
				})
				if serr != nil {
					return fmt.Errorf("starting bot %q: %w", b.Name, serr)
				}
				if outcome == bots.StartedFresh {
					autoFreshStarts = append(autoFreshStarts, b.Name)
				}
				if rerr := sup.WaitForReady(ctx, b.Name, readyTimeout); rerr != nil {
					return fmt.Errorf("bot %q: %w (check %s for daemon output)", b.Name, rerr, sup.LogPathFor(b.Name))
				}
				autoBotNames = append(autoBotNames, b.Name)
			}
			return nil
		}
		if perr := preflight(); perr != nil {
			// REV.1 invariant: pass autoFreshStarts, NOT
			// autoBotNames. autoBotNames includes AlreadyRunning
			// (operator-owned) bots that came back from Start
			// idempotently; stopping them on a run-creation
			// failure would kill manual work the operator was
			// using for other things.
			rollbackAutoStarts(ctx, sup, autoFreshStarts)
			return mcp.NewToolResultError("auto_bots: " + perr.Error()), nil
		}
	}

	apiPath := fmt.Sprintf("/api/v1/projects/%d/runs", projectID)
	data, err := c.post(ctx, apiPath, body)
	if err != nil {
		// POST failed AFTER the auto_bots preflight already
		// spun up the fleet. Roll back so a coord-side failure
		// doesn't leak running bots that no run will ever
		// reference — but only stop the bots THIS call started.
		//
		// REV.1 invariant: pass autoFreshStarts, NOT
		// autoBotNames. Pre-fix this site used autoBotNames,
		// killing operator-owned bots that came back from
		// Start as AlreadyRunning. autoFreshStarts is the
		// strict subset of "spawned by THIS call" — any
		// rollback that calls Stop must source from it.
		if autoBots {
			if sup, _ := c.botSupervisor(); sup != nil {
				rollbackAutoStarts(ctx, sup, autoFreshStarts)
			}
		}
		return mcp.NewToolResultError(err.Error()), nil
	}

	// auto_bots: now that the coord has assigned a run seq,
	// record it on each bot's pid file so the live.jsonl
	// tailer can decrement when the run finishes. Also
	// register the project's event log with the supervisor
	// so the tailer is running by the time terminal events
	// arrive — idempotent across concurrent auto_bots runs
	// in the same project.
	//
	// Bots whose MarkAutoRun fails (typically: the daemon
	// crashed between WaitForReady and here, reaper removed
	// the pid file) get collected into autoBotsUnhooked so
	// the operator-visible result text can flag them.
	// Without that signal, the run proceeds silently and
	// the operator only notices the leak when they wonder
	// why a bot is still alive after the run completed.
	var autoBotsUnhooked []string
	if autoBots && len(autoBotNames) > 0 {
		var resp map[string]interface{}
		if jerr := json.Unmarshal(data, &resp); jerr == nil {
			if seq, ok := resp["seq"].(float64); ok {
				runSeq := int64(seq)
				sup, _ := c.botSupervisor()
				if sup != nil && prep != nil && prep.Workflow != nil {
					for _, name := range autoBotNames {
						if merr := sup.MarkAutoRun(name, runSeq); merr != nil {
							slog.Default().Warn("auto_bots: MarkAutoRun failed", "bot", name, "run_seq", runSeq, "error", merr)
							autoBotsUnhooked = append(autoBotsUnhooked, name)
						}
					}
					sup.WatchProjectEvents(ctx, prep.Workflow.WorkDir(), int64(projectID))
				}
			}
		}
	}

	// Materialize the run branch in the local workspace + on
	// origin BEFORE the snapshot commit (which lands on it) and
	// before any claim/submit verb tries to fork from it.
	// Idempotent: existing run branches are no-op'd. Errors are
	// non-fatal — the run exists on the coord side; surface as
	// a warning so the operator knows the workspace state isn't
	// fully synced.
	ensureBranchWarning := c.fc.EnsureRunBranch(ctx, int64(projectID), data)

	// After the coordinator assigns a run seq, freeze the run's
	// YAML to enju/runs/{seq}-{slug}/template-snapshot/. Template
	// runs snapshot the full bundle (scripts + data + manifest);
	// inline runs snapshot just the YAML body. Either way the
	// fatclient resolves per-task execution-policy fields
	// (container image, container runtime) from this on-disk copy
	// at execute time. Errors are non-fatal — the run exists on
	// the coordinator side; failure surfaces as a warning.
	var snapshotWarning string
	authorName, authorEmail := c.commitAuthor(ctx)
	if prep != nil {
		// path= mode: workflow YAML lives at its authored path in
		// the base SHA. Materialize the run branch tree directly
		// — no template-snapshot/ commit, no nested duplication.
		// Per the per-run-snapshot redesign Phase 8.h.6.b direction.
		snapshotWarning = c.fc.MaterializeRunFromData(ctx, int64(projectID), data)
	} else {
		// Inline-YAML mode: YAML isn't in the repo anywhere; the
		// legacy template-snapshot/ commit puts it on the run
		// branch so execute can read it.
		snapshotWarning = c.fc.CommitRunInlineSnapshot(ctx, int64(projectID), yamlContent, data, authorName, authorEmail)
	}

	c.fc.TouchProject(int64(projectID))

	text := format.CreateRun(data)
	if ensureBranchWarning != "" {
		text += fmt.Sprintf("\n⚠ %s\n", ensureBranchWarning)
	}
	if snapshotWarning != "" {
		text += fmt.Sprintf("\n⚠ Snapshot %s\n", snapshotWarning)
	}
	if len(autoBotsUnhooked) > 0 {
		text += fmt.Sprintf("\n⚠ auto_bots: %d of %d bot(s) lost their auto-stop hook (%s) — likely crashed between WaitForReady and pid-file write. They will NOT auto-stop on run completion; use enju_bot_stop manually if they're still running.\n",
			len(autoBotsUnhooked), len(autoBotNames), strings.Join(autoBotsUnhooked, ", "))
	}
	return mcp.NewToolResultText(text), nil
}

// resolveAutoBranch picks a concrete branch name when the caller
// passes branch="auto". Walks <slug>-1, <slug>-2, ... and returns
// the first name absent from the project's bare repo. The slug
// comes from the parsed YAML's name field via the same slugger
// the coord uses for runs/{seq}-{slug}/, so branch name and
// run-dir name stay in lockstep.
//
// Coord ownership boundary: the coord intentionally doesn't do
// git operations. It owns DAG + state + events; the fat-client
// owns git. "auto" is a fat-client-side convenience, resolved
// here against the bare repo (the source of truth for branch
// existence). Coord then receives the concrete name and trusts
// the client.
//
// Returns an error if the bare repo is unreachable, the YAML
// can't be parsed, or we exhaust the 10_000-name search.
func (c *apiClient) resolveAutoBranch(ctx context.Context, projectID int64, yamlContent, templatePath string) (string, error) {
	parsed, err := enjuYaml.Parse([]byte(yamlContent))
	if err != nil {
		return "", fmt.Errorf("parsing YAML for slug: %w", err)
	}
	slug := corelayout.ComputeRunSlug(templatePath, parsed.Run.Name)
	if slug == "" {
		slug = "run"
	}
	// Build the "used" set from two sources:
	//
	//   1. On-disk local refs (the source of truth for branch
	//      existence in single-store mode — plumbing-submit writes
	//      refs into the operator's .git/ directly). After a coord
	//      DB wipe, coord may not have remote_url for the
	//      re-created project, but the local refs are right there
	//      on disk where they've always been. Going through coord
	//      here would re-introduce the divergence the user hit.
	//
	//   2. Coord DB runs list. Catches in-session allocations
	//      that haven't been written to git yet AND projects where
	//      the local ref lookup fails for some reason.
	used := make(map[string]bool)
	if wf, _, _, _, werr := c.fc.OpenWorkflow(ctx, projectID); werr == nil {
		if existing, lsErr := wf.LocalBranches(); lsErr == nil {
			for _, b := range existing {
				used[b] = true
			}
		}
	}
	// Coord DB view as a complement.
	dbBranches, dbErr := c.listCoordBranches(ctx, projectID)
	if dbErr == nil {
		for _, b := range dbBranches {
			used[b] = true
		}
	}
	for n := 1; n <= 10000; n++ {
		name := fmt.Sprintf("%s-%d", slug, n)
		if !used[name] {
			return name, nil
		}
	}
	return "", fmt.Errorf("unable to allocate an auto branch after 10000 tries — pass branch=<name> explicitly")
}

// listCoordBranches returns the branch names coord has run
// records for. Used as a complement to ls-remote in the auto-
// branch picker — coord-DB is always available even when the
// project has no remote_url to ls-remote against.
func (c *apiClient) listCoordBranches(ctx context.Context, projectID int64) ([]string, error) {
	data, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs", projectID))
	if err != nil {
		return nil, err
	}
	// Response is a JSON array of run records, each with a
	// "branch" field. Extract just the names.
	var runs []map[string]interface{}
	if jerr := json.Unmarshal(data, &runs); jerr != nil {
		return nil, jerr
	}
	var names []string
	for _, r := range runs {
		if b, ok := r["branch"].(string); ok && b != "" {
			names = append(names, b)
		}
	}
	return names, nil
}

// handleExportDiagram snapshots the run's current DAG as raw
// Mermaid and commits it to enju/runs/{seq}-{slug}/graph/{phase}.mmd.
// See toolExportDiagram for the tool-facing contract.
func (c *apiClient) handleExportDiagram(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	phase := strings.TrimSpace(req.GetString("phase", ""))
	if err := validateDiagramPhase(phase); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	authorName, authorEmail := c.commitAuthor(ctx)
	body, res, err := c.fc.ExportDiagramFile(ctx, int64(projectID), runID, phase, authorName, authorEmail)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Build the reply: path + embed hint + fenced inline render
	// so the LLM has everything it needs to both show the user
	// the diagram right now and cite where it lives.
	var b strings.Builder
	if res.NoOp {
		b.WriteString(fmt.Sprintf("✓ Diagram unchanged — skipped commit. File: %s\n", res.RepoRelPath))
	} else {
		b.WriteString(fmt.Sprintf("✓ Diagram written to %s (commit %s)\n", res.RepoRelPath, format.ShortSHA(res.CommitSHA)))
	}
	b.WriteString(fmt.Sprintf("  Embed in markdown: ![](%s)\n\n", res.RepoRelPath))
	b.WriteString("```mermaid\n")
	b.WriteString(body)
	b.WriteString("```\n")
	return mcp.NewToolResultText(b.String()), nil
}

// validateDiagramPhase enforces the safe-filename contract on
// the `phase` argument. We deliberately allow any other char so
// the LLM can pick descriptive labels ("post_vote_stack_choice",
// "after_reject_v2"), but we block path-traversal characters and
// the null byte, cap length, and reject empty.
func validateDiagramPhase(phase string) error {
	if phase == "" {
		return fmt.Errorf("phase is required (common values: 'initial', 'final', or a custom label)")
	}
	if len(phase) > 64 {
		return fmt.Errorf("phase is too long (%d chars) — max 64", len(phase))
	}
	if strings.Contains(phase, "/") || strings.Contains(phase, "\\") || strings.Contains(phase, "..") || strings.ContainsRune(phase, 0) {
		return fmt.Errorf("phase contains forbidden characters ('/', '\\\\', '..', or null byte)")
	}
	return nil
}

// handleExportRunEvents pulls the coordinator's synthesized
// event timeline for a run and commits it as JSONL under
// enju/runs/{seq}-{slug}/events/{phase}.jsonl.
func (c *apiClient) handleExportRunEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	phase := strings.TrimSpace(req.GetString("phase", ""))
	if err := validateDiagramPhase(phase); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	authorName, authorEmail := c.commitAuthor(ctx)
	events, res, err := c.fc.ExportRunEventsFile(ctx, int64(projectID), runID, phase, authorName, authorEmail)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var b strings.Builder
	if res.NoOp {
		b.WriteString(fmt.Sprintf("✓ Events unchanged (%d total) — skipped commit. File: %s\n", len(events), res.RepoRelPath))
	} else {
		b.WriteString(fmt.Sprintf("✓ %d events written to %s (commit %s)\n", len(events), res.RepoRelPath, format.ShortSHA(res.CommitSHA)))
	}
	// Inline preview — up to 10 lines so the LLM can show the
	// tail of the timeline without opening the file.
	preview := events
	if len(preview) > 10 {
		b.WriteString(fmt.Sprintf("  (showing first 10 of %d)\n", len(events)))
		preview = preview[:10]
	}
	b.WriteString("\n```jsonl\n")
	for _, e := range preview {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString("```\n")
	return mcp.NewToolResultText(b.String()), nil
}

func (c *apiClient) handleExportRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runSeq, err := req.RequireInt("run_seq")
	if err != nil {
		return mcp.NewToolResultError("run_seq is required"), nil
	}
	md, err := c.fc.ExportRunMarkdown(ctx, int64(projectID), runSeq)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(md), nil
}
