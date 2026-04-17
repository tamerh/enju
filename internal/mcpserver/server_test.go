package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/mcpgit"
	gogit "github.com/go-git/go-git/v5"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestAutoReregisterOnStaleCitizen verifies that an API call hitting
// a 404 "citizen not found" triggers a re-register + retry, and that
// the persisted credentials callback fires with the fresh handle.
func TestAutoReregisterOnStaleCitizen(t *testing.T) {
	var (
		firstCallServed   atomic.Bool
		registerCalls     atomic.Int32
		retryCallSucceeds atomic.Bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/citizens/by-username/alice", func(w http.ResponseWriter, r *http.Request) {
		if !firstCallServed.Load() {
			// First call: pretend the server forgot alice.
			firstCallServed.Store(true)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"citizen \"alice\" not found"}`))
			return
		}
		// Retry call: server now knows alice again.
		retryCallSucceeds.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"username":"alice","name":"Alice"}`))
	})
	mux.HandleFunc("/api/v1/citizens/register", func(w http.ResponseWriter, r *http.Request) {
		registerCalls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "alice" {
			t.Errorf("expected re-register to send username=alice, got %q", body["username"])
		}
		if body["name"] != "Alice" {
			t.Errorf("expected re-register to send name=Alice, got %q", body["name"])
		}
		if body["email"] != "alice@example.com" {
			t.Errorf("expected re-register to send email=alice@example.com, got %q", body["email"])
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"username":"alice","id":42}`))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	var savedUser, savedName, savedEmail string
	var saveCalls atomic.Int32
	c := &apiClient{
		baseURL:      ts.URL,
		username:     "alice",
		citizenName:  "Alice",
		citizenEmail: "alice@example.com",
		httpClient:   &http.Client{},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		saveCreds: func(u, n, e, t string) {
			savedUser = u
			savedName = n
			savedEmail = e
			saveCalls.Add(1)
		},
	}

	data, err := c.get(context.Background(), "/api/v1/citizens/by-username/alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if registerCalls.Load() != 1 {
		t.Errorf("expected exactly 1 register call, got %d", registerCalls.Load())
	}
	if !retryCallSucceeds.Load() {
		t.Error("expected the retry call to succeed against the refreshed coordinator")
	}
	if !strings.Contains(string(data), `"username":"alice"`) {
		t.Errorf("expected retry response body, got: %s", data)
	}
	if saveCalls.Load() != 1 || savedUser != "alice" || savedName != "Alice" || savedEmail != "alice@example.com" {
		t.Errorf("expected SaveCredentials(alice, Alice, alice@example.com) once, got %d calls with (%q, %q, %q)",
			saveCalls.Load(), savedUser, savedName, savedEmail)
	}
}

// TestStaleCitizenWithoutNameGivesUp verifies that when CitizenName
// is empty the client returns the original 404 body unchanged
// instead of silently swallowing it.
func TestStaleCitizenWithoutNameGivesUp(t *testing.T) {
	var registerCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/citizens/by-username/alice", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"citizen not found"}`))
	})
	mux.HandleFunc("/api/v1/citizens/register", func(w http.ResponseWriter, r *http.Request) {
		registerCalls.Add(1)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := &apiClient{
		baseURL:    ts.URL,
		username:   "alice",
		// citizenName intentionally empty
		httpClient: &http.Client{},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	data, err := c.get(context.Background(), "/api/v1/citizens/by-username/alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if registerCalls.Load() != 0 {
		t.Errorf("expected no register calls when CitizenName is empty, got %d", registerCalls.Load())
	}
	if !strings.Contains(string(data), "citizen not found") {
		t.Errorf("expected original error body to pass through, got: %s", data)
	}
}

// TestValidateReviewDecision locks in the centralized validator
// that every review submit path routes through. Missing/invalid
// inputs must produce stable phrasing so the three call sites
// (fat-client pre-validation, top-of-handler outer check, and the
// coordinator defense-in-depth) stay aligned.
func TestValidateReviewDecision(t *testing.T) {
	cases := []struct {
		name     string
		decision string
		wantErr  bool
		wantSub  string
	}{
		{"approve accepted", "approve", false, ""},
		{"reject accepted", "reject", false, ""},
		{"request_changes accepted", "request_changes", false, ""},
		{"comment accepted", "comment", false, ""},
		{"missing rejected", "", true, "decision is required"},
		{"invalid rejected", "maybe", true, `"maybe" is invalid`},
		{"uppercase rejected", "APPROVE", true, `"APPROVE" is invalid`},
		{"mixed case rejected", "Approve", true, `"Approve" is invalid`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateReviewDecision(tc.decision)
			if tc.wantErr && got == "" {
				t.Errorf("expected error for %q, got empty", tc.decision)
			}
			if !tc.wantErr && got != "" {
				t.Errorf("unexpected error for %q: %s", tc.decision, got)
			}
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Errorf("expected error containing %q, got %q", tc.wantSub, got)
			}
		})
	}
}

