package mcpserver

// Bulk-claim-by-filter. The `enju_claim_ready_matching` tool
// lets an agent claim every ready task in a run that matches
// an action filter in a single MCP call — the symmetric
// bulk primitive to `enju_submit_results_batch`. Designed
// for paper-scale evaluation workflows (bulk review,
// labeling cohorts) where N individual `enju_claim_task`
// calls bloat conversation context and tool-call budgets
// without buying wall-clock time.
//
// Pipeline (all steps under a single lock window for the
// pre-reconcile; claims themselves go through the
// coordinator one-by-one but under the coordinator's own
// per-task locking):
//
//   1. Fetch /tasks/ready scoped to (project, run).
//   2. Filter by action (optional) + assign_to (must include
//      this citizen) + state != "claimed by us" (idempotent
//      re-run).
//   3. Sort by seq ASC, cap at limit.
//   4. One pre-reconcile pull on the run's branch.
//   5. Loop: POST /tasks/{id}/claim per task.
//   6. Aggregate per-entry results, render summary + detail
//      lines.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// batchClaimEntry captures one task's claim outcome for the
// aggregated response. Mirrors batchEntryResult's shape so a
// caller can key off ("claimed" / "skipped" / "error") the
// same way.
type batchClaimEntry struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"` // "claimed", "skipped", "error"
	Reason  string `json:"reason,omitempty"`
	Summary string `json:"summary,omitempty"` // one-line detail for humans
}

const (
	defaultClaimSelectorLimit = 50
	maxClaimSelectorLimit     = 500
)

func (c *apiClient) handleClaimReadyMatching(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	actionFilter := strings.TrimSpace(req.GetString("action", ""))
	includeContext := req.GetBool("include_context", false)
	limit := req.GetInt("limit", defaultClaimSelectorLimit)
	if limit <= 0 {
		limit = defaultClaimSelectorLimit
	}
	if limit > maxClaimSelectorLimit {
		return mcp.NewToolResultError(fmt.Sprintf(
			"limit %d exceeds hard cap %d — lower it, or split the bulk claim across calls",
			limit, maxClaimSelectorLimit)), nil
	}

	// Step 1: fetch ready tasks for (project, run).
	path := fmt.Sprintf("/api/v1/tasks/ready?project_id=%d&run_id=%d", projectID, runID)
	data, err := c.get(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var readyRaw []map[string]interface{}
	if err := json.Unmarshal(data, &readyRaw); err != nil {
		return mcp.NewToolResultError("decoding ready-tasks response: " + err.Error()), nil
	}

	// Step 2: client-side filter. action + assign_to + dedup
	// of tasks already claimed by this citizen. require_role
	// intentionally not pre-filtered here — the client
	// doesn't know its own role (username is the canonical
	// handle on the wire); the coordinator rejects
	// role-mismatched claims and we surface those per-entry.
	type candidate struct {
		raw    map[string]interface{}
		taskID string
		seq    int
	}
	var pool []candidate
	for _, t := range readyRaw {
		taskID, _ := t["id"].(string)
		if taskID == "" {
			continue
		}
		if actionFilter != "" {
			if act, _ := t["action"].(string); act != actionFilter {
				continue
			}
		}
		// assign_to pre-filter: if declared non-empty, this
		// citizen must be in the list. Silently drop any
		// task we're not eligible to claim.
		if assigned := stringSliceFromAny(t["assign_to"]); len(assigned) > 0 {
			allowed := false
			for _, u := range assigned {
				if u == c.username {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}
		// Dedup: skip tasks already claimed by us. Lets a
		// caller re-run the selector safely to pick up
		// newly-ready tasks without reclaiming its own.
		if claimedBy, _ := t["claimed_by"].(string); claimedBy == c.username {
			continue
		}
		seqF, _ := t["seq"].(float64)
		pool = append(pool, candidate{raw: t, taskID: taskID, seq: int(seqF)})
	}

	// Step 3: deterministic order + cap. seq ASC matches the
	// DAG's natural ordering, so two concurrent callers
	// hitting this tool with the same project+run see the
	// same logical order — coordinator-side per-task
	// claim locking then arbitrates who wins each.
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].seq != pool[j].seq {
			return pool[i].seq < pool[j].seq
		}
		return pool[i].taskID < pool[j].taskID
	})
	if len(pool) > limit {
		pool = pool[:limit]
	}

	if len(pool) == 0 {
		return mcp.NewToolResultText(formatClaimMatchingSummary(nil, 0, actionFilter)), nil
	}

	// Step 4: one pre-reconcile pull for the run's branch.
	// Single-claim calls this per task; in bulk mode we pay
	// the cost once. Best-effort — a reconcile failure just
	// means slightly stale state for the batch, same as it
	// would for a single claim.
	//
	// Correctness of "one reconcile for all entries" rides
	// on the scope enforcement earlier in the pipeline:
	// every candidate shares the same (project, run), and
	// run → branch is 1:1, so pool[0]'s branch is pool[N]'s
	// branch. If a future relaxation allows cross-run
	// selectors, this optimization has to move inside the
	// per-entry loop.
	if c.workspace != nil {
		if firstMeta, err := c.fetchTaskMeta(ctx, pool[0].taskID); err == nil && firstMeta != nil && c.useFatClient(firstMeta) {
			if proj, _, _, _, perr := c.openProject(ctx, firstMeta.ProjectID); perr == nil && proj != nil {
				_ = c.pullBranchWithReconcile(ctx, proj, firstMeta.ProjectID, firstMeta.Branch)
			}
		}
	}

	// Step 5: loop and claim. Each entry surfaces its own
	// status so callers can distinguish "you got it" from
	// "someone else beat you to it" without parsing free
	// text.
	results := make([]batchClaimEntry, 0, len(pool))
	for _, cand := range pool {
		result := c.claimOneForSelector(ctx, cand.taskID, includeContext)
		results = append(results, result)
	}

	return mcp.NewToolResultText(formatClaimMatchingSummary(results, len(readyRaw), actionFilter)), nil
}

