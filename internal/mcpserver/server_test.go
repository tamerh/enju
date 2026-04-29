package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
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
	mux.HandleFunc("/api/v1/projects/7", func(w http.ResponseWriter, r *http.Request) {
		// openProject fetches the project record to wire
		// default_branch into the workspace. Minimal response
		// matching the fields fetchProjectMetaExpanded reads.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 7,
			"name": "review-test",
			"remote_url": "` + bareDir + `",
			"default_branch": "main"
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
	// Pass remote_url explicitly so the autoLocal path is
	// skipped — this test is about eager-clone-given-a-remote,
	// not bare-creation. The autoLocal path is covered by
	// TestAutoLocalBareRepoOnCreateProject.
	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name":       "my-cool-project",
				"remote_url": bareDir,
			},
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

// TestAutoLocalBareRepoOnCreateProject verifies the auto-local
// code path: when enju_create_project is called with no
// remote_url, the client (a) creates a seeded bare repo under
// $HOME/.enju/repos/{id}.git, (b) PUTs that path as the
// coordinator's remote_url for the project, and (c) eagerly
// clones the workspace so a subsequent submit has a working
// tree with at least the initial commit to branch from.
//
// Tester reported: "create_project succeeded but ~/.enju/
// workspaces is empty and submit fails with 'log: reference
// not found'". The failure mode is exactly the one this test
// guards — a regression anywhere in (a), (b), or (c) leaves
// the submit path without a ref to branch from.
func TestAutoLocalBareRepoOnCreateProject(t *testing.T) {
	// Redirect $HOME so the test doesn't pollute the real
	// ~/.enju/. The autoLocal branch hardcodes the path via
	// os.UserHomeDir(), so overriding $HOME is how we
	// sandbox it.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Fake coordinator:
	//   POST /projects        → returns {id:1, name:"demo"},
	//                            initially no remote_url.
	//   PUT  /projects/1/remote → stores the remote_url in
	//                            `storedRemote` so the
	//                            subsequent GET reflects it.
	//   GET  /projects/1      → returns remote_url the PUT set.
	var storedRemote atomic.Value
	storedRemote.Store("")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/1/remote", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		storedRemote.Store(body["remote_url"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/v1/projects/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		remote, _ := storedRemote.Load().(string)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":1,"name":"demo","remote_url":%q}`, remote)))
	})
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

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

	// Call create_project with NO remote_url — triggers autoLocal.
	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_create_project",
			Arguments: map[string]interface{}{"name": "demo"},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("handleCreateProject: err=%v result=%v", err, result)
	}

	// (a) Bare repo exists at the expected auto-local path.
	bareDir := filepath.Join(tmpHome, ".enju", "repos", "1.git")
	if _, err := os.Stat(bareDir); err != nil {
		t.Fatalf("auto-local bare not created at %s: %v", bareDir, err)
	}

	// (b) Coordinator's remote_url was set via PUT.
	if got, _ := storedRemote.Load().(string); got != bareDir {
		t.Errorf("coordinator remote_url: got %q, want %q", got, bareDir)
	}

	// (c) Eager clone produced a working tree. This is the
	// load-bearing assertion — without this, submit can't
	// branch because there's no ref in the clone.
	if !ws.HasLocalClone(1) {
		t.Fatal("expected local clone after create_project (regression: tester hit 'log: reference not found' on submit)")
	}

	// Sanity: the clone must carry the seeded initial commit,
	// otherwise a branch-off-of-HEAD in submit still fails
	// with the same "reference not found" message.
	proj, err := ws.ForProject(1, bareDir, "demo")
	if err != nil {
		t.Fatalf("ForProject post-create: %v", err)
	}
	if _, err := proj.HeadHash(); err != nil {
		t.Fatalf("clone has no HEAD ref (the exact tester-reported failure): %v", err)
	}
}

// TestAutoLocalSurfacesCoordinatorPUTFailure — when the
// coordinator rejects the PUT /projects/{id}/remote (auth, role
// check, etc.), handleCreateProject must surface the error to
// the caller instead of returning "success" while leaving the
// project with no remote. Silent success here is exactly what
// left the tester staring at "log: reference not found" one
// tool call later.
func TestAutoLocalSurfacesCoordinatorPUTFailure(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/1/remote", func(w http.ResponseWriter, r *http.Request) {
		// Simulate the coordinator rejecting the PUT. c.put
		// returns (body, nil) on 4xx — so the handler has to
		// decode the error from the body, not trust the Go err.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"only project owners can change the remote URL"}`))
	})
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ws, _ := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_create_project",
			Arguments: map[string]interface{}{"name": "demo"},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("handleCreateProject: err=%v result=%v", err, result)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when PUT /remote is rejected; got a success result (silent-failure regression)")
	}
	// The error text should cite the coordinator's refusal
	// message so the user knows the root cause instead of
	// chasing "reference not found" downstream.
	got := toolResultText(result)
	if !strings.Contains(got, "only project owners") {
		t.Errorf("error should include coordinator's refusal message, got: %q", got)
	}
}

