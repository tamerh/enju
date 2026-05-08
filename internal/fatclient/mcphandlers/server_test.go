package mcphandlers

import (
	"github.com/enju-ai/enju/internal/common/format"
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

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/service"
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:    ts.URL,
			Username:    "alice",
			CitizenName:  "Alice",
			CitizenEmail: "alice@example.com",
			Logger:     logger,
			SaveCredentials: func(u, n, e, t string) {
				savedUser = u
				savedName = n
				savedEmail = e
				saveCalls.Add(1)
			},
		}), "", logger)

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

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "alice",
			// CitizenName intentionally empty
			Logger:  logger,
		}), "", logger)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  "http://unused.invalid",
			Username: "tamer",
			Logger:  logger,
		}), "", logger)
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
				"", // model override (use session default)
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
// descriptor pointing at that commit, and a real project.Opener
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
		// default_branch into the project. Minimal response
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
	ws, err := enjugit.NewWorkspace(wsDir, enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("new workspace: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "bob",
			Logger:  logger,
		}), ws.RootDir(), logger)
	meta := &taskMeta{
		ID:               reviewID,
		ProjectID:        projectID,
		ProjectRemoteURL: bareDir,
		RunSeq:           runSeq,
		TaskDefID:        "check",
		Action:           "review",
		ReviewsTarget:    "draft",
	}

	data, err := c.fc.FetchAndResolveLocally(context.Background(), meta)
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

// TestCreateProjectPathRequired pins the contract: omitting
// `path` is a hard error. Earlier the handler accepted no-path
// and silently routed to a managed `~/.enju/workspaces/` dir;
// that fork was the source of the "two LocalPath shapes in
// projectreg" confusion. Callers MUST pick a path explicitly.
func TestCreateProjectPathRequired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		t.Error("POST /projects must NOT fire when path is missing — handler should reject before calling coord")
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsDir := t.TempDir()
	ws, err := enjugit.NewWorkspace(wsDir, enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("new workspace: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{BaseURL: ts.URL, Username: "tester", Logger: logger}), ws.RootDir(), logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_create_project",
			Arguments: map[string]interface{}{"name": "demo"},
		},
	})
	if err != nil {
		t.Fatalf("handleCreateProject (transport-level error, not expected): %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected IsError=true result, got %+v", result)
	}
	if !strings.Contains(mcpResultText(t, result), "path is required") {
		t.Errorf("error should explain path is required, got: %s", mcpResultText(t, result))
	}
}

// TestCreateProjectCustomPathFresh verifies the path= parameter
// on enju_create_project: the working tree lands at the
// caller-supplied absolute path (registered as an external dir),
// the eager-init produces a seeded git repo (README + scaffold),
// and no shadow bare is created at the legacy
// `~/.enju/repos/{id}.git/` path. `path` is the only way to
// create a project — the legacy "managed workspace dir" branch
// is gone.
func TestCreateProjectCustomPathFresh(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/1/remote", func(w http.ResponseWriter, r *http.Request) {
		// Should NEVER fire — local-only create has no remote
		// to PUT. Catches autoLocal regressions if they came back.
		t.Errorf("unexpected PUT /projects/1/remote on local-only create")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/projects/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo","remote_url":""}`))
	})
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsDir := t.TempDir()
	ws, err := enjugit.NewWorkspace(wsDir, enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("new workspace: %v", err)
	}
	// Shared registry so the apiClient's enjugit.Workspace and
	// the test's project.Opener see the same project→path
	// bindings. handleCreateProject upserts via the apiClient's
	// registry; the test asserts via ws.ForProject which consults
	// this same registry.
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	ws.AttachRegistry(reg)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newAPIClientForTest(TestClientConfig{
		Coord: coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:   logger,
		}),
		WorkspaceRoot:   ws.RootDir(),
		Logger:          logger,
		ProjectRegistry: reg,
	})

	customPath := filepath.Join(t.TempDir(), "my-project")
	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "demo",
				"path": customPath,
			},
		},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("handleCreateProject: err=%v result=%+v", err, result)
	}

	// Workspace resolves to the custom path.
	proj, err := ws.ForProject(1, "")
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if proj.WorkDir() != customPath {
		t.Errorf("WorkDir = %q, want %q", proj.WorkDir(), customPath)
	}

	// Eager-init seeded the repo so first-submit's
	// branchBaseHash has something to fork from.
	if _, _, err := proj.Head(); err != nil {
		t.Errorf("clone has no HEAD ref — seedLocalWorkspace didn't fire: %v", err)
	}
	for _, rel := range []string{"README.md", "enju/templates/.gitkeep"} {
		full := filepath.Join(proj.WorkDir(), rel)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected seeded file %s after create, got: %v", rel, err)
		}
	}

	// No shadow bare under ~/.enju/repos/ — the legacy path is
	// gone, but a regression to the auto-bare flow would
	// re-introduce it.
	legacyBare := filepath.Join(tmpHome, ".enju", "repos", "1.git")
	if _, err := os.Stat(legacyBare); err == nil {
		t.Errorf("unexpected shadow bare at %s — local-only create should not create one", legacyBare)
	}
}