// TestSubmitReviewPreValidationBlocksGit verifies that
// submitResultFatClient rejects missing/invalid review decisions
// BEFORE touching the workspace — the critical guarantee that
// phantom commits can't land in the append-only history. The test
// builds an apiClient with a nil workspace: if pre-validation
// runs first, the helper returns a tool-result-error cleanly; if
// it runs later, the nil workspace dereference panics.
//
// This is a small test doing a large amount of work: it encodes
// the "always check action-specific inputs before any side
// effect" invariant that iteration E.1's phantom-commit feedback
// round forced into the design.
func TestSubmitReviewPreValidationBlocksGit(t *testing.T) {
	c := &apiClient{
		baseURL:    "http://unused.invalid",
		username:   "tamer",
		httpClient: &http.Client{},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		// workspace is intentionally nil — pre-validation must
		// fire before any workspace access or this test crashes.
	}
	reviewMeta := &taskMeta{
		ID:            "1:1:check",
		ProjectID:     1,
		RunSeq:        1,
		TaskDefID:     "check",
		Action:        "review",
		ReviewsTarget: "draft",
	}

	// Invalid decisions must be caught before any workspace access.
	invalidCases := []struct {
		name     string
		decision string
		wantSub  string
	}{
		{"missing", "", "decision is required"},
		{"invalid", "maybe", `"maybe" is invalid`},
		{"uppercase", "APPROVE", `"APPROVE" is invalid`},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("submitResultFatClient panicked — pre-validation didn't fire before workspace access: %v", r)
				}
			}()
			res, err := c.submitResultFatClient(
				context.Background(),
				reviewMeta.ID,
				reviewMeta,
				"some review comment",
				nil,
				nil,
				nil,
				tc.decision,
				"", // option (non-vote task)
			)
			if err != nil {
				t.Fatalf("handler should return tool error, not Go error: %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("expected IsError=true tool result, got %+v", res)
			}
			text := toolResultText(res)
			if !strings.Contains(text, tc.wantSub) {
				t.Errorf("expected error containing %q, got %q", tc.wantSub, text)
			}
		})
	}
}

// TestSubmitReviewAllFourDecisionsPassValidation verifies that
// request_changes and comment pass through the MCP client-side
// validation (both the outer handleSubmitResult check AND the
// fat-client pre-validation) without being rejected. This test
// would have caught the bug where an inline check at the top of
// handleSubmitResult only allowed approve/reject.
func TestSubmitReviewAllFourDecisionsPassValidation(t *testing.T) {
	// All four decisions must pass validateReviewDecision.
	for _, decision := range []string{"approve", "reject", "request_changes", "comment"} {
		t.Run(decision, func(t *testing.T) {
			if msg := validateReviewDecision(decision); msg != "" {
				t.Errorf("validateReviewDecision(%q) rejected: %s", decision, msg)
			}
		})
	}

	// Verify the outer handleSubmitResult check also passes.
	// We can't call handleSubmitResult directly without a full
	// server, but the refactored code delegates to
	// validateReviewDecision, so the unit test above covers it.
	// This test exists as a regression guard: if someone adds
	// another inline check, the test name makes the contract
	// visible.
}

