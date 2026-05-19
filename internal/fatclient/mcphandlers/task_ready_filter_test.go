package mcphandlers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestHandleListReadyTasks_ForwardsProjectFilter is the bug hunt
// B-4 regression. enju_list_ready_tasks(project_id=N) (no run_id)
// used to send a bare /api/v1/tasks/ready — the project filter
// was silently dropped because the handler only appended query
// params when BOTH project_id AND run_id were set, so the
// coordinator returned every project's ready tasks. The handler
// must forward project_id and run_id independently.
func TestHandleListReadyTasks_ForwardsProjectFilter(t *testing.T) {
	cases := []struct {
		name      string
		args      map[string]interface{}
		wantQuery string
	}{
		{
			"project_id only (the B-4 bug)",
			map[string]interface{}{"project_id": 12},
			"project_id=12",
		},
		{
			"run_id only",
			map[string]interface{}{"run_id": 37},
			"run_id=37",
		},
		{
			"both",
			map[string]interface{}{"project_id": 12, "run_id": 37},
			"project_id=12&run_id=37",
		},
		{
			"neither — unscoped, no query",
			map[string]interface{}{},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				gotPath string
				gotRaw  string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath = r.URL.Path
				gotRaw = r.URL.RawQuery
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			c := newClient(coord.New(coord.Config{
				BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger,
			}), "", logger)

			_, err := c.handleListReadyTasks(context.Background(), mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      "enju_list_ready_tasks",
					Arguments: tc.args,
				},
			})
			if err != nil {
				t.Fatalf("handleListReadyTasks: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if gotPath != "/api/v1/tasks/ready" {
				t.Errorf("path = %q, want /api/v1/tasks/ready", gotPath)
			}
			if gotRaw != tc.wantQuery {
				t.Errorf("query = %q, want %q", gotRaw, tc.wantQuery)
			}
		})
	}
}
