package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

// --- Runs ---

// resolveRunBranch turns a caller's `branch:` request into a
// concrete branch name, honoring the project's default and the
// "auto" sugar form. Returns an error when the explicit name is
// malformed (validator matches git's ref grammar loosely enough
// to accept "experiment-2", "enju/work", etc. but rejects empty
// segments, leading "-", and reserved forms).
//
// For branch="auto", the generated name uses `<slug>-N` where the
// slug comes from engine.ComputeRunSlug(sourcePath, runName) — the
// same rule that stamps the `enju/runs/{seq}-{slug}/` directory
// name. Keeping both on the same slugger means the user never
// sees `git checkout quick-inline-1` pointing at a run whose dir
// is `2-Quick_Inline/` (style drift that surfaced on early
// testing).
// validateBranchName rejects shapes that git would refuse to
// store as a ref. Deliberately loose — matches the subset of
// git-check-ref-format rules that matter at create_run time.
// The full rules are enforced by git itself when the fat client
// pushes; this is a fast-fail upfront validation so a typo
// doesn't leave us with a half-created run.
func validateBranchName(s string) error {
	if s == "" {
		return fmt.Errorf("branch name cannot be empty")
	}
	if s == "HEAD" {
		return fmt.Errorf("branch name %q is reserved", s)
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return fmt.Errorf("branch name %q: must not start with '-' or '/' and must not end with '/'", s)
	}
	if strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{") {
		return fmt.Errorf("branch name %q contains a forbidden sequence (.., //, or @{)", s)
	}
	for _, r := range s {
		switch r {
		case ' ', '\t', '~', '^', ':', '?', '*', '[', '\\', '\x7f':
			return fmt.Errorf("branch name %q contains a forbidden character %q", s, r)
		}
		if r < 0x20 {
			return fmt.Errorf("branch name %q contains a control character", s)
		}
	}
	return nil
}

type createRunRequest struct {
	YAML      string         `json:"yaml"`
	RepoURL     string         `json:"repo_url,omitempty"`
	Params     map[string]interface{} `json:"params,omitempty"`
	SourcePath   string         `json:"source_path,omitempty"`
	SourceCommitSHA string         `json:"source_commit_sha,omitempty"`
	Username    string         `json:"username,omitempty"` // citizen who created this run, for contribution tracking
	// Branch is the git branch this run should commit to.
	// Three forms:
	//  - empty → fall back to the project's DefaultBranch
	//  - "auto" → the coordinator picks an unused branch name
	//   of the shape "run-N" so parallel variants don't force
	//   the caller to invent names
	//  - explicit name → use it verbatim
	// Refused when there's already an active run on the resolved
	// branch (serial-per-branch invariant).
	Branch string `json:"branch,omitempty"`
}

// runResponse aliases service.RunResponse so the literal-struct
// call sites in this package (handleCreateRun) keep working while
// the canonical shape lives in the shared service layer.
type runResponse = service.RunResponse

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	resp, err := s.coord.CreateRun(projectID, service.CreateRunParams{
		YAML:      req.YAML,
		RepoURL:     req.RepoURL,
		Params:     req.Params,
		SourcePath:   req.SourcePath,
		SourceCommitSHA: req.SourceCommitSHA,
		Username:    req.Username,
		Branch:     req.Branch,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.ListRuns(s.store, caller)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetRunCostSummary(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	tasks, _ := s.store.ListTasksByRun(run.ID)

	var citizenSet = map[int64]bool{}
	var firstClaim, lastAccept time.Time
	for _, t := range tasks {
		if store.TaskState(t.State) == store.TaskAccepted {
			if t.ClaimedBy > 0 {
				citizenSet[t.ClaimedBy] = true
			}
			if t.ClaimedAt != nil && (firstClaim.IsZero() || t.ClaimedAt.Before(firstClaim)) {
				firstClaim = *t.ClaimedAt
			}
			if t.SubmittedAt != nil && (lastAccept.IsZero() || t.SubmittedAt.After(lastAccept)) {
				lastAccept = *t.SubmittedAt
			}
		}
	}
	// Pull char counts + estimated tokens from contribution
	// events metadata (the task record doesn't store content
	// length — content lives in git, but the events log has
	// both prompt_chars and content_chars from submit time).
	var totalPromptChars, totalContentChars, totalEstTokens int64
	taskIDs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		taskIDs = append(taskIDs, t.ID)
	}
	for _, tid := range taskIDs {
		metadata, err := s.store.GetEventMetadataForTask(tid, "task_completed")
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal([]byte(metadata), &m) == nil {
			if v, ok := m["prompt_chars"].(float64); ok {
				totalPromptChars += int64(v)
			}
			if v, ok := m["content_chars"].(float64); ok {
				totalContentChars += int64(v)
			}
			if v, ok := m["estimated_tokens"].(float64); ok {
				totalEstTokens += int64(v)
			}
		}
	}

	var wallClock string
	if !firstClaim.IsZero() && !lastAccept.IsZero() {
		wallClock = lastAccept.Sub(firstClaim).Round(time.Second).String()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project_id":    projectID,
		"run_seq":     runSeq,
		"tasks_total":   len(tasks),
		"tasks_accepted":  countByState(tasks, store.TaskAccepted),
		"prompt_chars":   totalPromptChars,
		"content_chars":  totalContentChars,
		"estimated_tokens": totalEstTokens,
		"citizen_count":  len(citizenSet),
		"wall_clock":    wallClock,
	})
}