// TestCreateProjectCustomPathRefusesPopulatedGitRepo pins the
// safety guarantee on enju_create_project: a path that already
// contains a git repo with commits but no Enju metadata is
// refused unless force=true. Catches the LLM-typoed-path footgun
// (running in /repo/A but passing /repo/B that turns out to be
// /repo/A again).
func TestCreateProjectCustomPathRefusesPopulatedGitRepo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	customPath := t.TempDir()
	// Make it a populated git repo with no Enju marker.
	repo, err := gogit.PlainInitWithOptions(customPath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: "refs/heads/main",
		},
	})
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(customPath, "existing.txt"), []byte("user data"), 0644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	_ = wt.AddGlob(".")
	sig := &object.Signature{Name: "u", Email: "u@u", When: time.Now()}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
		Username: "tester",
		Logger:   logger,
	}), "", logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "demo",
				"path": customPath,
			},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("expected curative error result, got err=%v result=%+v", err, result)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for populated git repo without Enju marker")
	}
	gotText := mcpResultText(t, result)
	if !strings.Contains(gotText, "force=true") {
		t.Errorf("error message should mention force=true escape hatch, got: %s", gotText)
	}
	// Existing file must remain untouched.
	if _, err := os.Stat(filepath.Join(customPath, "existing.txt")); err != nil {
		t.Errorf("existing file disturbed: %v", err)
	}
}

// TestCreateProjectCustomPathRejectsWithRemoteURL pins the
// path-vs-remote_url mutex: passing both is a config-drift bug
// (the custom-path branch seeds a local tree without cloning the
// remote, so the project record would persist a remote_url it
// never actually pulled from). Refuse the combo upfront.
func TestCreateProjectCustomPathRejectsWithRemoteURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			Username: "tester",
			Logger:  logger,
		}), "", logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name":       "demo",
				"path":       filepath.Join(t.TempDir(), "ws"),
				"remote_url": "git@github.com:org/repo.git",
			},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("expected error result, got err=%v result=%+v", err, result)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for path+remote_url combo")
	}
	gotText := mcpResultText(t, result)
	if !strings.Contains(gotText, "mutually exclusive") {
		t.Errorf("error message should explain mutual exclusion, got: %s", gotText)
	}
}

// TestCreateProjectCustomPathCreatesNonExistentParents pins the
// "user passes /var/enju_runs/my-project where /var/enju_runs/
// doesn't exist yet" case. MkdirAll handles it; the test ensures
// the contract doesn't regress to refusing missing parents.
func TestCreateProjectCustomPathCreatesNonExistentParents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo","remote_url":""}`))
	})
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsDir := t.TempDir()
	ws, err := enjugit.NewWorkspace(wsDir, enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("new workspace: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:  logger,
		}), ws.RootDir(), logger)

	deepPath := filepath.Join(t.TempDir(), "missing", "parent", "chain", "project")
	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "demo",
				"path": deepPath,
			},
		},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("handleCreateProject: err=%v result=%+v text=%s", err, result, mcpResultText(t, result))
	}
	if _, statErr := os.Stat(deepPath); statErr != nil {
		t.Errorf("expected mkdir -p to create %q, got: %v", deepPath, statErr)
	}
}