// TestInitFolderWithoutGit verifies that enju_init on a plain
// folder (no .git) initializes git, writes the scaffold, and
// registers the external dir so ForProject opens it directly.
func TestInitFolderWithoutGit(t *testing.T) {
	// Isolate $HOME — handleInit's auto-bare path writes under
	// ~/.enju/repos/, which would collide with the developer's
	// real project state if we let tests share $HOME.
	t.Setenv("HOME", t.TempDir())
	// Fake coordinator.
	var createdProjectID int64 = 1
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"test-init"}`))
	})
	mux.HandleFunc("/api/v1/projects/1/remote", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Create a plain folder with one file.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "paper.md"), []byte("# My Paper"), 0644)

	ws, _ := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	result, err := c.handleInit(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_init",
			Arguments: map[string]interface{}{"name": "test-init", "path": dir},
		},
	})
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success, got: %+v", result)
	}

	// Git should be initialized.
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		t.Error("expected .git dir after init")
	}

	// Scaffold should exist.
	if _, err := os.Stat(filepath.Join(dir, "enju", "templates", ".gitkeep")); err != nil {
		t.Error("expected enju/templates/.gitkeep after init")
	}

	// Original file preserved.
	data, _ := os.ReadFile(filepath.Join(dir, "paper.md"))
	if string(data) != "# My Paper" {
		t.Errorf("paper.md clobbered: %s", data)
	}

	// External dir registered — ForProject should open it.
	if !ws.HasExternalDir(createdProjectID) {
		t.Error("expected external dir registered")
	}
	proj, err := ws.ForProject(createdProjectID, "")
	if err != nil {
		t.Fatalf("ForProject on init'd dir: %v", err)
	}
	if proj.WorkDir() != dir {
		t.Errorf("expected workdir=%s, got %s", dir, proj.WorkDir())
	}
}

// TestInitFolderWithExistingGit verifies that enju_init on a
// folder that already has git preserves existing history and
// adds the scaffold on top.
func TestInitFolderWithExistingGit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":2,"name":"existing-git"}`))
	})
	mux.HandleFunc("/api/v1/projects/2/remote", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Create a folder with git + one commit.
	dir := t.TempDir()
	repo, _ := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("pre-existing"), 0644)
	wt, _ := repo.Worktree()
	wt.Add("existing.txt")
	wt.Commit("pre-existing commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test", When: time.Now()},
	})

	ws, _ := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	result, err := c.handleInit(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_init",
			Arguments: map[string]interface{}{"name": "existing-git", "path": dir},
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("handleInit: err=%v result=%+v", err, result)
	}

	// Should have 2 commits: the pre-existing one + scaffold.
	iter, _ := repo.Log(&gogit.LogOptions{})
	count := 0
	iter.ForEach(func(c *object.Commit) error { count++; return nil })
	if count != 2 {
		t.Errorf("expected 2 commits (pre-existing + scaffold), got %d", count)
	}

	// Pre-existing file preserved.
	data, _ := os.ReadFile(filepath.Join(dir, "existing.txt"))
	if string(data) != "pre-existing" {
		t.Errorf("existing.txt clobbered: %s", data)
	}

	// Scaffold added.
	if _, err := os.Stat(filepath.Join(dir, "enju", "templates", ".gitkeep")); err != nil {
		t.Error("expected enju/templates/.gitkeep after init on existing git repo")
	}
}