// handleListRunEvents returns the synthesized event timeline
// for one run — chronological JSON list built from
// events + task_claims. Consumed by the fat-
// client's enju_export_run_events tool which materializes
// the list into `enju/runs/{seq}/events/{phase}.jsonl`
// on demand. Authoritative data stays in the coordinator DB;
// git gets a snapshot only when the user asks for one.
func (s *Server) handleListRunEvents(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}
	// kill-switch UX. ListRunEvents synthesizes
	// from EventStore + state-DB task_claims; with events
	// disabled it serves a claims-only timeline (no signal
	// that audit is off). Header lets the MCP tool prepend
	// a "audit disabled — claims-only" warning. Body shape
	// stays the same for direct REST consumers.
	if !s.store.Events().Enabled() {
		w.Header().Set("X-Enju-Audit-Disabled", "true")
	}
	events, err := s.store.ListRunEvents(run.ProjectID, run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing events: "+err.Error())
		return
	}
	// Flatten into JSON-friendly shape with ts as RFC3339
	// (JSONL consumers parse this trivially) and metadata
	// as raw JSON (not a quoted string) when parseable.
	out := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		out = append(out, eventRowFromStore(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePauseRun moves a run to the `paused` state. Idempotent on
// already-paused runs; refuses on terminal (completed / failed)
// runs. Member-gated. Living-workflow phase 1 — see
// docs/living-workflow.md § 5 (out-of-band human interrupt).
func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.PauseRun(s.store, caller, projectID, runSeq)
	if err != nil {
		switch err {
		case service.ErrNotFound:
			writeError(w, http.StatusNotFound, "run not found")
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleResumeRun moves a paused run back to active or idle,
// depending on whether ready work exists. No-op on already-alive
// runs; refuses on terminal runs.
func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.ResumeRun(s.store, caller, projectID, runSeq)
	if err != nil {
		switch err {
		case service.ErrNotFound:
			writeError(w, http.StatusNotFound, "run not found")
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// terminateRunRequest is the JSON body for POST /terminate.
// Reason is optional, capped server-side to ~500 chars.
type terminateRunRequest struct {
	Reason string `json:"reason,omitempty"`
}

// handleTerminateRun is the human-pulled-the-plug endpoint:
// moves a run to the terminal "terminated" state, cascade-skips
// every non-terminal task, abandons every open claim. Member-
// gated. Refuses on already-terminal runs.
//
// Distinct from pause (reversible) and from a fail-cascade
// (system semantics). See store.TerminateRun for the cascade
// contract.
func (s *Server) handleTerminateRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req terminateRunRequest
	// Body is optional — empty body means no reason. Decode
	// errors on a non-empty body still surface as 400 so a
	// caller can't silently send malformed JSON.
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	resp, err := service.TerminateRun(s.store, caller, projectID, runSeq, req.Reason)
	if err != nil {
		switch err {
		case service.ErrNotFound:
			writeError(w, http.StatusNotFound, "run not found")
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type setCycleBudgetRequest struct {
	Max int `json:"max"`
}

// handleSetCycleBudget bumps the cycle-budget cap on a run.
// Used by operators to extend room after a runaway has been
// triaged and the underlying loop fixed. Member-gated.
func (s *Server) handleSetCycleBudget(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setCycleBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.SetCycleBudget(s.store, caller, projectID, runSeq, req.Max)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case err == service.ErrNotFound:
			writeError(w, http.StatusNotFound, "run not found")
		case err == service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// reportMergeRequest is the body shape for
// POST /projects/{p}/runs/{r}/merges. 's audit hook —
// the fat-client (or any merge-driving consumer) reports a
// successful FF-merge of a topic branch onto the run branch
// so the coordinator can emit a branch_merged event for the
// audit timeline. The coordinator does NOT verify the git
// state; it trusts the reporter (under linear progression
// the merge is already locked-in by git's FF check on the
// reporter side).
type reportMergeRequest struct {
	TopicBranch string `json:"topic_branch"`
	RunBranch  string `json:"run_branch"`
	MergeSHA  string `json:"merge_sha"`
	TaskID   string `json:"task_id,omitempty"` // optional — task whose ACCEPTED state drove this merge
}

// reportMergeConflictRequest is the body shape for
// POST /projects/{p}/runs/{r}/merges/conflicts. Fat-clients
// post this when their auto-merge of an ACCEPTED topic onto
// the run branch hits a content conflict — the accept stood,
// but the merge couldn't land cleanly. Phase 3 of the parallel-
// merge work will turn this into a merge_resolve task spawn;
// Phase 2 just records the audit event.
type reportMergeConflictRequest struct {
	TopicBranch   string   `json:"topic_branch"`
	RunBranch     string   `json:"run_branch"`
	TopicCommit   string   `json:"topic_commit"`
	RunTipCommit  string   `json:"run_tip_commit"`
	ConflictFiles []string `json:"conflict_files"`
	TaskID        string   `json:"task_id,omitempty"`
}

// handleReportMergeConflict — endpoint. Emits
// merge_conflict_detected with the topic/run/conflict-files
// payload. Body-of-truth in service.
func (s *Server) handleReportMergeConflict(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	var req reportMergeConflictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	caller := citizenFromRequest(r)
	resp, err := service.ReportMergeConflict(s.store, caller, projectID, runSeq, service.ReportMergeConflictParams{
		TopicBranch:   req.TopicBranch,
		RunBranch:     req.RunBranch,
		TopicCommit:   req.TopicCommit,
		RunTipCommit:  req.RunTipCommit,
		ConflictFiles: req.ConflictFiles,
		TaskID:        req.TaskID,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrNotMember):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// reportPushVerifyFailedRequest is the body shape for
// POST /projects/{p}/runs/{r}/push-verify-failed. Fat-clients
// post this when their post-push verify catches a silent-
// success state — push helper returned no error but the
// remote ref doesn't equal the local commit. Production
// trace: bot's SubmitTaskResult returned a SHA, coord stored
// it, but bare's branch ref stayed at the seed and downstream
// readers got "object not found".
type reportPushVerifyFailedRequest struct {
	Branch    string `json:"branch"`
	LocalSHA  string `json:"local_sha"`
	RemoteSHA string `json:"remote_sha,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
}

// handleReportPushVerifyFailed — endpoint. Emits
// push_verify_failed so the silent-push class of bugs surfaces
// in run_status / event log instead of being buried in a
// daemon-only log file.
func (s *Server) handleReportPushVerifyFailed(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	var req reportPushVerifyFailedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	caller := citizenFromRequest(r)
	resp, err := service.ReportPushVerifyFailed(s.store, caller, projectID, runSeq, service.ReportPushVerifyFailedParams{
		Branch:    req.Branch,
		LocalSHA:  req.LocalSHA,
		RemoteSHA: req.RemoteSHA,
		RemoteURL: req.RemoteURL,
		TaskID:    req.TaskID,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrNotMember):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleReportMerge — endpoint. Emits branch_merged with
// topic + run_branch + merge_sha. Body-of-truth in service.
func (s *Server) handleReportMerge(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))
	var req reportMergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	caller := citizenFromRequest(r)
	resp, err := service.ReportMerge(s.store, caller, projectID, runSeq, service.ReportMergeParams{
		TopicBranch: req.TopicBranch,
		RunBranch:  req.RunBranch,
		MergeSHA:  req.MergeSHA,
		TaskID:   req.TaskID,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrNotMember):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func countByState(tasks []store.TaskRecord, state store.TaskState) int {
	n := 0
	for _, t := range tasks {
		if store.TaskState(t.State) == state {
			n++
		}
	}
	return n
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))

	p, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	tasks, _ := s.store.ListTasksByRun(p.ID)

	resp := map[string]interface{}{
		"id":     p.ID,
		"project_id": p.ProjectID,
		"seq":    p.Seq,
		"name":    p.Name,
		"state":   p.State,
		"repo_url":  p.RepoURL,
		"branch":   p.Branch,
		"slug":    p.Slug,
		"task_count": len(tasks),
		"created_at": p.CreatedAt.Format(time.RFC3339),
	}
	if p.SourcePath != "" {
		resp["source_path"] = p.SourcePath
	}
	if p.SourceCommitSHA != "" {
		resp["source_commit_sha"] = p.SourceCommitSHA
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListRunTasks(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	runSeq, _ := strconv.Atoi(chi.URLParam(r, "runSeq"))

	run, err := s.store.GetRunByProjectSeq(projectID, runSeq)
	if err != nil || run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if _, ok := s.requireProjectMembership(w, r, projectID); !ok {
		return
	}

	tasks, err := s.store.ListTasksByRun(run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}
	writeJSON(w, http.StatusOK, s.toTaskResponses(tasks))
}