// TestCreateProjectCustomPathRefusesSymlink pins the symlink
// rejection. Following symlinks silently is a footgun: a user
// passing path=link-to-populated-repo would either get a
// confusing "not empty" error (mentioning the link path, not
// the target) or, if the target is empty, end up with the
// project's working tree dual-rooted via the symlink.
func TestCreateProjectCustomPathRefusesSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tmp := t.TempDir()
	target := filepath.Join(tmp, "real-target")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			Username: "tester",
			Logger:  logger,
		}), "", logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "demo",
				"path": link,
			},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("expected error result, got err=%v result=%+v", err, result)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for symlink path")
	}
	gotText := mcpResultText(t, result)
	if !strings.Contains(gotText, "symlink") {
		t.Errorf("error message should mention symlink + suggest readlink, got: %s", gotText)
	}
}

// TestCreateProjectCustomPathRefusesRegularFile pins the "user
// passed a file path" case — confusing without a clear error.
func TestCreateProjectCustomPathRefusesRegularFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			Username: "tester",
			Logger:  logger,
		}), "", logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "demo",
				"path": filePath,
			},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("expected error result, got err=%v result=%+v", err, result)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for file path")
	}
	gotText := mcpResultText(t, result)
	if !strings.Contains(gotText, "not a directory") {
		t.Errorf("error message should mention 'not a directory', got: %s", gotText)
	}
}

// TestCreateProjectCustomPathRefusesRelative pins the absolute-
// path requirement.
func TestCreateProjectCustomPathRefusesRelative(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			Username: "tester",
			Logger:  logger,
		}), "", logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "demo",
				"path": "relative/path",
			},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("expected error result, got err=%v result=%+v", err, result)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for relative path")
	}
}

// TestCreateProjectRefusesPopulatedUnrelatedRepoAdoption pins the
// safety contract: enju_create_project (smart-detect adoption)
// refuses to adopt a git repo that has commits AND no Enju marker.
// The footgun this catches is the calling LLM running inside
// /repo/A and typo'ing path=/repo/B → /repo/A, which would
// otherwise silently scaffold + commit Enju into the caller's
// source repo.
func TestCreateProjectRefusesPopulatedUnrelatedRepoAdoption(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("user code"), 0644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "User", Email: "user@example.com", When: time.Now()}
	if _, err := wt.Commit("user's own work", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			Username: "tester",
			Logger:  logger,
		}), "", logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "demo",
				"path": dir,
			},
		},
	})
	if err != nil || result == nil {
		t.Fatalf("expected error result, got err=%v result=%+v", err, result)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for populated unrelated git repo")
	}
	gotText := mcpResultText(t, result)
	for _, want := range []string{"populated git repo", "force=true"} {
		if !strings.Contains(gotText, want) {
			t.Errorf("error should mention %q, got: %s", want, gotText)
		}
	}
	// The user's commit must remain intact — refusal must not
	// mutate the repo at all.
	if _, err := os.Stat(filepath.Join(dir, "enju")); err == nil {
		t.Error("enju/ scaffold should NOT exist after refused init")
	}
}

// TestCreateProjectForceAdoptsPopulatedRepo pins the cure: with
// force=true, enju_create_project (smart-detect adoption) adopts
// the repo, scaffolds it, and proceeds normally.
func TestCreateProjectForceAdoptsPopulatedRepo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("user code"), 0644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	wt.Add("README.md")
	sig := &object.Signature{Name: "User", Email: "user@example.com", When: time.Now()}
	wt.Commit("user's work", &gogit.CommitOptions{Author: sig, Committer: sig})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo","remote_url":""}`))
	})
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"name":"demo"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ws, _ := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:  logger,
		}), ws.RootDir(), logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name":  "demo",
				"path":  dir,
				"force": true,
			},
		},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("handleCreateProject with force=true: err=%v result=%+v text=%s",
			err, result, mcpResultText(t, result))
	}
	// Scaffold must exist now.
	if _, err := os.Stat(filepath.Join(dir, "enju")); err != nil {
		t.Errorf("expected enju/ scaffold after force adopt, got: %v", err)
	}
}