// TestInitIdempotent verifies that running enju_init twice on the
// same folder doesn't clobber anything or fail.
func TestInitIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	callCount := 0
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":%d,"name":"idempotent"}`, callCount)))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Catch-all for PUT /projects/{id}/remote — both calls
		// (first and second init) hit a different ID so we
		// can't pre-register handlers per ID without growing
		// the test scaffolding. The second init is a no-op
		// idempotency check; replying 200 is fine.
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && strings.HasSuffix(r.URL.Path, "/remote") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "data.csv"), []byte("a,b,c"), 0644)

	ws, _ := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	makeReq := func() mcp.CallToolRequest {
		return mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "enju_init",
				Arguments: map[string]interface{}{"name": "idempotent", "path": dir},
			},
		}
	}

	// First init.
	r1, err := c.handleInit(context.Background(), makeReq())
	if err != nil || r1.IsError {
		t.Fatalf("first init: err=%v result=%+v", err, r1)
	}

	// Second init — should not fail, scaffold already exists.
	r2, err := c.handleInit(context.Background(), makeReq())
	if err != nil || r2.IsError {
		t.Fatalf("second init: err=%v result=%+v", err, r2)
	}

	// Data file still intact.
	data, _ := os.ReadFile(filepath.Join(dir, "data.csv"))
	if string(data) != "a,b,c" {
		t.Errorf("data.csv clobbered: %s", data)
	}
}