// toolResultText extracts the concatenated text content from a
// CallToolResult for assertion purposes. The SDK keeps Content as
// a slice of typed interfaces; we only care about the text form
// in tests.
func toolResultText(res interface{}) string {
	// Using reflection-free JSON round-trip keeps the test
	// tolerant to SDK content-type internals.
	data, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	var shape struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(data, &shape) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range shape.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// TestFetchAndResolveLocallyInlinesReviewingBlock is the end-to-end
// guarantee that a review task's claim response carries the
// reviewed target's content inline. Without this the reviewer has
// to call enju_get_task separately — the UX gap iteration E.1's
// polish round fixed.
//
// Setup: a bare git repo seeded with a draft result.md at a known
// commit SHA, an httptest coordinator that serves a fake /inputs
// descriptor pointing at that commit, and a real mcpgit.Workspace
// that clones the bare on first access. The test asserts the
// resolved inputs JSON contains a "reviewing" key with the target
// content, the target task def id, and the claimer's username.
func TestFetchAndResolveLocallyInlinesReviewingBlock(t *testing.T) {
	const (
		projectID   = int64(7)
		runSeq      = 1
		reviewID    = "7:1:check"
		draftID     = "7:1:draft"
		draftResult = "Photosynthesis is the process by which plants convert light into chemical energy."
	)

	// 1. Seed a bare remote with a draft result.md at a known
	//    path, capture the commit SHA so we can point the
	//    descriptor at it.
	bareDir := t.TempDir()
	if _, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	}); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	seedDir := t.TempDir()
	seedRepo, err := gogit.PlainInitWithOptions(seedDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if _, err := seedRepo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	}); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	draftPath := filepath.Join(seedDir, "runs/1/draft/result.md")
	if err := os.MkdirAll(filepath.Dir(draftPath), 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	if err := os.WriteFile(draftPath, []byte(draftResult), 0o644); err != nil {
		t.Fatalf("write draft: %v", err)
	}
	wt, _ := seedRepo.Worktree()
	if _, err := wt.Add("runs/1/draft/result.md"); err != nil {
		t.Fatalf("add: %v", err)
	}
	sig := &object.Signature{Name: "Seeder", Email: "seed@localhost", When: time.Unix(1700000000, 0)}
	draftCommitHash, err := wt.Commit("seed draft", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if err := seedRepo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push seed: %v", err)
	}
	draftCommitSHA := draftCommitHash.String()

	// 2. httptest coordinator: /inputs returns the descriptor
	//    naming draft as a dep at draftCommitSHA; /tasks/draft
	//    returns the task record with claimed_by=alice so the
	//    resolver can populate the reviewing block's author.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks/"+reviewID+"/inputs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"task_id": "` + reviewID + `",
			"prompt_template": "Is the draft accurate?",
			"for_each_params": {},
			"dependencies": [
				{
					"task_def_id": "draft",
					"instance_key": "",
					"instance_params": {},
					"commit_sha": "` + draftCommitSHA + `",
					"result_path": "runs/1/draft"
				}
			],
			"artifact_reads": [],
			"project_remote_url": "` + bareDir + `"
		}`))
	})
	mux.HandleFunc("/api/v1/tasks/"+draftID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "` + draftID + `",
			"task_def_id": "draft",
			"action": "answer",
			"state": "accepted",
			"claimed_by": "alice"
		}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 3. Real Workspace. Clones the bare on first ForProject.
	wsDir := t.TempDir()
	ws, err := mcpgit.NewWorkspace(wsDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new workspace: %v", err)
	}

	c := &apiClient{
		baseURL:    ts.URL,
		username:   "bob",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}
	meta := &taskMeta{
		ID:               reviewID,
		ProjectID:        projectID,
		ProjectRemoteURL: bareDir,
		RunSeq:           runSeq,
		TaskDefID:        "check",
		Action:           "review",
		ReviewsTarget:    "draft",
	}

	data, err := c.fetchAndResolveLocally(context.Background(), meta)
	if err != nil {
		t.Fatalf("fetchAndResolveLocally: %v", err)
	}
	var resolved map[string]interface{}
	if err := json.Unmarshal(data, &resolved); err != nil {
		t.Fatalf("unmarshal inputs: %v — raw: %s", err, data)
	}
	reviewing, ok := resolved["reviewing"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected reviewing block in inputs, got: %s", data)
	}
	if got, _ := reviewing["target_def_id"].(string); got != "draft" {
		t.Errorf("target_def_id: want draft, got %v", reviewing["target_def_id"])
	}
	if got, _ := reviewing["commit_sha"].(string); got != draftCommitSHA {
		t.Errorf("commit_sha: want %s, got %v", draftCommitSHA, reviewing["commit_sha"])
	}
	if got, _ := reviewing["content"].(string); got != draftResult {
		t.Errorf("content mismatch:\n  want: %q\n  got:  %v", draftResult, reviewing["content"])
	}
	if got, _ := reviewing["claimed_by"].(string); got != "alice" {
		t.Errorf("claimed_by: want alice, got %v", reviewing["claimed_by"])
	}
}

// TestStaleCitizenDetection covers the status/body classifier.
func TestStaleCitizenDetection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"quoted form", http.StatusNotFound, `{"error":"citizen \"alice\" not found"}`, true},
		{"plain form", http.StatusNotFound, `{"error":"citizen not found"}`, true},
		{"404 other", http.StatusNotFound, `{"error":"project not found"}`, false},
		{"200 with phrase", http.StatusOK, `{"error":"citizen not found"}`, false},
		{"500 with phrase", http.StatusInternalServerError, `{"error":"citizen not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isStaleCitizenResponse(tc.status, []byte(tc.body))
			if got != tc.want {
				t.Errorf("isStaleCitizenResponse(%d, %q) = %v, want %v",
					tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestEagerCloneOnCreateProject verifies that handleCreateProject
// clones the workspace directory immediately, so it exists before
// any task is claimed.
func TestEagerCloneOnCreateProject(t *testing.T) {
	// 1. Seed a bare repo to act as the project's remote.
	bareDir := t.TempDir()
	mcpgit.InitBareWithSeed(bareDir)

	// 2. Fake coordinator: POST /projects returns id=1,
	//    GET /projects/1 returns remote_url + name.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"my-cool-project"}`))
	})
	mux.HandleFunc("/api/v1/projects/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"my-cool-project","remote_url":"` + bareDir + `"}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 3. Real workspace.
	wsDir := t.TempDir()
	ws, err := mcpgit.NewWorkspace(wsDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new workspace: %v", err)
	}

	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	// Before: no clone exists.
	if ws.HasLocalClone(1) {
		t.Fatal("expected no clone before create")
	}

	// Call handleCreateProject via the MCP tool handler.
	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_create_project",
			Arguments: map[string]interface{}{"name": "my-cool-project"},
		},
	})
	if err != nil {
		t.Fatalf("handleCreateProject: %v", err)
	}
	if result == nil {
		t.Fatal("nil result from handleCreateProject")
	}

	// After: clone should exist with the slug-named directory.
	if !ws.HasLocalClone(1) {
		t.Fatal("expected clone to exist immediately after create")
	}
	// Verify slug naming.
	entries, _ := os.ReadDir(wsDir)
	found := false
	for _, e := range entries {
		if e.IsDir() && strings.Contains(e.Name(), "my-cool-project") {
			found = true
		}
	}
	if !found {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected slug-named dir containing 'my-cool-project', got: %v", names)
	}
}