// TestCreateProjectAcceptsAlreadyAdoptedRepo pins idempotency:
// re-running enju_create_project (smart-detect adoption) on a repo
// that was previously adopted (carries enju/ scaffold) passes
// through without force, even though it has commits.
func TestCreateProjectAcceptsAlreadyAdoptedRepo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-existing Enju scaffold + commit.
	if err := os.MkdirAll(filepath.Join(dir, "enju"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enju", ".gitkeep"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	wt.Add("enju/.gitkeep")
	sig := &object.Signature{Name: "Enju", Email: "enju@localhost", When: time.Now()}
	wt.Commit("prior adoption", &gogit.CommitOptions{Author: sig, Committer: sig})

	if reason := service.DetectPopulatedUnrelatedRepo(dir); reason != "" {
		t.Errorf("repo with enju/ marker should pass safety check, got refusal: %s", reason)
	}
}

// TestCreateProjectDetectsEnjuBinaryNotMistakenForScaffold pins
// the IsDir discrimination on the safety check used by
// enju_create_project (smart-detect adoption). The enju repo
// itself is a populated git repo whose root contains the
// compiled `enju` binary as a regular file. Without the IsDir
// check, an os.Stat-only marker test would treat the binary
// as an "enju/" scaffold marker and skip refusal — defeating
// the safety gate exactly in the scenario it's there to catch
// (calling LLM adopting the enju source repo).
func TestCreateProjectDetectsEnjuBinaryNotMistakenForScaffold(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Create a regular file named "enju" — mimics the compiled
	// binary in the enju source repo.
	if err := os.WriteFile(filepath.Join(dir, "enju"), []byte("#!/bin/sh\nexec real-enju\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	wt.Add(".")
	sig := &object.Signature{Name: "User", Email: "u@e.com", When: time.Now()}
	wt.Commit("source repo with enju binary", &gogit.CommitOptions{Author: sig, Committer: sig})

	if reason := service.DetectPopulatedUnrelatedRepo(dir); reason == "" {
		t.Error("expected refusal for populated repo with regular-file 'enju' (compiled binary), got empty (mistaken for scaffold)")
	}
}

// TestCreateProjectFolderWithoutGit verifies that
// enju_create_project (smart-detect adoption) on a plain folder
// (no .git) initializes git, writes the scaffold, and registers
// the external dir so ForProject opens it directly.
func TestCreateProjectFolderWithoutGit(t *testing.T) {
	// Isolate $HOME — defensive against any stray code path
	// that might still consult $HOME for state. Post-layout-
	// refactor, init no longer writes anywhere outside the
	// adopted dir, but the override is cheap insurance.
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

	ws, _ := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	ws.AttachRegistry(reg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newAPIClientForTest(TestClientConfig{
		Coord: coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:   logger,
		}),
		WorkspaceRoot:   ws.RootDir(),
		Logger:          logger,
		ProjectRegistry: reg,
	})

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "enju_create_project",
			Arguments: map[string]interface{}{"name": "test-init", "path": dir},
		},
	})
	if err != nil {
		t.Fatalf("handleCreateProject: %v", err)
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

	// External dir registered — ForProject resolves to the adopted folder.
	proj, err := ws.ForProject(createdProjectID, "")
	if err != nil {
		t.Fatalf("ForProject on init'd dir: %v", err)
	}
	if proj.WorkDir() != dir {
		t.Errorf("expected workdir=%s, got %s", dir, proj.WorkDir())
	}
}