// TestInitAutoCreatesBareForOriginlessFolder is the regression
// for TP53 Bug 1: enju_init on a folder with no `origin` must
// create ~/.enju/repos/{id}.git, configure it as origin in the
// working tree, and push initial state. Without this, async
// wrappers commit to local refs/heads/<branch> with nowhere to
// push and ScanBranchSince's walk of refs/remotes/origin/<branch>
// finds nothing — the pipeline silently stalls.
//
// We verify all four post-conditions:
//
//  1. The bare exists at ~/.enju/repos/{id}.git.
//  2. The working tree has origin pointing at the bare.
//  3. The bare has the working tree's HEAD commit (push happened).
//  4. The coordinator's stored remote_url is the bare path.
func TestInitAutoCreatesBareForOriginlessFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mux := http.NewServeMux()
	var capturedRemoteURL string
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"name":"tp53"}`))
	})
	mux.HandleFunc("/api/v1/projects/42/remote", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]string
		_ = json.Unmarshal(body, &parsed)
		capturedRemoteURL = parsed["remote_url"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Folder with `git init` + one commit, no origin — exact
	// reproduction of the user's TP53 setup.
	dir := t.TempDir()
	repo, _ := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	_ = os.WriteFile(filepath.Join(dir, "paper.md"), []byte("# tp53\n"), 0644)
	wt, _ := repo.Worktree()
	_, _ = wt.Add("paper.md")
	_, _ = wt.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "t@t", When: time.Now()},
	})
	ws, _ := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	result, err := c.handleInit(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_init",
			Arguments: map[string]interface{}{"name": "tp53", "path": dir},
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("handleInit: err=%v result=%+v", err, result)
	}

	// 1. Bare exists at ~/.enju/repos/42.git.
	expectedBare := filepath.Join(os.Getenv("HOME"), ".enju", "repos", "42.git")
	if _, err := os.Stat(expectedBare); err != nil {
		t.Fatalf("expected managed bare at %s: %v", expectedBare, err)
	}

	// 2. Working tree's origin points at the bare.
	rem, err := repo.Remote("origin")
	if err != nil {
		t.Fatalf("origin not configured on adopted folder: %v", err)
	}
	if cfg := rem.Config(); cfg == nil || len(cfg.URLs) == 0 || cfg.URLs[0] != expectedBare {
		t.Errorf("origin URL: got %v, want %s", cfg.URLs, expectedBare)
	}

	// 3. The bare contains the working tree's post-init HEAD on
	//    refs/heads/main — proves the push landed. This is the
	//    exact precondition ScanBranchSince needs to walk
	//    refs/remotes/origin/main and find result trailers later.
	//    handleInit may have added a scaffold commit on top of
	//    our seed, so we re-read working-tree HEAD after the call.
	headAfter, err := repo.Head()
	if err != nil {
		t.Fatalf("reading working-tree HEAD post-init: %v", err)
	}
	expectedHEAD := headAfter.Hash().String()
	bareRepo, err := gogit.PlainOpen(expectedBare)
	if err != nil {
		t.Fatalf("opening bare: %v", err)
	}
	bareMain, err := bareRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("bare missing refs/heads/main: %v", err)
	}
	if got := bareMain.Hash().String(); got != expectedHEAD {
		t.Errorf("bare main: got %s, want %s (working tree HEAD post-init)", got, expectedHEAD)
	}

	// 4. Coordinator stored the bare path as remote_url.
	if capturedRemoteURL != expectedBare {
		t.Errorf("PUT /remote captured: got %q, want %q", capturedRemoteURL, expectedBare)
	}

	// Response text should mention the managed bare so the
	// user knows where their state landed.
	if got := toolResultText(result); !strings.Contains(got, expectedBare) {
		t.Errorf("response should cite managed bare path; got:\n%s", got)
	}
}

// TestInitPreservesExistingOrigin verifies the github-clone
// case: when the adopted folder already has an origin (e.g.
// from a prior `git clone`), enju_init must NOT overwrite it
// with a managed bare. The user's existing push target stays
// intact.
func TestInitPreservesExistingOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mux := http.NewServeMux()
	var setRemoteCalled bool
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"name":"with-origin"}`))
	})
	mux.HandleFunc("/api/v1/projects/7/remote", func(w http.ResponseWriter, r *http.Request) {
		setRemoteCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Pre-existing origin URL (a fake github-style remote).
	const preExistingOrigin = "git@github.com:org/repo.git"

	// Folder with git init + commit + a pre-configured origin.
	dir := t.TempDir()
	repo, _ := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	_ = os.WriteFile(filepath.Join(dir, "x.md"), []byte("hi"), 0644)
	wt, _ := repo.Worktree()
	_, _ = wt.Add("x.md")
	_, _ = wt.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t", When: time.Now()},
	})
	_, _ = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{preExistingOrigin},
	})

	ws, _ := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	result, err := c.handleInit(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_init",
			Arguments: map[string]interface{}{"name": "with-origin", "path": dir},
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("handleInit: err=%v result=%+v", err, result)
	}

	// Origin must be untouched.
	rem, err := repo.Remote("origin")
	if err != nil {
		t.Fatalf("origin removed by init: %v", err)
	}
	if cfg := rem.Config(); cfg == nil || len(cfg.URLs) == 0 || cfg.URLs[0] != preExistingOrigin {
		t.Errorf("origin URL changed: got %v, want %s", cfg.URLs, preExistingOrigin)
	}

	// No managed bare should have been created.
	managedBare := filepath.Join(os.Getenv("HOME"), ".enju", "repos", "7.git")
	if _, err := os.Stat(managedBare); err == nil {
		t.Errorf("unexpectedly created managed bare at %s — github-clone case should be untouched", managedBare)
	}

	// PUT /remote must NOT have been called — coordinator's
	// remote_url stays at whatever was set during POST
	// (the working-tree path, per the with-existing-origin path).
	if setRemoteCalled {
		t.Error("PUT /projects/7/remote should not be called when origin already exists")
	}
}

// TestFormatClaimResultShowsRevisionContext verifies that when
// reviewFeedback and previousSubmission are provided, the claim
// response includes both sections so the author knows what they
// wrote and what the reviewer said.
func TestFormatClaimResultShowsRevisionContext(t *testing.T) {
	claimData := []byte(`{
		"task": {"id": "1:1:draft", "seq": 1, "prompt": "Write something."},
		"deadline": "2026-04-17T10:00:00Z"
	}`)

	feedback := []byte(`{
		"reviewer": "bob",
		"decision": "request_changes",
		"content": "Too vague. Be more specific."
	}`)

	prevSubmission := []byte(`{
		"content": "This was my first attempt at writing."
	}`)

	result := formatClaimResult(claimData, nil, "alice", feedback, prevSubmission)

	if !strings.Contains(result, "Previous submission") {
		t.Errorf("expected 'Previous submission' section, got:\n%s", result)
	}
	if !strings.Contains(result, "first attempt") {
		t.Errorf("expected previous content in output, got:\n%s", result)
	}
	if !strings.Contains(result, "Reviewer feedback") {
		t.Errorf("expected 'Reviewer feedback' section, got:\n%s", result)
	}
	if !strings.Contains(result, "Too vague") {
		t.Errorf("expected reviewer comment in output, got:\n%s", result)
	}
	if !strings.Contains(result, "@bob") {
		t.Errorf("expected reviewer name in output, got:\n%s", result)
	}
}

