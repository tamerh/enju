package bots

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// fakeCoordHTTP stands in for the coordinator REST surface.
// Records the last request payload + URL so tests can assert
// the wire shape, then replies with caller-configured
// responses.
type fakeCoordHTTP struct {
	t *testing.T

	// Pre-canned responses keyed by path.
	getResponses  map[string]string
	postResponses map[string]string
	postStatus    map[string]int

	// Recordings.
	lastPostPath string
	lastPostBody map[string]interface{}
}

func (f *fakeCoordHTTP) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.URL.RawQuery != "" {
			path = path + "?" + r.URL.RawQuery
		}
		switch r.Method {
		case "GET":
			body, ok := f.getResponses[path]
			if !ok {
				// Try without the query string — tests don't
				// always include it as part of the key.
				body, ok = f.getResponses[r.URL.Path]
			}
			if !ok {
				http.Error(w, "no fixture", 404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		case "POST":
			f.lastPostPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			f.lastPostBody = map[string]interface{}{}
			_ = json.Unmarshal(raw, &f.lastPostBody)
			status := http.StatusOK
			if s, ok := f.postStatus[r.URL.Path]; ok {
				status = s
			}
			body := f.postResponses[r.URL.Path]
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		default:
			http.Error(w, "unexpected method", 405)
		}
	})
}

func newHTTPClient(srv *httptest.Server) *HTTPCoordClient {
	return &HTTPCoordClient{C: coord.New(coord.Config{
		BaseURL:     srv.URL,
		Username:    "test-bot",
		AuthToken:   "test-token",
		CitizenName: "Test Bot",
	})}
}

func TestHTTPCoordClient_ListReadyForBot_Filters(t *testing.T) {
	// Coord returns three ready tasks; only one is assigned
	// to the bot. The others should be filtered out.
	fixture := `[
        {"id":"1:1:a","action":"review","prompt":"p1","assign_to":["test-bot"]},
        {"id":"1:1:b","action":"review","prompt":"p2","assign_to":["other-bot"]},
        {"id":"1:1:c","action":"vote","prompt":"p3","assign_to":["test-bot","other-bot"]}
    ]`
	fc := &fakeCoordHTTP{t: t, getResponses: map[string]string{
		"/api/v1/tasks/ready?project_id=7": fixture,
	}}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	h := newHTTPClient(srv)

	got, err := h.ListReadyForBot(context.Background(), 7, "test-bot")
	if err != nil {
		t.Fatalf("ListReadyForBot: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 tasks (assigned to bot), got %d: %+v", len(got), got)
	}
	for _, task := range got {
		if !contains(task.AssignTo, "test-bot") {
			t.Errorf("returned task %s not assigned to bot: %+v", task.ID, task.AssignTo)
		}
	}
}

func TestHTTPCoordClient_ListReadyForBot_NoProjectQuery(t *testing.T) {
	// project_id == 0 should NOT include the query string —
	// the coord interprets the absence of the param as
	// "across every project I can see."
	fc := &fakeCoordHTTP{t: t, getResponses: map[string]string{
		"/api/v1/tasks/ready": `[]`,
	}}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	h := newHTTPClient(srv)
	if _, err := h.ListReadyForBot(context.Background(), 0, "test-bot"); err != nil {
		t.Fatalf("ListReadyForBot: %v", err)
	}
}