// claimOneForSelector performs a single claim POST + minimal
// detail render, returning a batchClaimEntry. Kept separate
// from handleClaimTask because the selector doesn't need
// the latter's per-task pre-reconcile (done once upfront)
// nor its verbose context-fetch path by default (the whole
// point of the selector is lean bulk-work responses).
func (c *apiClient) claimOneForSelector(ctx context.Context, taskID string, includeContext bool) batchClaimEntry {
	data, err := c.post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
		"username": c.username,
	})
	if err != nil {
		return batchClaimEntry{TaskID: taskID, Status: "error", Reason: err.Error()}
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return batchClaimEntry{TaskID: taskID, Status: "error", Reason: "decoding claim response: " + err.Error()}
	}
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		// "already claimed" races and already-terminal states
		// surface here. Classify as "skipped" (not "error")
		// because both are expected outcomes of a concurrent
		// selector — another citizen got there first, or the
		// task moved past ready between the /ready fetch and
		// the claim POST.
		//
		// CAVEAT: the classification keys on coordinator
		// error-message substrings. A coordinator-side
		// reword of either message would silently demote
		// those races to "error" here. Long-term fix: a
		// structured error code on the JSON response (e.g.
		// {"error": "...", "code": "already_claimed"}) that
		// both sides reference. Tracked as a coordinator-
		// side follow-up; this substring check is pinned to
		// the exact strings emitted by
		// internal/engine/claim.go (ComputeClaim +
		// acceptComputeTaskCore) and
		// internal/api/router.go's handleReportResult.
		if strings.Contains(errMsg, "already claimed") ||
			strings.Contains(errMsg, "cannot accept result") {
			return batchClaimEntry{TaskID: taskID, Status: "skipped", Reason: errMsg}
		}
		return batchClaimEntry{TaskID: taskID, Status: "error", Reason: errMsg}
	}

	// Build the per-entry summary. Default (lean) form: task
	// id + action + deadline + one-line action-specific
	// hint. Full-context form: hand off to formatClaimResult
	// with the inputs fetched like a single claim.
	if includeContext {
		var inputs []byte
		if meta, _ := c.fetchTaskMeta(ctx, taskID); meta != nil && c.useFatClient(meta) {
			inputs, _ = c.fetchAndResolveLocally(ctx, meta)
			if inputs == nil {
				inputs, _ = c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
			}
		} else {
			inputs, _ = c.get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
		}
		return batchClaimEntry{
			TaskID:  taskID,
			Status:  "claimed",
			Summary: formatClaimResult(data, inputs, c.username),
		}
	}
	return batchClaimEntry{
		TaskID:  taskID,
		Status:  "claimed",
		Summary: formatClaimMatchingEntryLean(resp),
	}
}