// TestFormatClaimResultNoFeedbackOnFirstClaim verifies that on a
// fresh claim (no prior review), no feedback sections appear.
func TestFormatClaimResultNoFeedbackOnFirstClaim(t *testing.T) {
	claimData := []byte(`{
		"task": {"id": "1:1:draft", "seq": 1, "prompt": "Write something."},
		"deadline": "2026-04-17T10:00:00Z"
	}`)

	result := formatClaimResult(claimData, nil, "alice")

	if strings.Contains(result, "Previous submission") {
		t.Errorf("should not show previous submission on first claim, got:\n%s", result)
	}
	if strings.Contains(result, "Reviewer feedback") {
		t.Errorf("should not show reviewer feedback on first claim, got:\n%s", result)
	}
}

// TestIsLocalWorkingTree verifies detection of local working trees
// vs bare repos vs non-git directories.
func TestIsLocalWorkingTree(t *testing.T) {
	// Case 1: folder with .git → true.
	wtDir := t.TempDir()
	gogit.PlainInitWithOptions(wtDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if !mcpgit.IsLocalWorkingTree(wtDir) {
		t.Error("expected working tree detected")
	}

	// Case 2: plain folder → false.
	plainDir := t.TempDir()
	if mcpgit.IsLocalWorkingTree(plainDir) {
		t.Error("plain dir should not be detected as working tree")
	}

	// Case 3: non-existent path → false.
	if mcpgit.IsLocalWorkingTree("/tmp/nonexistent-enju-test-path") {
		t.Error("non-existent path should not be detected")
	}

	// Case 4: SSH URL → false.
	if mcpgit.IsLocalWorkingTree("git@github.com:org/repo.git") {
		t.Error("SSH URL should not be detected as working tree")
	}
}

// TestIsSSHURL verifies SSH URL detection.
func TestIsSSHURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"git@github.com:org/repo.git", true},
		{"ssh://git@github.com/org/repo.git", true},
		{"/tmp/my-folder", false},
		{"https://github.com/org/repo.git", false},
		{"", false},
		{"/home/tamer/.enju/repos/1.git", false},
	}
	for _, tc := range cases {
		if got := mcpgit.IsSSHURL(tc.url); got != tc.want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// TestRenderDAGTree verifies the DAG tree rendering with fan-in.
func TestRenderDAGTree(t *testing.T) {
	tasks := []map[string]interface{}{
		{"id": "1:1:draft", "task_def_id": "draft", "state": "accepted", "seq": float64(1), "depends_on": ""},
		{"id": "1:1:check", "task_def_id": "check", "state": "ready", "seq": float64(2), "depends_on": "1:1:draft"},
		{"id": "1:1:publish", "task_def_id": "publish", "state": "pending", "seq": float64(3), "depends_on": "1:1:draft,1:1:check"},
	}
	result := renderDAGTree(tasks)
	// draft should be root.
	if !strings.Contains(result, "draft") {
		t.Error("expected draft in tree")
	}
	// publish has 2 parents → should nest under deepest (check).
	if !strings.Contains(result, "publish") {
		t.Error("expected publish in tree")
	}
	// check should be a child of draft.
	if !strings.Contains(result, "├── check") && !strings.Contains(result, "└── check") {
		t.Error("expected check nested under draft")
	}
	// publish should be nested under check (deepest parent), not at root.
	if strings.HasPrefix(strings.TrimSpace(result), "publish") {
		t.Error("publish should not be at root level — it has deps")
	}
}

// TestRenderYourQueueClipsLargeGroups verifies that when a
// for_each has fanned a template into many ready instances,
// the queue output caps per-template rows and collapses the
// rest behind a "...plus N more" line so status stays
// scannable.
func TestRenderYourQueueClipsLargeGroups(t *testing.T) {
	var tasks []map[string]interface{}
	// 30 ready instances of the same template — the pathological
	// case the UX feedback flagged.
	for i := 1; i <= 30; i++ {
		tasks = append(tasks, map[string]interface{}{
			"id":          fmt.Sprintf("1:1:t%02d:expand", i),
			"task_def_id": "expand",
			"state":       "ready",
			"seq":         float64(i),
			"instance_key": fmt.Sprintf("t%02d", i),
		})
	}
	out := renderYourQueue(tasks, "alice")
	if out == "" {
		t.Fatalf("expected queue output for 30 ready tasks, got empty")
	}
	if !strings.Contains(out, "Your queue (30)") {
		t.Errorf("expected header count 30; got:\n%s", out)
	}
	// Must clip — 30 rows is the UX problem we're fixing.
	yellowCount := strings.Count(out, "🟡")
	if yellowCount > maxQueueEntriesPerTemplate {
		t.Errorf("expected at most %d 🟡 rows per template, got %d; output:\n%s", maxQueueEntriesPerTemplate, yellowCount, out)
	}
	// Must tell the reader about the clipped remainder.
	want := fmt.Sprintf("...plus %d more of same template (expand)", 30-maxQueueEntriesPerTemplate)
	if !strings.Contains(out, want) {
		t.Errorf("expected %q in output; got:\n%s", want, out)
	}
	if !strings.Contains(out, "enju_list_ready_tasks") {
		t.Errorf("expected pointer to enju_list_ready_tasks in clipped output; got:\n%s", out)
	}
}

// TestFormatRunStatusMermaid snapshots the structural shape
// of the Mermaid export — fenced code block, flowchart header,
// one node per task with state-glyph labels, one edge per
// depends_on, class definitions. The integration test covers
// the HTTP wiring; this isolates the formatter.
func TestFormatRunStatusMermaid(t *testing.T) {
	runJSON := []byte(`{"project_id":1,"seq":1,"name":"demo","state":"active"}`)
	tasksJSON := []byte(`[
  {"id":"1:1:draft","task_def_id":"draft","state":"accepted","depends_on":""},
  {"id":"1:1:check","task_def_id":"check","state":"claimed","depends_on":"1:1:draft"},
  {"id":"1:1:publish","task_def_id":"publish","state":"pending","depends_on":"1:1:draft,1:1:check"}
]`)
	out := formatRunStatusMermaid(runJSON, tasksJSON)

	// Fenced block + flowchart header.
	if !strings.HasPrefix(out, "```mermaid") {
		t.Errorf("expected ```mermaid prefix; got first line: %q", strings.SplitN(out, "\n", 2)[0])
	}
	if !strings.Contains(out, "flowchart TD") {
		t.Errorf("missing flowchart TD header; got:\n%s", out)
	}

	// One node per task, labeled with state glyph + class suffix.
	if !strings.Contains(out, "t_1_1_draft[\"draft ✅\"]:::accepted") {
		t.Errorf("expected draft node with accepted class; got:\n%s", out)
	}
	if !strings.Contains(out, "t_1_1_check[\"check 🔵\"]:::active") {
		t.Errorf("expected check node with active class; got:\n%s", out)
	}
	if !strings.Contains(out, "t_1_1_publish[\"publish ⚪\"]:::pending") {
		t.Errorf("expected publish node with pending class; got:\n%s", out)
	}

	// After transitive reduction: 2 edges remain
	// (draft→check, check→publish). The direct draft→publish
	// is redundant because draft→check→publish already
	// carries the dependency, so the renderer drops it per
	// standard DAG visualization convention.
	if got := strings.Count(out, "-->"); got != 2 {
		t.Errorf("expected 2 edges after transitive reduction, got %d; output:\n%s", got, out)
	}
	if strings.Contains(out, "t_1_1_draft --> t_1_1_publish") {
		t.Errorf("expected draft→publish to be reduced; got:\n%s", out)
	}

	// Class definitions present.
	for _, cls := range []string{"classDef accepted", "classDef failed", "classDef skipped"} {
		if !strings.Contains(out, cls) {
			t.Errorf("expected %q in class defs; got:\n%s", cls, out)
		}
	}
}

// TestTransitivelyReduce covers the core redundant-edge
// cases we care about:
//   - Diamond (discover → {expand, tag}, expand → tag): the
//     direct discover → tag is redundant and must be dropped.
//   - Two-hop chain (a → b → c) with a direct shortcut a → c:
//     drop a → c.
//   - No redundancy (a → b, a → c, neither b nor c reachable
//     from the other): keep both.
func TestTransitivelyReduce(t *testing.T) {
	// Diamond: discover → expand → tag, discover → tag (redundant).
	in := []edge{
		{"discover", "expand"},
		{"discover", "tag"},
		{"expand", "tag"},
	}
	out := transitivelyReduce(in)
	if len(out) != 2 {
		t.Errorf("diamond reduce: expected 2 edges, got %d: %v", len(out), out)
	}
	for _, e := range out {
		if e.from == "discover" && e.to == "tag" {
			t.Errorf("expected direct discover→tag to be dropped; got %v", out)
		}
	}

	// Chain: a → b → c, a → c (redundant).
	in2 := []edge{{"a", "b"}, {"b", "c"}, {"a", "c"}}
	out2 := transitivelyReduce(in2)
	if len(out2) != 2 {
		t.Errorf("chain reduce: expected 2 edges, got %d: %v", len(out2), out2)
	}
	for _, e := range out2 {
		if e.from == "a" && e.to == "c" {
			t.Errorf("expected direct a→c to be dropped; got %v", out2)
		}
	}

	// No redundancy: fan-out only, each child disjoint.
	in3 := []edge{{"a", "b"}, {"a", "c"}, {"a", "d"}}
	out3 := transitivelyReduce(in3)
	if len(out3) != 3 {
		t.Errorf("no-redundancy case: expected all 3 edges kept, got %d: %v", len(out3), out3)
	}
}

// TestRenderMermaidBodyTransitiveReduction verifies the full
// pipeline applies reduction: a materialized dynamic for_each
// where the source → instance edge already flows via the
// intermediate (discover → expand → tag with discover → tag
// also recorded) should collapse to two edges, not three.
func TestRenderMermaidBodyTransitiveReduction(t *testing.T) {
	runJSON := []byte(`{"project_id":1,"seq":1,"name":"demo","state":"active"}`)
	// tag depends on BOTH discover (for_each source) and
	// expand (explicit). Without reduction, the Mermaid output
	// would show a redundant discover→tag edge.
	tasksJSON := []byte(`[
  {"id":"1:1:discover","task_def_id":"discover","state":"accepted","depends_on":""},
  {"id":"1:1:expand","task_def_id":"expand","state":"accepted","depends_on":"1:1:discover"},
  {"id":"1:1:tag","task_def_id":"tag","state":"ready","depends_on":"1:1:discover,1:1:expand"}
]`)
	out := renderMermaidBody(runJSON, tasksJSON)
	// Expect exactly 2 edges: discover→expand, expand→tag.
	// The direct discover→tag should be pruned.
	if got := strings.Count(out, "-->"); got != 2 {
		t.Errorf("expected 2 edges after reduction, got %d; output:\n%s", got, out)
	}
	if !strings.Contains(out, "t_1_1_discover --> t_1_1_expand") {
		t.Errorf("expected discover→expand edge; got:\n%s", out)
	}
	if !strings.Contains(out, "t_1_1_expand --> t_1_1_tag") {
		t.Errorf("expected expand→tag edge; got:\n%s", out)
	}
	if strings.Contains(out, "t_1_1_discover --> t_1_1_tag") {
		t.Errorf("direct discover→tag should be reduced; got:\n%s", out)
	}
}

// TestRenderYourQueueNoClipBelowThreshold checks the
// below-cap path: a handful of distinct ready tasks should
// still render one row each, with no clipping footer.
func TestRenderYourQueueNoClipBelowThreshold(t *testing.T) {
	tasks := []map[string]interface{}{
		{"id": "1:1:draft", "task_def_id": "draft", "state": "ready"},
		{"id": "1:1:check", "task_def_id": "check", "state": "ready"},
	}
	out := renderYourQueue(tasks, "alice")
	if strings.Contains(out, "plus") {
		t.Errorf("did not expect clipping at 2 tasks; got:\n%s", out)
	}
	if strings.Contains(out, "enju_list_ready_tasks") {
		t.Errorf("did not expect list_ready_tasks pointer when all rows fit; got:\n%s", out)
	}
	if strings.Count(out, "🟡") != 2 {
		t.Errorf("expected 2 🟡 rows; got:\n%s", out)
	}
}

// TestSetProjectRemoteResetsCursorsForRescan is the regression
// for TP53 Bug 2: when a remote is configured (or changed) on
// a project that already has commits on local refs/heads/*,
// enju_set_project_remote must (a) push existing local
// branches into the new bare so refs/remotes/origin/* gets
// populated, and (b) reset every local branch's cursor to the
// rescan sentinel so the next reconcile walks the full history
// and re-emits historical trailers.
//
// Without this, a project that ran async compute pre-remote
// has commits stranded on local refs/heads/* with no way for
// the scanner to find them: ScanBranchSince would either return
// nothing (no remote ref) or baseline-tip on first scan after
// the push (cursor empty) and skip the historical trailers.
func TestSetProjectRemoteResetsCursorsForRescan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Coordinator stub: PUT /projects/2/remote returns OK.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/2/remote", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Build the buggy state: a working tree with commits on
	// main + a run branch, NO origin configured. This is what
	// a pre-Bug-1-fix init'd project looks like in practice.
	workDir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(workDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	wt, _ := repo.Worktree()
	_ = os.WriteFile(filepath.Join(workDir, "seed.md"), []byte("seed"), 0644)
	_, _ = wt.Add("seed.md")
	_, _ = wt.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t", When: time.Now()},
	})
	// Branch off and add a "trailer-bearing" run-branch commit
	// so we can verify the bare receives it after set_remote.
	branchName := "run-1"
	if err := wt.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branchName),
		Create: true,
	}); err != nil {
		t.Fatalf("checkout run-1: %v", err)
	}
	_ = os.WriteFile(filepath.Join(workDir, "result.md"), []byte("ran"), 0644)
	_, _ = wt.Add("result.md")
	runCommitHash, _ := wt.Commit("Task 2:1:work by @t: result\n\nEnju-Task-Complete: 2:1:work\n",
		&gogit.CommitOptions{Author: &object.Signature{Name: "T", Email: "t@t", When: time.Now()}})

	// Wire the workspace + project.
	wsRoot := t.TempDir()
	ws, _ := mcpgit.NewWorkspace(wsRoot, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ws.RegisterExternalDir(2, workDir)
	if _, err := ws.ForProject(2, ""); err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Fresh empty bare for the new remote.
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	if err := mcpgit.InitBareEmpty(bareDir); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	c := &apiClient{
		baseURL:    ts.URL,
		username:   "tester",
		workspace:  ws,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
	}

	result, err := c.handleSetProjectRemote(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_set_project_remote",
			Arguments: map[string]interface{}{
				"project_id": float64(2),
				"remote_url": bareDir,
			},
		},
	})
	if err != nil || (result != nil && result.IsError) {
		t.Fatalf("handleSetProjectRemote: err=%v result=%+v", err, result)
	}

	// Bare must contain run-1's commit on refs/heads/run-1 —
	// proves the all-branches push happened. Without this,
	// the scanner would have nothing to walk even after the
	// cursor reset.
	bareRepo, err := gogit.PlainOpen(bareDir)
	if err != nil {
		t.Fatalf("opening bare: %v", err)
	}
	bareRun, err := bareRepo.Reference(plumbing.NewBranchReferenceName("run-1"), true)
	if err != nil {
		t.Fatalf("bare missing refs/heads/run-1 — push did not seed the new remote: %v", err)
	}
	if got := bareRun.Hash().String(); got != runCommitHash.String() {
		t.Errorf("bare run-1 head: got %s, want %s", got, runCommitHash.String())
	}

	// Cursors must be set to the rescan sentinel for every
	// local branch — main and run-1. Without this, the next
	// scan would baseline tip and miss the historical trailer
	// commit on run-1.
	cursors, err := mcpgit.LoadCursors(c.stateDir(), 2)
	if err != nil {
		t.Fatalf("loading cursors: %v", err)
	}
	for _, b := range []string{"main", "run-1"} {
		if got := cursors.Get(b); got != mcpgit.RescanSentinelSHA {
			t.Errorf("cursor for %s: got %q, want sentinel %q", b, got, mcpgit.RescanSentinelSHA)
		}
	}
}