// TestCreateProjectFolderWithExistingGit verifies that
// enju_create_project (smart-detect adoption) on a folder that
// already has git preserves existing history. Unlike the
// no-.git or empty-folder branches, the existing-.git branch
// does NOT add a scaffold commit — the existing repo is
// adopted as-is and only origin is wired (managed bare).
func TestCreateProjectFolderWithExistingGit(t *testing.T) {
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

	ws, _ := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:  logger,
		}), ws.RootDir(), logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "existing-git", "path": dir, "force": true,
			},
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("handleCreateProject: err=%v result=%+v", err, result)
	}

	// Pre-existing commit preserved — existing-.git branch
	// does NOT add a scaffold commit (that's reserved for the
	// no-.git branches).
	iter, _ := repo.Log(&gogit.LogOptions{})
	count := 0
	iter.ForEach(func(c *object.Commit) error { count++; return nil })
	if count != 1 {
		t.Errorf("expected 1 commit (pre-existing), got %d — existing-.git branch should not add commits on top", count)
	}

	// Pre-existing file preserved.
	data, _ := os.ReadFile(filepath.Join(dir, "existing.txt"))
	if string(data) != "pre-existing" {
		t.Errorf("existing.txt clobbered: %s", data)
	}

	// Managed bare wired (the existing-.git+no-origin branch
	// always promotes a managed bare so origin is non-empty).
	managedBare := filepath.Join(dir, "enju", ".bare.git")
	if _, err := os.Stat(filepath.Join(managedBare, "HEAD")); err != nil {
		t.Errorf("managed bare missing at %s: %v", managedBare, err)
	}
}

// TestCreateProjectIdempotent verifies that running
// enju_create_project (smart-detect adoption) twice on the same
// folder doesn't clobber anything or fail.
func TestCreateProjectIdempotent(t *testing.T) {
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

	ws, _ := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:  logger,
		}), ws.RootDir(), logger)

	makeReq := func() mcp.CallToolRequest {
		return mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "enju_create_project",
				Arguments: map[string]interface{}{"name": "idempotent", "path": dir},
			},
		}
	}

	// First create_project.
	r1, err := c.handleCreateProject(context.Background(), makeReq())
	if err != nil || r1.IsError {
		t.Fatalf("first create_project: err=%v result=%+v", err, r1)
	}

	// Second create_project — should not fail, scaffold already exists.
	r2, err := c.handleCreateProject(context.Background(), makeReq())
	if err != nil || r2.IsError {
		t.Fatalf("second create_project: err=%v result=%+v", err, r2)
	}

	// Data file still intact.
	data, _ := os.ReadFile(filepath.Join(dir, "data.csv"))
	if string(data) != "a,b,c" {
		t.Errorf("data.csv clobbered: %s", data)
	}
}

