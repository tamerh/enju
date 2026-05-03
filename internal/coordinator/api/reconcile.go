package api

import (
	"encoding/json"
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/service"
)

// reconcileEntry is one item in a POST /tasks/reconcile batch.
// Same semantics as submitResultRequest fields but flattened for
// batch transport. All fields are optional on the wire except
// TaskID + CommitSHA + ExitCode — the fetch-path scanner extracts
// them from commit trailers and forwards whatever it parsed.
type reconcileEntry struct {
	TaskID      string  `json:"task_id"`
	CommitSHA    string  `json:"commit_sha"`
	ExitCode     int   `json:"exit_code"`
	ResultPath    string  `json:"result_path,omitempty"`
	ArtifactsWritten []string `json:"artifacts_written,omitempty"`
	Content     string  `json:"content,omitempty"`
	FailReason    string  `json:"fail_reason,omitempty"` // optional override when ExitCode != 0
	Username     string  `json:"username,omitempty"`
	Model      string  `json:"model,omitempty"`
}

// reconcileBatchRequest is the top-level shape posted to
// /tasks/reconcile. Accepts either a batch or a single entry
// inline — scanners send batches, individual callers (wrapper's
// future async path) send one entry at a time.
type reconcileBatchRequest struct {
	Tasks []reconcileEntry `json:"tasks"`
}

// reconcileResult is the per-entry outcome returned in the
// response. Status is one of "accepted", "failed", "noop"
// (already terminal at this commit), or "error" — the scanner
// advances its cursor past entries regardless of status so a
// persistent error doesn't wedge the queue, but surfaces the
// error text so humans can diagnose.
type reconcileResult struct {
	TaskID  string `json:"task_id"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// handleReconcileTasks delegates to the service-layer reconciler.
// Per-entry results carry their own status/error so the batch
// emits one writeJSON at the end.
func (s *Server) handleReconcileTasks(w http.ResponseWriter, r *http.Request) {
	var req reconcileBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Tasks) == 0 {
		writeError(w, http.StatusBadRequest, "tasks is required and must be non-empty")
		return
	}
	caller := citizenFromRequest(r)
	results := make([]service.ReconcileResult, 0, len(req.Tasks))
	for _, entry := range req.Tasks {
		results = append(results, s.coord.ReconcileTask(caller, service.ReconcileEntry{
			TaskID:      entry.TaskID,
			CommitSHA:    entry.CommitSHA,
			ExitCode:     entry.ExitCode,
			ResultPath:    entry.ResultPath,
			ArtifactsWritten: entry.ArtifactsWritten,
			Content:     entry.Content,
			FailReason:    entry.FailReason,
			Username:     entry.Username,
			Model:      entry.Model,
		}))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}
