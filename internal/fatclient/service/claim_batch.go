package service

// Bulk-claim-by-filter orchestration. The selector flow lives
// here so the mcphandlers/claim_batch.go entry point can stay
// thin (parse args → call ClaimReadyMatching → format result).
//
// Pipeline (mirrors the handler-side comment that used to live
// at the top of mcphandlers/claim_batch.go):
//
//   1. Fetch /tasks/ready scoped to (project, run).
//   2. Filter by action (optional) + assign_to (must include
//      this citizen) + state != "claimed by us" (idempotent
//      re-run).
//   3. Sort by seq ASC, cap at limit.
//   4. One pre-reconcile pull on the run's branch.
//   5. Loop: POST /tasks/{id}/claim per task.
//   6. Aggregate per-entry results — rendering happens in the
//      handler via the formatters that still live there.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
)

// ClaimMatchingParams is the input shape for
// FatClient.ClaimReadyMatching. Mirrors the MCP tool's surface.
type ClaimMatchingParams struct {
	ProjectID      int64
	RunID          int64
	ActionFilter   string
	IncludeContext bool
	Limit          int
}

// BatchClaimEntry captures one task's claim outcome for the
// aggregated response. Exported because the formatters in
// mcphandlers/claim_batch.go consume it directly.
type BatchClaimEntry struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"` // "claimed", "skipped", "error"
	Reason  string `json:"reason,omitempty"`
	Summary string `json:"summary,omitempty"` // one-line detail for humans
}

// ClaimMatchingResult bundles the structured data the
// formatters need to render the selector response without
// re-doing any work.
type ClaimMatchingResult struct {
	Entries           []BatchClaimEntry
	CandidatesScanned int
	ActionFilter      string
}

// ClaimReadyMatching executes the bulk-claim-by-filter flow.
// Returns the aggregated outcome; the caller renders it.
func (s *FatClient) ClaimReadyMatching(ctx context.Context, params ClaimMatchingParams) (*ClaimMatchingResult, error) {
	// Step 1: fetch ready tasks for (project, run).
	path := fmt.Sprintf("/api/v1/tasks/ready?project_id=%d&run_id=%d", params.ProjectID, params.RunID)
	data, err := s.coord.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	var readyRaw []map[string]interface{}
	if err := json.Unmarshal(data, &readyRaw); err != nil {
		return nil, fmt.Errorf("decoding ready-tasks response: %w", err)
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
	username := s.Username()
	var pool []candidate
	for _, t := range readyRaw {
		taskID, _ := t["id"].(string)
		if taskID == "" {
			continue
		}
		if params.ActionFilter != "" {
			if act, _ := t["action"].(string); act != params.ActionFilter {
				continue
			}
		}
		// assign_to pre-filter: if declared non-empty, this
		// citizen must be in the list. Silently drop any
		// task we're not eligible to claim.
		if assigned := format.StringSliceFromAny(t["assign_to"]); len(assigned) > 0 {
			allowed := false
			for _, u := range assigned {
				if u == username {
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
		if claimedBy, _ := t["claimed_by"].(string); claimedBy == username {
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
	if params.Limit > 0 && len(pool) > params.Limit {
		pool = pool[:params.Limit]
	}

	if len(pool) == 0 {
		return &ClaimMatchingResult{
			Entries:           nil,
			CandidatesScanned: len(readyRaw),
			ActionFilter:      params.ActionFilter,
		}, nil
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
	if s.enjugit != nil {
		if firstMeta, err := s.FetchTaskMeta(ctx, pool[0].taskID); err == nil && firstMeta != nil && s.UseFatClient(firstMeta) {
			if wf, _, _, _, perr := s.OpenWorkflow(ctx, firstMeta.ProjectID); perr == nil && wf != nil {
				_ = s.PullBranchWithReconcileWF(ctx, wf, firstMeta.ProjectID, firstMeta.Branch)
			}
		}
	}

	// Step 5: loop and claim. Each entry surfaces its own
	// status so callers can distinguish "you got it" from
	// "someone else beat you to it" without parsing free
	// text.
	results := make([]BatchClaimEntry, 0, len(pool))
	for _, cand := range pool {
		result := s.claimOneForSelector(ctx, cand.taskID, params.IncludeContext)
		results = append(results, result)
	}

	return &ClaimMatchingResult{
		Entries:           results,
		CandidatesScanned: len(readyRaw),
		ActionFilter:      params.ActionFilter,
	}, nil
}

// claimOneForSelector performs a single claim POST + minimal
// detail render, returning a BatchClaimEntry. Kept separate
// from ClaimTask because the selector doesn't need
// the latter's per-task pre-reconcile (done once upfront)
// nor its verbose context-fetch path by default (the whole
// point of the selector is lean bulk-work responses).
func (s *FatClient) claimOneForSelector(ctx context.Context, taskID string, includeContext bool) BatchClaimEntry {
	data, err := s.coord.Post(ctx, "/api/v1/tasks/"+taskID+"/claim", map[string]string{
		"username": s.Username(),
		"model":    s.modelName, // operator/model design — empty for unaided humans
	})
	if err != nil {
		return BatchClaimEntry{TaskID: taskID, Status: "error", Reason: err.Error()}
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return BatchClaimEntry{TaskID: taskID, Status: "error", Reason: "decoding claim response: " + err.Error()}
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
			return BatchClaimEntry{TaskID: taskID, Status: "skipped", Reason: errMsg}
		}
		return BatchClaimEntry{TaskID: taskID, Status: "error", Reason: errMsg}
	}

	// Build the per-entry summary. Default (lean) form: task
	// id + action + deadline + one-line action-specific
	// hint. Full-context form: hand off to format.ClaimResult
	// with the inputs fetched like a single claim.
	if includeContext {
		var inputs []byte
		if meta, _ := s.FetchTaskMeta(ctx, taskID); meta != nil && s.UseFatClient(meta) {
			inputs, _ = s.FetchAndResolveLocally(ctx, meta)
			if inputs == nil {
				inputs, _ = s.coord.Get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
			}
		} else {
			inputs, _ = s.coord.Get(ctx, "/api/v1/tasks/"+taskID+"/inputs")
		}
		return BatchClaimEntry{
			TaskID:  taskID,
			Status:  "claimed",
			Summary: format.ClaimResult(data, inputs, s.Username()),
		}
	}
	return BatchClaimEntry{
		TaskID:  taskID,
		Status:  "claimed",
		Summary: formatLeanClaimEntry(resp),
	}
}

// formatLeanClaimEntry renders the one-line summary that goes
// into the entry's Summary field for the default (lean)
// selector response. Pulls action + deadline + a handful of
// action-specific hints (options list for votes, reviews-target
// for reviews) — enough for a scripted caller to pipeline a
// submit without a second round-trip, without the 10+ lines
// that a full enju_claim_task response would emit.
//
// Lives on the service side because it's part of constructing
// the result (the value put into BatchClaimEntry.Summary), not
// a presentation choice the handler-side formatters make.
func formatLeanClaimEntry(claim map[string]interface{}) string {
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
		opts := format.ParseVoteOptionsForDisplay(optsRaw)
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