func TestHTTPCoordClient_Claim_HappyPath(t *testing.T) {
	fc := &fakeCoordHTTP{t: t,
		postResponses: map[string]string{"/api/v1/tasks/1:1:x/claim": `{"ok":true}`},
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	h := newHTTPClient(srv)

	if err := h.Claim(context.Background(), "1:1:x", "test-bot", "claude-sonnet-4-6"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if fc.lastPostPath != "/api/v1/tasks/1:1:x/claim" {
		t.Errorf("posted to %q", fc.lastPostPath)
	}
	if fc.lastPostBody["username"] != "test-bot" {
		t.Errorf("username: got %v", fc.lastPostBody["username"])
	}
	if fc.lastPostBody["model"] != "claude-sonnet-4-6" {
		t.Errorf("model: got %v", fc.lastPostBody["model"])
	}
}

func TestHTTPCoordClient_Claim_RaceMaps(t *testing.T) {
	fc := &fakeCoordHTTP{t: t,
		postResponses: map[string]string{"/api/v1/tasks/1:1:x/claim": `{"error":"task already claimed by other-user"}`},
		postStatus:    map[string]int{"/api/v1/tasks/1:1:x/claim": 409},
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	h := newHTTPClient(srv)

	err := h.Claim(context.Background(), "1:1:x", "test-bot", "m")
	if err != ErrClaimRace {
		t.Errorf("expected ErrClaimRace, got %v", err)
	}
}

func TestHTTPCoordClient_Submit_Review(t *testing.T) {
	fc := &fakeCoordHTTP{t: t,
		postResponses: map[string]string{"/api/v1/tasks/1:1:r/result": `{"ok":true}`},
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	h := newHTTPClient(srv)

	task := TaskInfo{ID: "1:1:r", Action: "review"}
	resp := "approve\nLooks correct, ship it."
	if err := h.Submit(context.Background(), task, resp, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := fc.lastPostBody["decision"]; got != "approve" {
		t.Errorf("decision: got %v", got)
	}
	if got := fc.lastPostBody["content"]; !strings.Contains(got.(string), "Looks correct") {
		t.Errorf("content: got %v", got)
	}
}

func TestHTTPCoordClient_Submit_Vote(t *testing.T) {
	fc := &fakeCoordHTTP{t: t,
		postResponses: map[string]string{"/api/v1/tasks/1:1:v/result": `{"ok":true}`},
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	h := newHTTPClient(srv)

	if err := h.Submit(context.Background(), TaskInfo{ID: "1:1:v", Action: "vote"}, "  option-a  \n", "m"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := fc.lastPostBody["option"]; got != "option-a" {
		t.Errorf("option (should be trimmed): got %q", got)
	}
}

func TestHTTPCoordClient_Submit_UnsupportedAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("should not POST for unsupported action")
	}))
	defer srv.Close()
	h := newHTTPClient(srv)
	err := h.Submit(context.Background(), TaskInfo{ID: "x", Action: "answer"}, "text", "m")
	if err == nil || !strings.Contains(err.Error(), "doesn't support action=") {
		t.Errorf("expected unsupported-action error, got %v", err)
	}
}

func TestParseReviewResponse(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantDecide  string
		wantComment string // substring match
	}{
		{"approve_with_comment", "approve\nLooks good", "approve", "Looks good"},
		{"approve_inline", "APPROVE — ship it", "approve", "ship it"},
		{"reject_with_reason", "reject\nMissing tests", "reject", "Missing tests"},
		{"reject_inline", "Reject: too risky", "reject", "too risky"},
		{"request_changes_block", "request_changes\nFix the off-by-one", "request_changes", "Fix the off-by-one"},
		{"request_changes_inline", "request_changes: nit-pick on naming", "request_changes", "nit-pick on naming"},
		{"comment_block", "comment\nLove the variable names", "comment", "Love the variable names"},
		{"comment_inline", "comment: nice", "comment", "nice"},
		{"unparseable_defaults_to_request_changes", "looks fine i guess", "request_changes", "looks fine i guess"},
		{"empty_defaults_to_request_changes", "", "request_changes", "empty response"},
		// request_changes must NOT get clipped to "request" by
		// loop-order bugs — keep this case as a pin.
		{"request_changes_not_clipped", "request_changes — please address X", "request_changes", "please address X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, c := parseReviewResponse(tc.in)
			if d != tc.wantDecide {
				t.Errorf("decision: got %q, want %q", d, tc.wantDecide)
			}
			if !strings.Contains(c, tc.wantComment) {
				t.Errorf("comment: got %q, want substring %q", c, tc.wantComment)
			}
		})
	}
}