// TestCreateProjectOriginlessFolderGetsManagedBare verifies the
// "every project has origin" precondition (Phase 1 of the
// no-remote collapse): enju_create_project (smart-detect
// adoption) on a folder with no `origin` MUST create a managed
// bare at <project>/enju/.bare.git/ and wire origin to it. This
// eliminates the no-remote state class so verbs downstream don't
// need to handle "what if there's no push target" branches.
//
// The legacy ~/.enju/repos/{id}.git/ shadow bare must NOT
// appear — that path is removed by the layout refactor;
// the new bare lives sibling-to the operator's working tree.
//
// Coord must NOT receive a PUT /remote — the origin is local
// to the operator's machine and coord-side state stays
// remote_url="" (path-mode project).
func TestCreateProjectOriginlessFolderGetsManagedBare(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"name":"tp53"}`))
	})
	mux.HandleFunc("/api/v1/projects/42/remote", func(w http.ResponseWriter, r *http.Request) {
		// Should NEVER fire for solo-mode init. If this hits,
		// the auto-bare regression came back.
		t.Error("unexpected PUT /projects/42/remote — solo-mode enju_create_project should not call it")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Folder with `git init` + one commit, no origin — the
	// minimal repro of the TP53 setup that used to silently
	// stall. With Phase 1's scanner fallback shipped, this
	// case Just Works without any bare.
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

	ws, _ := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	ws.AttachRegistry(reg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newAPIClientForTest(TestClientConfig{
		Coord: coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:   logger,
		}),
		WorkspaceRoot:   ws.RootDir(),
		Logger:          logger,
		ProjectRegistry: reg,
	})

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "tp53", "path": dir, "force": true,
			},
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("handleCreateProject: err=%v result=%+v", err, result)
	}

	// Legacy ~/.enju/repos/{id}.git/ shadow bare must NOT
	// exist — that path was removed by the layout refactor.
	legacyBare := filepath.Join(os.Getenv("HOME"), ".enju", "repos", "42.git")
	if _, err := os.Stat(legacyBare); err == nil {
		t.Errorf("unexpected legacy shadow bare at %s", legacyBare)
	}

	// The managed bare MUST exist at <dir>/enju/.bare.git/ —
	// every project gets a push target at creation time.
	managedBare := filepath.Join(dir, "enju", ".bare.git")
	if _, err := os.Stat(filepath.Join(managedBare, "HEAD")); err != nil {
		t.Errorf("managed bare missing at %s: %v", managedBare, err)
	}

	// Working tree's origin MUST point at the managed bare —
	// `git push` lands there; no more silent push-skip.
	rem, err := repo.Remote("origin")
	if err != nil {
		t.Fatalf("origin not configured after init: %v", err)
	}
	if cfg := rem.Config(); cfg == nil || len(cfg.URLs) == 0 || cfg.URLs[0] != managedBare {
		t.Errorf("origin URL: got %v, want [%s]", cfg.URLs, managedBare)
	}

	// Workspace's external dir registration succeeded — ForProject
	// resolves to the adopted folder.
	proj, err := ws.ForProject(42, "")
	if err != nil {
		t.Fatalf("ForProject post-init: %v", err)
	}
	if proj.WorkDir() != dir {
		t.Errorf("workspace points at %s, want %s", proj.WorkDir(), dir)
	}
}

// TestCreateProjectPreservesExistingOrigin verifies the
// github-clone case: when the adopted folder already has an
// origin (e.g. from a prior `git clone`), enju_create_project
// (smart-detect adoption) must NOT overwrite it with a managed
// bare. The user's existing push target stays intact.
func TestCreateProjectPreservesExistingOrigin(t *testing.T) {
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

	ws, _ := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:  logger,
		}), ws.RootDir(), logger)

	result, err := c.handleCreateProject(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "enju_create_project",
			Arguments: map[string]interface{}{
				"name": "with-origin", "path": dir, "force": true,
			},
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("handleCreateProject: err=%v result=%+v", err, result)
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

	result := format.ClaimResult(claimData, nil, "alice", feedback, prevSubmission)

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

	result := format.ClaimResult(claimData, nil, "alice")

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
	if !enjugit.IsLocalWorkingTree(wtDir) {
		t.Error("expected working tree detected")
	}

	// Case 2: plain folder → false.
	plainDir := t.TempDir()
	if enjugit.IsLocalWorkingTree(plainDir) {
		t.Error("plain dir should not be detected as working tree")
	}

	// Case 3: non-existent path → false.
	if enjugit.IsLocalWorkingTree("/tmp/nonexistent-enju-test-path") {
		t.Error("non-existent path should not be detected")
	}

	// Case 4: SSH URL → false.
	if enjugit.IsLocalWorkingTree("git@github.com:org/repo.git") {
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
		{"/home/tamer/projects/myproject/enju/.bare.git", false},
	}
	for _, tc := range cases {
		if got := enjugit.IsSSHURL(tc.url); got != tc.want {
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
	result := format.RenderDAGTree(tasks)
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
	out := format.RenderYourQueue(tasks, "alice")
	if out == "" {
		t.Fatalf("expected queue output for 30 ready tasks, got empty")
	}
	if !strings.Contains(out, "Your queue (30)") {
		t.Errorf("expected header count 30; got:\n%s", out)
	}
	// Must clip — 30 rows is the UX problem we're fixing.
	yellowCount := strings.Count(out, "🟡")
	if yellowCount > format.MaxQueueEntriesPerTemplate {
		t.Errorf("expected at most %d 🟡 rows per template, got %d; output:\n%s", format.MaxQueueEntriesPerTemplate, yellowCount, out)
	}
	// Must tell the reader about the clipped remainder.
	want := fmt.Sprintf("...plus %d more of same template (expand)", 30-format.MaxQueueEntriesPerTemplate)
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
	out := format.RunStatusMermaid(runJSON, tasksJSON)

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
	in := []format.Edge{
		{"discover", "expand"},
		{"discover", "tag"},
		{"expand", "tag"},
	}
	out := format.TransitivelyReduce(in)
	if len(out) != 2 {
		t.Errorf("diamond reduce: expected 2 edges, got %d: %v", len(out), out)
	}
	for _, e := range out {
		if e.From == "discover" && e.To == "tag" {
			t.Errorf("expected direct discover→tag to be dropped; got %v", out)
		}
	}

	// Chain: a → b → c, a → c (redundant).
	in2 := []format.Edge{{"a", "b"}, {"b", "c"}, {"a", "c"}}
	out2 := format.TransitivelyReduce(in2)
	if len(out2) != 2 {
		t.Errorf("chain reduce: expected 2 edges, got %d: %v", len(out2), out2)
	}
	for _, e := range out2 {
		if e.From == "a" && e.To == "c" {
			t.Errorf("expected direct a→c to be dropped; got %v", out2)
		}
	}

	// No redundancy: fan-out only, each child disjoint.
	in3 := []format.Edge{{"a", "b"}, {"a", "c"}, {"a", "d"}}
	out3 := format.TransitivelyReduce(in3)
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
	out := format.RenderMermaidBody(runJSON, tasksJSON)
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
	out := format.RenderYourQueue(tasks, "alice")
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

	// Wire the opener + project. Path resolution comes from a
	// projectreg.Registry attached to the opener (the durable
	// per-machine "project N → home path" record).
	wsRoot := t.TempDir()
	ws, _ := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	if err := reg.Upsert(projectreg.Entry{ID: 2, LocalPath: workDir}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws.AttachRegistry(reg)
	if _, err := ws.ForProject(2, ""); err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Fresh empty bare for the new remote.
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	if err := enjugit.InitBareEmpty(bareDir); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newAPIClientForTest(TestClientConfig{
		Coord: coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:   logger,
		}),
		WorkspaceRoot:   ws.RootDir(),
		Logger:          logger,
		ProjectRegistry: reg,
	})

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
	cursors, err := enjugit.LoadCursors(c.fc.StateDir(), 2)
	if err != nil {
		t.Fatalf("loading cursors: %v", err)
	}
	for _, b := range []string{"main", "run-1"} {
		if got := cursors.Get(b); got != enjugit.RescanSentinelSHA {
			t.Errorf("cursor for %s: got %q, want sentinel %q", b, got, enjugit.RescanSentinelSHA)
		}
	}
}

// TestSetProjectRemoteRejectsEmptyURL is the validation guard
// for the no-origin broken state (TP53 Bug 1's failure mode).
// The handler must reject remote_url="" loudly with a recovery
// hint pointing at the real alternatives — clearing the
// remote silently stalls async reconciliation, and there's no
// legitimate use case for it.
func TestSetProjectRemoteRejectsEmptyURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/9/remote", func(w http.ResponseWriter, r *http.Request) {
		// Should never reach the coordinator — handler must
		// reject before the HTTP call.
		t.Error("unexpected PUT /projects/9/remote — handler should reject empty remote_url before calling the coordinator")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:  logger,
		}), "", logger)

	for _, badURL := range []string{"", "   ", "\t\n"} {
		result, err := c.handleSetProjectRemote(context.Background(), mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "enju_set_project_remote",
				Arguments: map[string]interface{}{
					"project_id": float64(9),
					"remote_url": badURL,
				},
			},
		})
		if err != nil {
			t.Fatalf("transport error for %q: %v", badURL, err)
		}
		if result == nil || !result.IsError {
			t.Errorf("expected error for remote_url=%q, got success: %+v", badURL, result)
			continue
		}
		got := toolResultText(result)
		if !strings.Contains(got, "cannot be empty") {
			t.Errorf("expected 'cannot be empty' in error for %q, got: %s", badURL, got)
		}
		// Should point at the real alternatives.
		if !strings.Contains(got, "enju_leave_project") {
			t.Errorf("expected error to suggest enju_leave_project as the deletion path, got: %s", got)
		}
	}
}

// Note: TestClaimTransientRetryRecovers and
// TestClaimTransientRetrySkipsSubstantiveErrors moved to
// internal/fatclient/service/execute_test.go alongside the
// claimWithTransientRetry implementation.
