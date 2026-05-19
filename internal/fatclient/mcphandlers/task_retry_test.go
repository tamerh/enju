package mcphandlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleRetryTask is a two-half composition: (1) the coordinator
// re-opens the failed_retryable task, then (2) the client executes
// the compute path. Step 2 is compute-only. The live test caught
// that a CITIZEN task's successful recovery still surfaced a
// spurious "action=… not compute — use enju_submit_result" error
// because the handler blindly ran step 2. The coordinator now
// returns is_compute; the handler must short-circuit citizen tasks
// to a success message (re-open only — the assignee re-claims).
//
// This pins the citizen path specifically: the coordinator/service
// retry generalization is unit-tested elsewhere, but the fat-client
// handler composition had no citizen-path coverage — exactly the
// gap that let the regression ship.
func TestHandleRetryTask_CitizenTask_ReOpenOnlyNoSpuriousError(t *testing.T) {
	var retryHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks/1:1:answer-a/retry", func(w http.ResponseWriter, r *http.Request) {
		retryHits++
		if r.Method != http.MethodPost {
			t.Errorf("retry: method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		// Coordinator re-opened the task; it is NOT a compute task,
		// so the handler must not attempt the execute half.
		_, _ = w.Write([]byte(`{"status":"retrying","task_id":"1:1:answer-a","from":"snapshot","new_state":"ready","is_compute":false}`))
	})
	// A bare 404 for anything else makes an accidental step-2
	// coordinator call (or any stray request) a visible failure
	// rather than a silent pass.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s %s — citizen retry must be re-open only", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := newE2EClient(ts.URL, "", "tok")

	resp, err := c.handleRetryTask(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_retry_task",
			Arguments: map[string]interface{}{"task_id": "1:1:answer-a"},
		},
	})
	if err != nil {
		t.Fatalf("handleRetryTask returned a Go error: %v", err)
	}
	text := mcpResultText(t, resp)

	if resp.IsError {
		t.Fatalf("citizen retry surfaced an error result (the regression): %q", text)
	}
	// The exact lie the live test caught must never reappear.
	if strings.Contains(text, "not compute") || strings.Contains(text, "enju_submit_result") {
		t.Fatalf("citizen retry must not surface the compute-only error; got: %q", text)
	}
	if !strings.Contains(text, "re-opened to READY") || !strings.Contains(text, "Citizen task") {
		t.Errorf("expected a re-open success message for a citizen task; got: %q", text)
	}
	if retryHits != 1 {
		t.Errorf("coordinator retry endpoint hit %d times, want exactly 1", retryHits)
	}
}

// Defensive: a malformed/empty coordinator body must not be read as
// a compute task (is_compute defaults to false → safe re-open
// message, never the execute half against a non-compute task).
func TestHandleRetryTask_MissingIsCompute_DefaultsToReOpenOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks/1:1:answer-b/retry", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"retrying","task_id":"1:1:answer-b","from":"snapshot"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := newE2EClient(ts.URL, "", "tok")
	resp, err := c.handleRetryTask(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_retry_task",
			Arguments: map[string]interface{}{"task_id": "1:1:answer-b"},
		},
	})
	if err != nil {
		t.Fatalf("handleRetryTask: %v", err)
	}
	if resp.IsError {
		t.Fatalf("absent is_compute must not error: %q", mcpResultText(t, resp))
	}
	if !strings.Contains(mcpResultText(t, resp), "re-opened to READY") {
		t.Errorf("absent is_compute should default to the safe re-open path; got: %q", mcpResultText(t, resp))
	}
}