// formatClaimMatchingEntryLean renders the one-line summary
// shown per-entry in the default (lean) selector response.
// Pulls action + deadline + a handful of action-specific
// hints (options list for votes, reviews-target for reviews)
// — enough for a scripted caller to pipeline a submit
// without a second round-trip, without the 10+ lines that
// a full enju_claim_task response would emit.
func formatClaimMatchingEntryLean(claim map[string]interface{}) string {
	task, _ := claim["task"].(map[string]interface{})
	action, _ := task["action"].(string)
	deadline, _ := claim["deadline"].(string)
	var b strings.Builder
	if action != "" {
		b.WriteString("action=" + action)
	}
	if deadline != "" {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString("deadline=" + deadline)
	}
	if reviews, _ := task["reviews_target"].(string); reviews != "" {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString("reviews=" + reviews)
	}
	if optsRaw, _ := task["vote_options"].(string); optsRaw != "" {
		opts := parseVoteOptionsForDisplay(optsRaw)
		if len(opts) > 0 {
			ids := make([]string, 0, len(opts))
			for _, o := range opts {
				ids = append(ids, o.ID)
			}
			if b.Len() > 0 {
				b.WriteString(", ")
			}
			b.WriteString("options=[" + strings.Join(ids, ",") + "]")
		}
	}
	return b.String()
}

// formatClaimMatchingSummary renders the whole selector
// response: a header with N claimed / M skipped / E errored
// + optional action-filter echo, followed by per-entry
// lines. Humans read the header, agents parse the entries.
func formatClaimMatchingSummary(results []batchClaimEntry, candidatesScanned int, actionFilter string) string {
	var b strings.Builder
	var claimed, skipped, errored int
	for _, r := range results {
		switch r.Status {
		case "claimed":
			claimed++
		case "skipped":
			skipped++
		default:
			errored++
		}
	}
	if len(results) == 0 {
		if actionFilter != "" {
			b.WriteString(fmt.Sprintf("No ready tasks match action=%q (scanned %d ready task(s))\n", actionFilter, candidatesScanned))
		} else {
			b.WriteString(fmt.Sprintf("No ready tasks to claim (scanned %d)\n", candidatesScanned))
		}
		return b.String()
	}
	if errored == 0 && skipped == 0 {
		b.WriteString(fmt.Sprintf("✓ Claimed %d task(s)\n\n", claimed))
	} else {
		b.WriteString(fmt.Sprintf("Claim summary: %d claimed, %d skipped, %d errored\n\n", claimed, skipped, errored))
	}
	for _, r := range results {
		writeClaimEntryLine(&b, r)
	}
	return b.String()
}

// writeClaimEntryLine renders one row of the selector's
// per-entry section. Split out of formatClaimMatchingSummary
// because the claim row has three rendering shapes —
// single-line lean, multi-line full-context block, and
// reason-only (skipped/errored) — and inlining the picker
// made the loop hard to follow.
func writeClaimEntryLine(b *strings.Builder, r batchClaimEntry) {
	b.WriteString(claimStatusPrefix(r.Status))
	b.WriteString(" ")
	b.WriteString(r.TaskID)

	if r.Status == "claimed" && r.Summary != "" {
		if isSingleLineClaimSummary(r.Summary) {
			// Lean response: everything fits on the header
			// line. Append in parentheses so the task id
			// stays visually anchored.
			b.WriteString("  (" + r.Summary + ")\n")
			return
		}
		// include_context=true response: the summary is a
		// full formatClaimResult render. Put it on its own
		// indented block underneath the header row.
		b.WriteString("\n")
		for _, line := range strings.Split(r.Summary, "\n") {
			if line != "" {
				b.WriteString("    " + line + "\n")
			}
		}
		return
	}

	if r.Reason != "" {
		b.WriteString("  — " + r.Reason)
	}
	b.WriteString("\n")
}

// claimStatusPrefix picks the leading glyph for a
// per-entry line. ✓ = claimed this call; — = skipped
// (already claimed by us, race, terminal); ✗ = error.
func claimStatusPrefix(status string) string {
	switch status {
	case "claimed":
		return "✓"
	case "skipped":
		return "—"
	case "error":
		return "✗"
	}
	return "?"
}

// isSingleLineClaimSummary reports whether a summary string
// is the lean one-line form (no embedded newlines). Used to
// pick the inline-append vs indented-block render path.
func isSingleLineClaimSummary(s string) bool {
	return !strings.ContainsRune(s, '\n')
}
