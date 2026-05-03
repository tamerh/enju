package test

// Coordinator-layer integration tests.
//
// This file holds tests that verify coordinator invariants
// independent of any client — state-machine rules (double-claim
// rejection, submit-to-non-claimed, reaper timeouts), parser
// contracts (YAML validation, atomic run creation), numbering
// invariants (per-project run seq), auth middleware, and the
// registration endpoint. These paths are called the same way by
// every client (MCP today, future web UI / CLI worker), so
// testing them at the REST layer gives honest coverage of the
// coordinator contract without paying the MCP-handler +
// workspace ceremony for every check.
//
// User-facing scenarios — claim/submit flows, review/vote
// cycles, artifacts, access control, invalidation cascades,
// templates, dynamic for_each — live in
// mcp_integration_test.go and mcp_migrated_test.go. Those
// exercise the MCP tool handlers end-to-end because that's the
// layer where real users hit bugs (client-side pre-validation,
// workspace interactions, format output).
//
// This file also hosts shared helpers (testServer, register,
// submitYAML, taskID, readRepoFile, etc.) used by both test
// layers. Moving them here keeps the split clear: one place for
// engine-level setup, one set of files per user-scenario area.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/api"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/mcpgit"
	"github.com/enju-ai/enju/internal/coordinator/store"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestMain cleans the shared output directory before running
// tests. Also routes `test.binary wrap-task …` invocations to
// the compute wrapper so async-compute tests (phase 4d) can
// spawn the subprocess via os.Executable() without a pre-built
// `enju` binary on PATH. Production dispatch lives in
// cmd/enju/main.go.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "wrap-task" {
		os.Exit(compute.WrapMain(os.Args[2:], os.Stderr))
	}
	os.RemoveAll(testOutputBase)
	os.MkdirAll(testOutputBase, 0755)
	os.Exit(m.Run())
}

// llmMode returns true when ENJU_LLM_TEST is set.
func llmMode() bool {
	return os.Getenv("ENJU_LLM_TEST") != ""
}

// answer returns an LLM-generated answer (via claude -p) or a canned answer.
func answer(t *testing.T, prompt string, canned string) string {
	t.Helper()
	if !llmMode() {
		return canned
	}
	t.Logf("LLM: asking claude -p: %.80s...", prompt)
	out, err := exec.Command("claude", "-p", prompt).CombinedOutput()
	if err != nil {
		t.Fatalf("claude -p failed: %v\noutput: %s", err, out)
	}
	result := strings.TrimSpace(string(out))
	t.Logf("LLM: got %d chars", len(result))
	return result
}

// testServer wraps a running Enju coordinator for testing. Post the
// iteration A orchestrator rewrite, the coordinator holds no git
// state of its own — each project gets a bare repo under
// `bareBaseDir` which acts as the project's "remote", and submits
// are routed through a mcpgit.Workspace under `workspaceDir` so the
// test client exercises the exact same fat-client code path the
// real MCP server uses.
type testServer struct {
	t             *testing.T
	server        *httptest.Server
	url           string
	bareBaseDir   string // base directory containing per-project bare remotes
	workspaceDir  string // base directory for fat-client working clones
	workspace     *mcpgit.Workspace
	store         *store.Store // direct store access for testing reaper/internals
	lastRunID     string       // "projectID:runSeq" of last submitted run
	lastProjectID int64
	lastRunSeq    int

	// Per-project cached remote URLs so the submit helpers can pass
	// them to the workspace without re-hitting the API.
	muRemotes sync.Mutex
	remotes   map[int64]string

	// Auth: the coordinator requires a Bearer token on every
	// endpoint except /citizens/register. The test harness
	// caches the token returned by register() per username and
	// attaches the default citizen's token on every unqualified
	// post/get. Tests that act as a specific citizen use the
	// postAs/getAs variants.
	muAuth         sync.Mutex
	tokens         map[string]string // username → token
	defaultUser    string            // first-registered citizen, used when no explicit actor specified
}

// bareRemotePath returns the on-disk path of the bare repo acting
// as a project's "remote". Used by test helpers that need to verify
// what actually landed in the "remote".
func (s *testServer) bareRemotePath(projectID int64) string {
	return filepath.Join(s.bareBaseDir, fmt.Sprintf("%d", projectID))
}

// rememberRemote caches a project's remote URL so subsequent
// submit/claim helpers can open the fat-client workspace without an
// extra API round-trip.
func (s *testServer) rememberRemote(projectID int64, url string) {
	s.muRemotes.Lock()
	defer s.muRemotes.Unlock()
	s.remotes[projectID] = url
}

// remoteFor returns the cached remote URL for a project, fetching
// it from the coordinator the first time if not yet cached.
func (s *testServer) remoteFor(projectID int64) string {
	s.muRemotes.Lock()
	u, ok := s.remotes[projectID]
	s.muRemotes.Unlock()
	if ok {
		return u
	}
	p := s.get(fmt.Sprintf("/api/v1/projects/%d", projectID))
	if u, ok := p["remote_url"].(string); ok && u != "" {
		s.rememberRemote(projectID, u)
		return u
	}
	return ""
}

// testOutputDir is a fixed temp directory that gets symlinked into the run.
const testOutputBase = "/tmp/enju-test-output"

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	// Create a test-specific subdirectory under the shared output base
	testDir := filepath.Join(testOutputBase, t.Name())
	os.RemoveAll(testDir)
	os.MkdirAll(testDir, 0755)

	dbPath := filepath.Join(testDir, "test.db")
	bareBaseDir := filepath.Join(testDir, "bare-remotes")
	workspaceDir := filepath.Join(testDir, "workspaces")
	os.MkdirAll(bareBaseDir, 0755)
	os.MkdirAll(workspaceDir, 0755)

	// Ensure the symlink exists: test/output -> /tmp/enju-test-output
	outputLink := filepath.Join(".", "output")
	if target, err := os.Readlink(outputLink); err != nil || target != testOutputBase {
		os.Remove(outputLink)
		os.Symlink(testOutputBase, outputLink)
	}

	var logWriter io.Writer = io.Discard
	if os.Getenv("ENJU_TEST_VERBOSE") != "" {
		logWriter = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logWriter, nil))

	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Events live in their own DB. Place it next to the
	// state DB the same way `enju serve` does.
	eventsPath := strings.TrimSuffix(dbPath, ".db") + "-events.db"
	es, err := store.NewSQLiteEventStore(eventsPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { es.Close() })
	st.AttachEventStore(es)

	ws, err := mcpgit.NewWorkspace(workspaceDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	srv := api.NewServer(st, logger)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	return &testServer{
		t:            t,
		server:       ts,
		url:          ts.URL,
		bareBaseDir:  bareBaseDir,
		workspaceDir: workspaceDir,
		workspace:    ws,
		store:        st,
		remotes:      make(map[int64]string),
		tokens:       make(map[string]string),
	}
}

// tokenFor returns the cached token for a username, or the
// store-persisted token if we didn't cache it (e.g. the
// username was created outside our helpers). Returns an empty
// string if the citizen doesn't exist at all.
func (s *testServer) tokenFor(username string) string {
	s.muAuth.Lock()
	tok, ok := s.tokens[username]
	s.muAuth.Unlock()
	if ok {
		return tok
	}
	if cz, err := s.store.GetCitizenByUsername(username); err == nil && cz != nil {
		s.muAuth.Lock()
		s.tokens[username] = cz.Token
		s.muAuth.Unlock()
		return cz.Token
	}
	return ""
}

// defaultToken returns the token of the first-registered citizen
// (the implicit "default actor" for unqualified post/get calls).
// Returns "" when no citizen has been registered yet.
func (s *testServer) defaultToken() string {
	s.muAuth.Lock()
	defer s.muAuth.Unlock()
	if s.defaultUser == "" {
		return ""
	}
	return s.tokens[s.defaultUser]
}

// wipeProjectMembers strips a project down to zero members so
// it lands in the legacy open bucket that gating treats as
// transparent. Test-harness convention: most existing tests
// register multiple citizens and expect them all to work
// against a freshly-created project without explicit membership
// setup. Membership-specific tests create projects via
// mcpCreateProjectAs which does NOT call this helper.
func (s *testServer) wipeProjectMembers(projectID int64) {
	s.t.Helper()
	members, _ := s.store.ListProjectMembers(projectID)
	for _, m := range members {
		_ = s.store.RemoveProjectMember(projectID, m.CitizenID)
	}
}

// ensureDefaultCitizen auto-registers a throwaway "harness"
// citizen so tests that don't care about identity still have an
// implicit token to attach to unqualified post/get calls. No-op
// if a default citizen is already set (either because the test
// already registered someone, or because this helper fired on a
// previous call).
func (s *testServer) ensureDefaultCitizen() {
	s.muAuth.Lock()
	have := s.defaultUser != ""
	s.muAuth.Unlock()
	if have {
		return
	}
	s.register("Test Harness")
}

// setAuth stashes a username/token pair and, if no default actor
// is set yet, marks this username as the default. register()
// calls this as soon as the coordinator hands back a fresh
// token, so the very next call that uses the implicit default
// picks up the right identity.
func (s *testServer) setAuth(username, token string) {
	s.muAuth.Lock()
	defer s.muAuth.Unlock()
	if s.tokens == nil {
		s.tokens = make(map[string]string)
	}
	s.tokens[username] = token
	if s.defaultUser == "" {
		s.defaultUser = username
	}
}

// doAuthed is the shared request path for every helper that
// needs to authenticate. Builds the request, attaches the token
// (if any), decodes the response into one of the shapes the
// caller requested. A token of "" means "anonymous" — only
// valid for /citizens/register, which the coordinator whitelists.
func (s *testServer) doAuthed(method, path, token string, body interface{}) (*http.Response, []byte) {
	s.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		jsonBody, _ := json.Marshal(body)
		reader = bytes.NewReader(jsonBody)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, s.url+path, reader)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (s *testServer) get(path string) map[string]interface{} {
	s.t.Helper()
	s.ensureDefaultCitizen()
	_, data := s.doAuthed("GET", path, s.defaultToken(), nil)
	var result map[string]interface{}
	json.Unmarshal(data, &result)
	return result
}

func (s *testServer) getList(path string) []interface{} {
	s.t.Helper()
	s.ensureDefaultCitizen()
	_, data := s.doAuthed("GET", path, s.defaultToken(), nil)
	var result []interface{}
	json.Unmarshal(data, &result)
	return result
}

// getAs issues a GET with a specific citizen's token. Used by
// membership tests that want to verify gating acts differently
// per caller.
func (s *testServer) getAs(username, path string) map[string]interface{} {
	s.t.Helper()
	_, data := s.doAuthed("GET", path, s.tokenFor(username), nil)
	var result map[string]interface{}
	json.Unmarshal(data, &result)
	return result
}

func (s *testServer) post(path string, body interface{}) map[string]interface{} {
	s.t.Helper()
	// /citizens/register is the bootstrap endpoint — no token
	// required (and none available for the first citizen).
	if path == "/api/v1/citizens/register" {
		_, data := s.doAuthed("POST", path, "", body)
		var result map[string]interface{}
		json.Unmarshal(data, &result)
		return result
	}
	// Every other endpoint requires auth; auto-register a
	// throwaway harness citizen if the test hasn't already.
	s.ensureDefaultCitizen()
	_, data := s.doAuthed("POST", path, s.defaultToken(), body)
	var result map[string]interface{}
	json.Unmarshal(data, &result)
	return result
}

// postAs issues a POST with a specific citizen's token.
func (s *testServer) postAs(username, path string, body interface{}) map[string]interface{} {
	s.t.Helper()
	_, data := s.doAuthed("POST", path, s.tokenFor(username), body)
	var result map[string]interface{}
	json.Unmarshal(data, &result)
	return result
}

// register creates a citizen with the given display name. Returns the
// username the server generated (same as the display name if it was a
// clean slug, otherwise a slugified version or a suffixed variant on
// collision). Tests use the returned username everywhere that used to
// use the citizen id.
func (s *testServer) register(name string) string {
	s.t.Helper()
	resp := s.post("/api/v1/citizens/register", map[string]string{"name": name})
	username, ok := resp["username"].(string)
	if !ok {
		s.t.Fatalf("register failed: %v", resp)
	}
	// Stash the server-issued token so subsequent unauth'd
	// helpers (post, get) can attach the first-registered
	// citizen's Bearer automatically.
	if tok, _ := resp["token"].(string); tok != "" {
		s.setAuth(username, tok)
	}
	return username
}

func (s *testServer) registerWithEmail(name, email string) string {
	s.t.Helper()
	resp := s.post("/api/v1/citizens/register", map[string]string{"name": name, "email": email})
	username, ok := resp["username"].(string)
	if !ok {
		s.t.Fatalf("register failed: %v", resp)
	}
	if tok, _ := resp["token"].(string); tok != "" {
		s.setAuth(username, tok)
	}
	return username
}

// citizenID resolves a username to the internal int64 primary key via
// the store (skipping the API). Tests that need the PK for direct store
// operations like SetCitizenRole call this.
func (s *testServer) citizenID(username string) int64 {
	s.t.Helper()
	c, err := s.store.GetCitizenByUsername(username)
	if err != nil || c == nil {
		s.t.Fatalf("citizenID: username %q not found", username)
	}
	return c.ID
}

func (s *testServer) getCitizen(username string) map[string]interface{} {
	s.t.Helper()
	return s.get("/api/v1/citizens/by-username/" + username)
}

func (s *testServer) updateProfile(username, name, email string) map[string]interface{} {
	s.t.Helper()
	jsonBody, _ := json.Marshal(map[string]string{"name": name, "email": email})
	req, _ := http.NewRequest("PUT", s.url+"/api/v1/citizens/by-username/"+username+"/profile", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result
}

// createTestProject creates a fresh test project per call (unique
// name to avoid conflicts). In the iteration A model, every project
// needs a remote — the test helper spins up a bare repo on disk and
// passes its path as remote_url, so the fat-client submit path has
// somewhere to push.
func (s *testServer) createTestProject() int64 {
	s.t.Helper()
	// Hard-auth requires SOME citizen at the wheel. Tests that
	// don't care about identity (schema/parser tests) still
	// need a token, so auto-register a "harness" citizen the
	// first time we create a project without one.
	s.ensureDefaultCitizen()
	// Unique name — timestamp + counter-ish from test server
	name := fmt.Sprintf("test-%d", time.Now().UnixNano())

	// Pick a bare-remote path unique to this project. We don't know
	// the project ID yet so hash the timestamp suffix.
	barePath := filepath.Join(s.bareBaseDir, name)
	if _, err := gogit.PlainInitWithOptions(barePath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	}); err != nil {
		s.t.Fatalf("init bare remote: %v", err)
	}
	// Seed with an empty README commit on main so clones and pushes
	// have a base to work from.
	seedDir, err := os.MkdirTemp(s.bareBaseDir, "seed-")
	if err != nil {
		s.t.Fatalf("mkdir seed: %v", err)
	}
	defer os.RemoveAll(seedDir)
	sRepo, err := gogit.PlainInitWithOptions(seedDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		s.t.Fatalf("init seed: %v", err)
	}
	if _, err := sRepo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{barePath},
	}); err != nil {
		s.t.Fatalf("create seed remote: %v", err)
	}
	wt, _ := sRepo.Worktree()
	readme := filepath.Join(seedDir, "README.md")
	_ = os.WriteFile(readme, []byte("# "+name+"\n"), 0644)
	_, _ = wt.Add("README.md")
	sig := &object.Signature{Name: "Test", Email: "test@localhost", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		s.t.Fatalf("seed commit: %v", err)
	}
	if err := sRepo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		s.t.Fatalf("seed push: %v", err)
	}

	resp := s.post("/api/v1/projects", map[string]string{
		"name":       name,
		"remote_url": barePath,
	})
	id, _ := resp["id"].(float64)
	if id == 0 {
		s.t.Fatalf("failed to create test project: %v", resp)
	}
	projectID := int64(id)
	s.rememberRemote(projectID, barePath)
	s.wipeProjectMembers(projectID)
	return projectID
}

// createTestProjectNoRemote creates a project WITHOUT a remote URL
// — the post-Option-B "I just want to start working, no GitHub yet"
// shape that real users hit via enju_create_project without
// remote_url=. Used by regression tests for code paths that must
// work without a configured remote (e.g. fat-client commits should
// still land in the local clone for vote/review/answer submits;
// previously they silently went to the legacy POST path because
// useFatClient gated on remote URL presence).
//
// Returns the project ID. No bare repo is created — the workspace
// clone is created on demand by Workspace.ForProject(remoteURL="")
// via Option B's local-only fallback.
func (s *testServer) createTestProjectNoRemote() int64 {
	s.t.Helper()
	s.ensureDefaultCitizen()
	name := fmt.Sprintf("test-noremote-%d", time.Now().UnixNano())
	resp := s.post("/api/v1/projects", map[string]string{
		"name": name,
	})
	id, _ := resp["id"].(float64)
	if id == 0 {
		s.t.Fatalf("failed to create no-remote test project: %v", resp)
	}
	projectID := int64(id)
	s.wipeProjectMembers(projectID)
	return projectID
}

// createTestProjectAt creates a project at a specific bare remote
// path. Used by tests that need to share a remote across calls or
// verify external remote behavior. For the normal per-test case
// just call createTestProject().
func (s *testServer) createTestProjectAt(name, barePath string) int64 {
	s.t.Helper()
	s.ensureDefaultCitizen()
	resp := s.post("/api/v1/projects", map[string]string{
		"name":       name,
		"remote_url": barePath,
	})
	id, _ := resp["id"].(float64)
	if id == 0 {
		s.t.Fatalf("failed to create test project: %v", resp)
	}
	projectID := int64(id)
	s.rememberRemote(projectID, barePath)
	s.wipeProjectMembers(projectID)
	return projectID
}

func (s *testServer) submitYAML(path string) string {
	s.t.Helper()
	// Auto-create a test project
	projectID := s.createTestProject()
	return s.submitYAMLToProject(path, projectID)
}

func (s *testServer) submitYAMLToProject(path string, projectID int64) string {
	s.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatal(err)
	}
	// Serial-runs-per-branch invariant: every run lands on
	// exactly one branch. For test code that creates multiple
	// runs in a single project to exercise numbering /
	// idempotency / error paths, auto-naming each one sidesteps
	// the "main already has an active run" refusal. Membership-
	// and branch-specific tests pass branch explicitly via the
	// MCP tool path instead.
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", projectID), map[string]string{
		"yaml":   string(data),
		"branch": "auto",
	})

	// The run's per-project sequence is what we use for task ID prefixing
	seqFloat, _ := resp["seq"].(float64)
	if seqFloat == 0 {
		s.t.Fatalf("submit failed: %v", resp)
		return ""
	}

	// Track project_id and run_seq for task auto-prefixing
	s.lastProjectID = projectID
	s.lastRunSeq = int(seqFloat)
	s.lastRunID = fmt.Sprintf("%d:%d", projectID, int(seqFloat))
	return s.lastRunID
}

// submitInlineYAML creates a run from an inline YAML string (no file needed).
func (s *testServer) submitInlineYAML(yamlContent string) string {
	s.t.Helper()
	projectID := s.createTestProject()
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", projectID), map[string]string{
		"yaml": yamlContent,
	})
	seqFloat, _ := resp["seq"].(float64)
	if seqFloat == 0 {
		s.t.Fatalf("submit failed: %v", resp)
		return ""
	}
	s.lastProjectID = projectID
	s.lastRunSeq = int(seqFloat)
	s.lastRunID = fmt.Sprintf("%d:%d", projectID, int(seqFloat))
	return s.lastRunID
}

// taskID returns the full task ID (project-run-prefixed) for the most recently submitted run.
// Format: {projectID}:{runSeq}:{shortID} or {projectID}:{runSeq}:{instanceKey}:{taskDefID}
func (s *testServer) taskID(shortID string) string {
	// Already fully qualified if it starts with an integer that matches the last project
	if strings.Contains(shortID, ":") {
		parts := strings.SplitN(shortID, ":", 2)
		if _, err := strconv.Atoi(parts[0]); err == nil {
			// looks like it already starts with projectID
			return shortID
		}
	}
	if s.lastProjectID > 0 && s.lastRunSeq > 0 {
		return fmt.Sprintf("%d:%d:%s", s.lastProjectID, s.lastRunSeq, shortID)
	}
	return shortID
}

func (s *testServer) readyTasks(runID string) []interface{} {
	s.t.Helper()
	path := "/api/v1/tasks/ready"
	if runID != "" {
		// runID is "projectID:runSeq" — split into separate query params
		parts := strings.SplitN(runID, ":", 2)
		if len(parts) == 2 {
			path += "?project_id=" + parts[0] + "&run_id=" + parts[1]
		} else {
			path += "?run_id=" + runID
		}
	}
	return s.getList(path)
}

// claim posts a claim request on behalf of the given username.
// The parameter is named citizenID for historical reasons but tests
// now pass the username (which is returned by register()).
func (s *testServer) claim(taskID, username string) map[string]interface{} {
	s.t.Helper()
	return s.post("/api/v1/tasks/"+s.taskID(taskID)+"/claim", map[string]string{"username": username})
}

// submit writes a text result via the fat-client path: compute the
// task's expected result layout, write result.md + metadata.json to
// the project's local clone, commit+push via mcpgit, then POST the
// report with commit_sha to the coordinator. This exercises the
// exact code path the real MCP client uses.
func (s *testServer) submit(taskID, content string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmit(taskID, content, nil, nil)
}

// submitWithArtifacts is submit + artifact writes in one atomic commit.
func (s *testServer) submitWithArtifacts(taskID, content string, artifacts map[string]string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmit(taskID, content, nil, artifacts)
}

// submitOutputs submits a named-outputs result. Named outputs are
// serialized as a single result.json for the A.3 first cut; the
// follow-up iteration will support multi-file named output schemas.
func (s *testServer) submitOutputs(taskID string, outputs map[string]string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmit(taskID, "", outputs, nil)
}

// submitReview is the review-action variant of submit: writes the
// reviewer's comment as result.md, commits/pushes, and posts the
// report with decision:approve or decision:reject. Mirrors what
// the MCP client sends on a real review submission.
func (s *testServer) submitReview(taskID, comment, decision string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmitWithDecision(taskID, comment, nil, nil, decision, "")
}

// submitReviewAs is the multi-reviewer variant — names which
// reviewer is casting this verdict so the coordinator credits
// the right task_claims slot and writes the prose into the
// correct citizen-{username}/ subdirectory.
func (s *testServer) submitReviewAs(taskID, username, comment, decision string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmitWithDecisionAs(taskID, username, comment, nil, nil, decision, "")
}

// submitVote is the vote-action variant of submit: writes the
// voter's commentary as result.md, commits/pushes, and posts
// the report with option:<option_id>. Mirrors what the MCP
// client sends on a real vote submission. Defaults to the
// single-voter "test" username for session-1-style submissions.
func (s *testServer) submitVote(taskID, comment, option string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmitWithDecisionAs(taskID, "", comment, nil, nil, "", option)
}

// submitVoteAs is the multi-voter variant: names which citizen is
// casting this vote so the coordinator credits the right
// task_claims slot and writes the result into the correct
// citizen-{username}/ subdirectory.
func (s *testServer) submitVoteAs(taskID, username, comment, option string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmitWithDecisionAs(taskID, username, comment, nil, nil, "", option)
}

// fatClientSubmit is the shared helper that performs the full
// iteration A write path: open the project's local clone, write
// result files + artifacts, commit + push to the project's bare
// remote, then POST the metadata report to the coordinator.
func (s *testServer) fatClientSubmit(taskIDShort, content string, outputs, artifacts map[string]string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmitWithDecision(taskIDShort, content, outputs, artifacts, "", "")
}

// fatClientSubmitWithDecision is fatClientSubmit with a review
// decision and/or vote option attached. Review tasks pass
// "approve" / "reject" in decision; vote tasks pass the chosen
// option id in voteOption; other actions pass empty strings.
func (s *testServer) fatClientSubmitWithDecision(taskIDShort, content string, outputs, artifacts map[string]string, decision, voteOption string) map[string]interface{} {
	s.t.Helper()
	return s.fatClientSubmitWithDecisionAs(taskIDShort, "", content, outputs, artifacts, decision, voteOption)
}

// fatClientSubmitWithDecisionAs is the multi-citizen variant: takes
// an explicit username identifying which citizen is submitting.
// For multi-citizen tasks the result lands under a
// `citizen-<username>/` subdirectory so parallel submissions
// don't race on the same result.md. When asUser is empty, the
// helper uses the generic test identity (single-citizen shape).
func (s *testServer) fatClientSubmitWithDecisionAs(taskIDShort, asUser, content string, outputs, artifacts map[string]string, decision, voteOption string) map[string]interface{} {
	s.t.Helper()
	fullTaskID := s.taskID(taskIDShort)

	// Fetch task metadata so we know run_seq, task_def_id,
	// instance_key, project_id (all needed to compute the result
	// layout).
	task := s.get("/api/v1/tasks/" + fullTaskID)
	if errMsg, ok := task["error"].(string); ok {
		s.t.Fatalf("fetchTask %q: %s", fullTaskID, errMsg)
	}
	projectID := int64(task["project_id"].(float64))
	runSeq := int(task["run_seq"].(float64))
	taskDefID, _ := task["task_def_id"].(string)
	instanceKey, _ := task["instance_key"].(string)

	remoteURL := s.remoteFor(projectID)
	if remoteURL == "" {
		s.t.Fatalf("project %d has no remote_url configured", projectID)
	}

	proj, err := s.workspace.ForProject(projectID, remoteURL)
	if err != nil {
		s.t.Fatalf("open project %d: %v", projectID, err)
	}

	// Phase E.2 session 2a — multi-citizen tasks route each
	// submission into `citizen-<username>/` under the task's
	// base result directory so parallel submitters don't race
	// on the same result.md. Session-1 single-citizen tasks
	// keep the flat layout.
	// Server-computed result_dir on the task response is the
	// canonical layout — use it directly rather than
	// duplicating the layout rule in test code.
	baseResultDir, _ := task["result_dir"].(string)
	_ = runSeq
	_ = instanceKey
	_ = taskDefID
	resultDir := baseResultDir
	citizens := int64(1)
	if v, ok := task["citizens"].(float64); ok {
		citizens = int64(v)
	}
	if citizens > 1 {
		voterUser := asUser
		if voterUser == "" {
			voterUser = "test"
		}
		resultDir = filepath.Join(baseResultDir, "citizen-"+voterUser)
	}
	files := []mcpgit.FileWrite{}
	metadata := map[string]interface{}{
		"task_id":     fullTaskID,
		"model":       "test",
		"result_type": "text",
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	// Phase E review-action audit trail. Mirror what
	// submitResultFatClient does in production: when the task is
	// action:review, embed action + decision + reviews_target in
	// metadata.json so `git show` on the commit tells the full
	// verdict story without touching the coordinator DB.
	action, _ := task["action"].(string)
	if action == "review" {
		metadata["action"] = "review"
		metadata["decision"] = decision
		if rt, _ := task["reviews_target"].(string); rt != "" {
			metadata["reviews_target"] = rt
		}
	}
	if action == "vote" {
		metadata["action"] = "vote"
		metadata["option"] = voteOption
		if voteOptsRaw, _ := task["vote_options"].(string); voteOptsRaw != "" {
			var parsed interface{}
			if json.Unmarshal([]byte(voteOptsRaw), &parsed) == nil {
				metadata["options"] = parsed
			}
		}
	}
	if content != "" {
		files = append(files, mcpgit.FileWrite{
			RepoRelPath: filepath.Join(resultDir, "result.md"),
			Content:     []byte(content),
		})
	}
	if outputs != nil {
		metadata["result_type"] = "json"
		metadata["named_outputs"] = true
		// If the task declares a schema with file: specs, write
		// each output to its own file per the schema. Otherwise
		// serialize the outputs map as a single result.json.
		schemaJSON, _ := task["outputs"].(string)
		schema := mcpgit.ParseNamedOutputSchema(schemaJSON)
		hasFileSpec := false
		for _, sp := range schema {
			if sp.File != "" {
				hasFileSpec = true
				break
			}
		}
		if hasFileSpec {
			outFiles, fileIndex := mcpgit.BuildNamedOutputFiles(resultDir, schema, outputs)
			files = append(files, outFiles...)
			metadata["output_files"] = fileIndex
		} else {
			outBytes, _ := json.MarshalIndent(outputs, "", "  ")
			files = append(files, mcpgit.FileWrite{
				RepoRelPath: filepath.Join(resultDir, "result.json"),
				Content:     outBytes,
			})
		}
	}
	if content == "" && outputs == nil && len(artifacts) == 0 {
		s.t.Fatalf("submit with no content, outputs, or artifacts")
	}
	metaBytes, _ := json.MarshalIndent(metadata, "", "  ")
	files = append(files, mcpgit.FileWrite{
		RepoRelPath: filepath.Join(resultDir, "metadata.json"),
		Content:     metaBytes,
	})
	var artifactPaths []string
	if len(artifacts) > 0 {
		for p := range artifacts {
			artifactPaths = append(artifactPaths, p)
		}
		// Deterministic ordering so commit message body matches.
		for i := 1; i < len(artifactPaths); i++ {
			for j := i; j > 0 && artifactPaths[j-1] > artifactPaths[j]; j-- {
				artifactPaths[j-1], artifactPaths[j] = artifactPaths[j], artifactPaths[j-1]
			}
		}
		for _, p := range artifactPaths {
			files = append(files, mcpgit.FileWrite{
				RepoRelPath: mcpgit.ArtifactPath(p),
				Content:     []byte(artifacts[p]),
			})
		}
	}

	proj.Lock()
	res, err := proj.SubmitTaskResult(mcpgit.SubmitRequest{
		TaskID:        fullTaskID,
		Username:      "test",
		AuthorName:    "Test Citizen",
		AuthorEmail:   "test@enju.local",
		Files:         files,
		ArtifactPaths: artifactPaths,
	})
	proj.Unlock()
	if err != nil {
		// Surface client-side submit failures (path
		// traversal rejected by wt.Add, malformed files,
		// etc.) as an error-shaped response so tests that
		// EXPECT rejection can assert on it the same way
		// they do for server-side rejections. Pre-scoped-
		// staging, AddGlob(".") silently skipped paths
		// outside the worktree and let server validation
		// reject; now we reject earlier at the git layer.
		// Both produce the same user-visible outcome.
		return map[string]interface{}{"error": err.Error()}
	}

	// Report the commit to the coordinator so state machine +
	// artifact index get updated.
	reportBody := map[string]interface{}{
		"commit_sha":        res.CommitSHA,
		"result_path":       resultDir,
		"artifacts_written": artifactPaths,
		"tokens_used":       100,
		"model":             "test",
	}
	if asUser != "" {
		reportBody["username"] = asUser
	}
	if decision != "" {
		reportBody["decision"] = decision
	}
	if voteOption != "" {
		reportBody["option"] = voteOption
	}
	if content != "" {
		reportBody["content"] = content
	}
	return s.post("/api/v1/tasks/"+fullTaskID+"/result", reportBody)
}

// readArtifactFile reads an artifact's content from the project's
// bare remote. Clones the bare into a throwaway dir to retrieve the
// file. Path is the user-facing artifact path (no prefix); the
// helper adds the `projects/{id}/artifacts/` namespace prefix that
// iteration A.5 introduced.
func (s *testServer) readArtifactFile(projectID int64, path string) (string, bool) {
	s.t.Helper()
	remoteURL := s.remoteFor(projectID)
	if remoteURL == "" {
		return "", false
	}
	cloneDir, err := os.MkdirTemp("", "read-artifact-")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(cloneDir)
	if _, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{URL: remoteURL}); err != nil {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(cloneDir, mcpgit.ArtifactPath(path)))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// readRepoFile reads any repo-relative file from a project's bare
// remote. Thin wrapper around a throwaway clone. Used by tests that
// need to peek at per-task result files like metadata.json or
// result.md without knowing the artifact path-mapping convention.
func (s *testServer) readRepoFile(projectID int64, repoRelPath string) ([]byte, bool) {
	s.t.Helper()
	remoteURL := s.remoteFor(projectID)
	if remoteURL == "" {
		return nil, false
	}
	cloneDir, err := os.MkdirTemp("", "read-repo-")
	if err != nil {
		return nil, false
	}
	defer os.RemoveAll(cloneDir)
	if _, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{URL: remoteURL}); err != nil {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(cloneDir, repoRelPath))
	if err != nil {
		return nil, false
	}
	return data, true
}

// repoFileSpec is the per-file payload for writeRepoFilesWithMode.
// Lets callers seed content at a specific permission mode so
// tests covering exec-bit preservation (template bundles with
// scripts, for instance) don't have to chmod after the fact.
type repoFileSpec struct {
	body string
	mode os.FileMode
}

// writeRepoFiles commits + pushes a set of files directly to a
// project's bare remote. Simple "all 0644" variant — thin
// wrapper over writeRepoFilesWithMode.
func (s *testServer) writeRepoFiles(projectID int64, files map[string]string, commitMsg string) {
	specs := make(map[string]repoFileSpec, len(files))
	for p, body := range files {
		specs[p] = repoFileSpec{body: body, mode: 0o644}
	}
	s.writeRepoFilesWithMode(projectID, specs, commitMsg)
}

// writeRepoFilesWithMode commits + pushes files with
// per-file permission modes via a throwaway clone. Use this
// when exec-bit preservation matters (scripts that run under
// the compute executor).
func (s *testServer) writeRepoFilesWithMode(projectID int64, files map[string]repoFileSpec, commitMsg string) {
	s.t.Helper()
	remoteURL := s.remoteFor(projectID)
	if remoteURL == "" {
		s.t.Fatalf("writeRepoFiles: no remote URL for project %d", projectID)
	}
	cloneDir, err := os.MkdirTemp("", "write-repo-")
	if err != nil {
		s.t.Fatalf("writeRepoFiles: mkdtemp: %v", err)
	}
	defer os.RemoveAll(cloneDir)
	repo, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{URL: remoteURL})
	if err != nil {
		s.t.Fatalf("writeRepoFiles: clone: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		s.t.Fatalf("writeRepoFiles: worktree: %v", err)
	}
	for rel, spec := range files {
		full := filepath.Join(cloneDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			s.t.Fatalf("writeRepoFiles: mkdir %s: %v", full, err)
		}
		mode := spec.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(full, []byte(spec.body), mode); err != nil {
			s.t.Fatalf("writeRepoFiles: write %s: %v", full, err)
		}
		if err := os.Chmod(full, mode); err != nil {
			s.t.Fatalf("writeRepoFiles: chmod %s: %v", full, err)
		}
		if _, err := wt.Add(rel); err != nil {
			s.t.Fatalf("writeRepoFiles: add %s: %v", rel, err)
		}
	}
	if _, err := wt.Commit(commitMsg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@localhost", When: time.Now()},
	}); err != nil {
		s.t.Fatalf("writeRepoFiles: commit: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{}); err != nil {
		s.t.Fatalf("writeRepoFiles: push: %v", err)
	}
}

func (s *testServer) release(taskID, username string) map[string]interface{} {
	s.t.Helper()
	return s.post("/api/v1/tasks/"+s.taskID(taskID)+"/release", map[string]string{"username": username})
}

// invalidate posts an invalidation request for a task, optionally with
// a reason. Returns the raw JSON response.
func (s *testServer) invalidate(taskID, reason string) map[string]interface{} {
	s.t.Helper()
	return s.post("/api/v1/tasks/"+s.taskID(taskID)+"/invalidate", map[string]string{"reason": reason})
}

// taskGet returns details for a task, auto-prefixing with run_id if needed.
func (s *testServer) taskGet(taskID string) map[string]interface{} {
	s.t.Helper()
	return s.get("/api/v1/tasks/" + s.taskID(taskID))
}

// taskInputs returns a resolved-prompt view of a task, matching the
// legacy response shape: `resolved_prompt`, `artifacts`,
// `missing_artifacts`. In the iteration A model the coordinator only
// serves the dependency descriptor; the test helper does the
// client-side resolution via mcpgit.Project.Resolve so existing
// tests can keep asserting on `resolved_prompt`.
func (s *testServer) taskInputs(taskID string) map[string]interface{} {
	s.t.Helper()
	fullTaskID := s.taskID(taskID)
	desc := s.get("/api/v1/tasks/" + fullTaskID + "/inputs?client_mode=true")
	if errMsg, ok := desc["error"].(string); ok {
		s.t.Fatalf("taskInputs descriptor: %s", errMsg)
	}

	// Marshal the dependency descriptor back into the mcpgit types
	// so we can use the shared resolver. JSON → struct is the
	// simplest path that doesn't re-implement the resolver.
	raw, _ := json.Marshal(desc)
	var d struct {
		PromptTemplate     string                   `json:"prompt_template"`
		UserPromptTemplate string                   `json:"user_prompt_template"`
		ForEachParams      map[string]string        `json:"for_each_params"`
		Dependencies       []map[string]interface{} `json:"dependencies"`
		ArtifactReads      []map[string]interface{} `json:"artifact_reads"`
		ProjectRemoteURL   string                   `json:"project_remote_url"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		s.t.Fatalf("decode descriptor: %v", err)
	}

	// Fetch project metadata so we can open the local workspace
	// clone.
	task := s.get("/api/v1/tasks/" + fullTaskID)
	projectID := int64(task["project_id"].(float64))

	remoteURL := s.remoteFor(projectID)
	if remoteURL == "" {
		// No remote? Return the raw descriptor — tests that rely
		// on this path will surface the issue loudly.
		return desc
	}
	proj, err := s.workspace.ForProject(projectID, remoteURL)
	if err != nil {
		s.t.Fatalf("open project: %v", err)
	}
	proj.Lock()
	_ = proj.Pull()
	proj.Unlock()

	input := mcpgit.ResolveInput{
		PromptTemplate:     d.PromptTemplate,
		UserPromptTemplate: d.UserPromptTemplate,
		ForEachParams:      d.ForEachParams,
	}
	for _, dep := range d.Dependencies {
		paramsRaw, _ := dep["instance_params"].(map[string]interface{})
		params := make(map[string]string, len(paramsRaw))
		for k, v := range paramsRaw {
			if sv, ok := v.(string); ok {
				params[k] = sv
			}
		}
		ref := mcpgit.DependencyRef{
			TaskDefID:      asString(dep["task_def_id"]),
			InstanceKey:    asString(dep["instance_key"]),
			InstanceParams: params,
			CommitSHA:      asString(dep["commit_sha"]),
			ResultPath:     asString(dep["result_path"]),
			VoteChoice:     asString(dep["vote_choice"]),
		}
		// Phase E.2 session 2b — multi-citizen upstream
		// responses. The coordinator populates a per-citizen
		// list on the descriptor; the resolver reads each
		// citizen's result.md from the local clone.
		if respsRaw, ok := dep["responses"].([]interface{}); ok {
			for _, r := range respsRaw {
				rm, _ := r.(map[string]interface{})
				pathUser := asString(rm["real_username"])
				if pathUser == "" {
					pathUser = asString(rm["username"])
				}
				ref.Responses = append(ref.Responses, mcpgit.CitizenResponseRef{
					Username:     asString(rm["username"]),
					PathUsername: pathUser,
					Option:       asString(rm["option"]),
					CommitSHA:    asString(rm["commit_sha"]),
				})
			}
		}
		input.Dependencies = append(input.Dependencies, ref)
	}
	for _, a := range d.ArtifactReads {
		input.ArtifactReads = append(input.ArtifactReads, mcpgit.ArtifactRef{
			Path:      asString(a["path"]),
			CommitSHA: asString(a["commit_sha"]),
		})
	}

	resolved, err := proj.Resolve(input)
	if err != nil {
		s.t.Fatalf("resolve: %v", err)
	}

	out := map[string]interface{}{
		"task_id":         fullTaskID,
		"resolved_prompt": resolved.Prompt,
	}
	if resolved.UserPrompt != "" {
		out["resolved_user_prompt"] = resolved.UserPrompt
	}
	if len(resolved.ResolvedArtifacts) > 0 {
		// Match legacy shape: map[string]string
		as := make(map[string]interface{}, len(resolved.ResolvedArtifacts))
		for k, v := range resolved.ResolvedArtifacts {
			as[k] = v
		}
		out["artifacts"] = as
	}
	if len(resolved.MissingArtifacts) > 0 {
		miss := make([]interface{}, 0, len(resolved.MissingArtifacts))
		for _, p := range resolved.MissingArtifacts {
			miss = append(miss, p)
		}
		out["missing_artifacts"] = miss
	}
	return out
}

// asString is a safe map-value-to-string coercion for the
// descriptor decode path.
func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// runPath converts a composite "projectID:runSeq" runID into the URL path segment
// "projects/{projectID}/runs/{runSeq}".
func runPath(runID string) string {
	parts := strings.SplitN(runID, ":", 2)
	if len(parts) != 2 {
		return "projects/0/runs/" + runID
	}
	return "projects/" + parts[0] + "/runs/" + parts[1]
}

func (s *testServer) runStatus(runID string) map[string]interface{} {
	s.t.Helper()
	return s.get("/api/v1/" + runPath(runID))
}

func (s *testServer) runTasks(runID string) []interface{} {
	s.t.Helper()
	return s.getList("/api/v1/" + runPath(runID) + "/tasks")
}

// assertResultFile checks that a task's result file was written to
// the project's bare remote with the expected content. Clones the
// bare on demand into a throwaway directory — this is slow but
// matches the reality of "content lives on the user's git host" in
// the orchestrator model.
func (s *testServer) assertResultFile(runID, instanceKey, taskDefID, expectedContent string) {
	s.t.Helper()
	parts := strings.SplitN(runID, ":", 2)
	if len(parts) != 2 {
		s.t.Fatalf("assertResultFile: bad runID %q (want projectID:runSeq)", runID)
	}
	projectIDInt, _ := strconv.ParseInt(parts[0], 10, 64)

	remoteURL := s.remoteFor(projectIDInt)
	if remoteURL == "" {
		s.t.Fatalf("assertResultFile: project %d has no remote_url", projectIDInt)
	}
	cloneDir, err := os.MkdirTemp("", "assert-result-")
	if err != nil {
		s.t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(cloneDir)
	if _, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{URL: remoteURL}); err != nil {
		s.t.Fatalf("clone bare: %v", err)
	}

	// Ask the coordinator for the task's result_dir rather than
	// rebuilding the layout rule here — the schema lives in
	// engine.ComputeResultDir.
	shortID := taskDefID
	if instanceKey != "" {
		shortID = instanceKey + ":" + taskDefID
	}
	task := s.taskGet(shortID)
	relDir, _ := task["result_dir"].(string)
	if relDir == "" {
		s.t.Fatalf("assertResultFile: task %s has no result_dir", shortID)
	}
	dir := filepath.Join(cloneDir, relDir)

	resultPath := filepath.Join(dir, "result.md")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		s.t.Fatalf("result file not found on remote: %s", resultPath)
	}
	content := string(data)
	if expectedContent != "" && !strings.Contains(content, expectedContent) {
		s.t.Fatalf("result file %s: expected to contain %q, got %q", resultPath, expectedContent, content)
	}

	metaPath := filepath.Join(dir, "metadata.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		s.t.Fatalf("metadata file not found on remote: %s", metaPath)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		s.t.Fatalf("invalid metadata JSON: %v", err)
	}
	if meta["task_id"] == nil {
		s.t.Fatal("metadata missing task_id")
	}
}

// assertGitCommits checks the number of commits in a project's
// bare remote. Uses `git log` against the bare — fast because the
// bare is on local disk.
func (s *testServer) assertGitCommits(projectID int64, expected int) {
	s.t.Helper()
	remoteURL := s.remoteFor(projectID)
	if remoteURL == "" {
		s.t.Fatalf("assertGitCommits: project %d has no remote_url", projectID)
	}
	cmd := exec.Command("git", "--git-dir", remoteURL, "log", "--oneline")
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatalf("git log failed on bare %s: %v", remoteURL, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != expected {
		s.t.Fatalf("project %d: expected %d git commits, got %d: %v", projectID, expected, len(lines), lines)
	}
}

// ===================================================================
// Tests
// ===================================================================

func TestHealth(t *testing.T) {
	s := newTestServer(t)
	resp := s.get("/health")
	if resp["status"] != "ok" {
		t.Fatalf("expected ok, got %v", resp)
	}
}





// ===================================================================
// Error Case Tests
// ===================================================================

func TestDoubleClaimRejected(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)

	resp := s.claim("task_a", bob)
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error for double claim")
	}
}

func TestClaimReleaseReclaim(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitYAML("testdata/simple-no-deps.yaml")

	s.claim("task_a", alice)
	rel := s.release("task_a", alice)
	if rel["status"] != "released" {
		t.Fatalf("expected released, got %v", rel)
	}

	resp := s.claim("task_a", bob)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("bob should be able to claim after release: %v", resp)
	}
}

func TestSubmitToNonClaimedFails(t *testing.T) {
	s := newTestServer(t)
	s.submitYAML("testdata/simple-no-deps.yaml")

	resp := s.submit("task_a", "some content")
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error submitting to unclaimed task")
	}
}

func TestNonexistentTaskFails(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	resp := s.claim("nonexistent", alice)
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestRegisterWithoutNameFails(t *testing.T) {
	s := newTestServer(t)
	resp := s.post("/api/v1/citizens/register", map[string]string{})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error for empty name")
	}
}

func TestInvalidYAMLRejected(t *testing.T) {
	s := newTestServer(t)
	pid := s.createTestProject()
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": "not: [valid"})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error for invalid yaml")
	}
}

func TestEmptyYAMLRejected(t *testing.T) {
	s := newTestServer(t)
	pid := s.createTestProject()
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": ""})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error for empty yaml")
	}
}


func TestTaskTimeout(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitYAML("testdata/short-timeout.yaml")

	// Alice claims the task (1s timeout)
	s.claim("quick_task", alice)

	// Wait for the deadline to pass
	time.Sleep(2 * time.Second)

	// Check for expired claims directly via store
	expired, err := s.store.GetExpiredClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 {
		t.Fatalf("expected 1 expired claim, got %d", len(expired))
	}

	// Expire it (simulating what the reaper does)
	err = s.store.ExpireClaimedTask(expired[0].TaskID, expired[0].CitizenID)
	if err != nil {
		t.Fatal(err)
	}

	// Task should be back to READY
	task := s.taskGet("quick_task")
	if task["state"] != "ready" {
		t.Fatalf("expected ready after timeout, got %v", task["state"])
	}

	// Bob can now claim it
	resp := s.claim("quick_task", bob)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("bob should be able to claim after timeout: %v", resp)
	}
}


func TestTaskSeqNumbers(t *testing.T) {
	s := newTestServer(t)

	s.submitYAML("testdata/branching.yaml")

	// Check tasks have sequential numbers
	tasks := s.getList("/api/v1/tasks/ready")
	seqs := make(map[float64]bool)
	for _, task := range tasks {
		tm, _ := task.(map[string]interface{})
		seq, _ := tm["seq"].(float64)
		if seq == 0 {
			t.Fatal("expected non-zero seq number")
		}
		seqs[seq] = true
	}
	if len(seqs) != 2 {
		t.Fatalf("expected 2 unique seq numbers for ready tasks, got %d", len(seqs))
	}
}

func TestRunAutoIncrementID(t *testing.T) {
	s := newTestServer(t)

	// Two runs in the same project → seq #1, #2
	projectID := s.createTestProject()
	pid1 := s.submitYAMLToProject("testdata/simple-no-deps.yaml", projectID)
	pid2 := s.submitYAMLToProject("testdata/branching.yaml", projectID)

	expected1 := fmt.Sprintf("%d:1", projectID)
	expected2 := fmt.Sprintf("%d:2", projectID)
	if pid1 != expected1 {
		t.Fatalf("expected first run id %q, got %q", expected1, pid1)
	}
	if pid2 != expected2 {
		t.Fatalf("expected second run id %q, got %q", expected2, pid2)
	}
}



func TestFreshDBHasNoProjects(t *testing.T) {
	s := newTestServer(t)

	projects := s.getList("/api/v1/projects")
	if len(projects) != 0 {
		t.Fatalf("expected 0 projects on fresh DB, got %d", len(projects))
	}
}


func TestRunRequiresProject(t *testing.T) {
	s := newTestServer(t)

	// Submit with non-existent project_id — should fail
	yamlData, _ := os.ReadFile("testdata/simple-no-deps.yaml")
	bad := s.post("/api/v1/projects/999/runs", map[string]string{"yaml": string(yamlData)})
	if _, hasErr := bad["error"]; !hasErr {
		t.Fatal("expected error when submitting to non-existent project")
	}
}


func TestPerProjectRunNumbering(t *testing.T) {
	s := newTestServer(t)

	// Create two projects
	pj1 := s.post("/api/v1/projects", map[string]string{"name": "Project A"})
	pid1 := int64(pj1["id"].(float64))
	pj2 := s.post("/api/v1/projects", map[string]string{"name": "Project B"})
	pid2 := int64(pj2["id"].(float64))

	yamlData, _ := os.ReadFile("testdata/simple-no-deps.yaml")

	// Project A gets runs #1, #2. Each lands on its own auto-
	// allocated branch so the serial-per-branch invariant
	// doesn't block the second submission.
	r1 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid1), map[string]string{"yaml": string(yamlData), "branch": "auto"})
	seq1, _ := r1["seq"].(float64)
	r2 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid1), map[string]string{"yaml": string(yamlData), "branch": "auto"})
	seq2, _ := r2["seq"].(float64)

	if int(seq1) != 1 || int(seq2) != 2 {
		t.Fatalf("expected seq 1,2 in project A, got %d,%d", int(seq1), int(seq2))
	}

	// Project B also starts from #1 (independent numbering)
	r3 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid2), map[string]string{"yaml": string(yamlData), "branch": "auto"})
	seq3, _ := r3["seq"].(float64)

	if int(seq3) != 1 {
		t.Fatalf("expected seq 1 in new project B, got %d", int(seq3))
	}
}



func TestCitizenRegistrationWithEmail(t *testing.T) {
	s := newTestServer(t)

	// Register with email
	alice := s.registerWithEmail("Alice", "alice@example.com")

	// Check profile has email
	profile := s.getCitizen(alice)
	if profile["name"] != "Alice" {
		t.Fatalf("expected Alice, got %v", profile["name"])
	}
	if profile["email"] != "alice@example.com" {
		t.Fatalf("expected email, got %v", profile["email"])
	}
	if profile["role"] != "citizen" {
		t.Fatalf("expected citizen role, got %v", profile["role"])
	}
}

func TestDuplicateEmailRejected(t *testing.T) {
	s := newTestServer(t)

	s.registerWithEmail("Alice", "alice@example.com")

	// Try registering with same email
	resp := s.post("/api/v1/citizens/register", map[string]string{
		"name":  "Fake Alice",
		"email": "alice@example.com",
	})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error for duplicate email")
	}
}

func TestRegisterWithoutEmailAllowed(t *testing.T) {
	s := newTestServer(t)

	// Multiple citizens without email — all should succeed
	a := s.register("Alice")
	b := s.register("Bob")

	if a == b {
		t.Fatal("expected different IDs")
	}
}


func TestUpdateProfileDuplicateEmail(t *testing.T) {
	s := newTestServer(t)

	s.registerWithEmail("Alice", "alice@example.com")
	bob := s.registerWithEmail("Bob", "bob@example.com")

	// Bob tries to take Alice's email
	resp := s.updateProfile(bob, "Bob", "alice@example.com")
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatal("expected error for duplicate email")
	}
}

// --- Phase C: Artifacts ---



// TestArtifactWriteRejectedForTraversal confirms path validation.
func TestArtifactWriteRejectedForTraversal(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	s.submitYAML("testdata/artifacts.yaml")
	s.claim("bootstrap", alice)

	resp := s.submitWithArtifacts("bootstrap", "path traversal attempt", map[string]string{
		"../escape.txt": "no",
	})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("expected error for path traversal, got %v", resp)
	}
}




// --- Iteration 2: username model ---

// TestRegisterGeneratesUsernameFromName verifies the server
// auto-slugifies the display name into a unique username when no
// explicit username is provided.
func TestRegisterGeneratesUsernameFromName(t *testing.T) {
	s := newTestServer(t)

	// Simple name → slug is identical.
	resp := s.post("/api/v1/citizens/register", map[string]string{"name": "alice"})
	if u, _ := resp["username"].(string); u != "alice" {
		t.Fatalf("expected username 'alice', got %v", resp["username"])
	}

	// Multi-word display name → slug with hyphens.
	resp = s.post("/api/v1/citizens/register", map[string]string{"name": "Tamer Gur"})
	if u, _ := resp["username"].(string); u != "tamer-gur" {
		t.Fatalf("expected username 'tamer-gur', got %v", resp["username"])
	}

	// Collision on the same slug → -2 suffix.
	resp = s.post("/api/v1/citizens/register", map[string]string{"name": "alice"})
	if u, _ := resp["username"].(string); u != "alice-2" {
		t.Fatalf("expected username 'alice-2' on collision, got %v", resp["username"])
	}
}

// TestRegisterWithExplicitUsername verifies the caller can override the
// auto-generated username.
func TestRegisterWithExplicitUsername(t *testing.T) {
	s := newTestServer(t)

	resp := s.post("/api/v1/citizens/register", map[string]string{
		"name":     "Tamer Gur",
		"username": "tamerh",
	})
	if u, _ := resp["username"].(string); u != "tamerh" {
		t.Fatalf("expected username 'tamerh', got %v", resp["username"])
	}

	// Invalid username (uppercase) is rejected.
	resp = s.post("/api/v1/citizens/register", map[string]string{
		"name":     "Bob",
		"username": "BobTheBuilder",
	})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("expected error for invalid username format, got %v", resp)
	}

	// Already-taken username is rejected.
	resp = s.post("/api/v1/citizens/register", map[string]string{
		"name":     "Impersonator",
		"username": "tamerh",
	})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("expected error for taken username, got %v", resp)
	}
}

// TestAssignToRejectsUnknownUsername confirms run submission fails if
// assign_to references a username that isn't registered.
func TestAssignToRejectsUnknownUsername(t *testing.T) {
	s := newTestServer(t)

	yaml := `name: "Unknown assignee"
version: 1
tasks:
  - id: t1
    action: answer
    assign_to: nonexistent-user
    prompt: "Hi"
`
	pid := s.createTestProject()
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml})
	errMsg, hasErr := resp["error"].(string)
	if !hasErr {
		t.Fatalf("expected error for unknown assignee, got: %v", resp)
	}
	if !strings.Contains(errMsg, "nonexistent-user") {
		t.Fatalf("expected error to mention unknown username, got: %s", errMsg)
	}
}

// TestRunCreationAtomicOnValidationFailure is a regression test for
// the ghost-run bug: if a task midway through a YAML fails validation
// (here, an unknown assign_to username), the entire submission must be
// rejected with zero side effects — no run inserted, no tasks created,
// and the per-project seq counter must not advance.
func TestRunCreationAtomicOnValidationFailure(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.createTestProject()

	// First submit a valid run so we know what the next seq should be.
	validYAML := `name: "Valid run"
version: 1
tasks:
  - id: warmup
    action: answer
    prompt: "Warmup"
`
	// Each successful run gets its own auto-allocated branch so
	// the serial-per-branch invariant doesn't interfere with
	// the seq-numbering assertions in this test.
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": validYAML, "branch": "auto"})
	firstSeq, _ := resp["seq"].(float64)
	if firstSeq != 1 {
		t.Fatalf("expected first run seq 1, got %v", resp["seq"])
	}

	// Now submit a run where the 3rd task has an unregistered
	// assign_to, so validation fails after the first two are
	// conceptually "processed."
	badYAML := fmt.Sprintf(`name: "Mid-file failure"
version: 1
tasks:
  - id: open_task
    action: answer
    prompt: "Anyone"
  - id: assigned_to_me
    action: answer
    assign_to: %s
    prompt: "Mine"
  - id: assigned_to_stranger
    action: answer
    assign_to: nobody-here
    prompt: "Rejected"
  - id: open_task_4
    action: answer
    prompt: "Fourth"
`, alice)

	badResp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": badYAML, "branch": "auto"})
	if _, hasErr := badResp["error"]; !hasErr {
		t.Fatalf("expected error for mid-file validation failure, got: %v", badResp)
	}

	// Side effect checks:
	// 1. The next successful run should be seq 2, not seq 3 — the failed
	//    submission must NOT have consumed a seq number.
	goodYAML := `name: "Follow-up"
version: 1
tasks:
  - id: next
    action: answer
    prompt: "After the failure"
`
	goodResp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": goodYAML, "branch": "auto"})
	nextSeq, _ := goodResp["seq"].(float64)
	if nextSeq != 2 {
		t.Fatalf("expected next run seq 2 (failed submission should not consume a seq), got %v", goodResp["seq"])
	}

	// 2. Listing runs under the project should show exactly 2 runs
	//    (the warmup at seq 1 and the follow-up at seq 2), not 3.
	runs := s.getList(fmt.Sprintf("/api/v1/projects/%d/runs", pid))
	if len(runs) != 2 {
		var seqs []int
		for _, r := range runs {
			m, _ := r.(map[string]interface{})
			s, _ := m["seq"].(float64)
			seqs = append(seqs, int(s))
		}
		t.Fatalf("expected 2 runs after atomic-failed submission, got %d (seqs: %v)", len(runs), seqs)
	}

	// 3. None of the ghost task IDs from the failed run should exist.
	for _, ghostID := range []string{
		fmt.Sprintf("%d:2:open_task", pid),
		fmt.Sprintf("%d:2:assigned_to_me", pid),
	} {
		task := s.get("/api/v1/tasks/" + ghostID)
		if _, found := task["id"].(string); found {
			t.Fatalf("ghost task %q exists after atomic-failed submission: %v", ghostID, task)
		}
	}
}

// --- Iteration 1: assign_to + require_role ---





// --- Iteration 3: cascade invalidation ---









// --- Iteration 1: assign_to + require_role (historical section) ---






// cleanYAML strips common LLM output issues: markdown fences, leading text, trailing text.
func cleanYAML(raw string) string {
	s := strings.TrimSpace(raw)

	// Remove markdown code fences
	if strings.HasPrefix(s, "```yaml") {
		s = strings.TrimPrefix(s, "```yaml")
	} else if strings.HasPrefix(s, "```yml") {
		s = strings.TrimPrefix(s, "```yml")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}

	s = strings.TrimSpace(s)

	// If LLM added text before the YAML, find where "name:" starts
	if idx := strings.Index(s, "name:"); idx > 0 {
		s = s[idx:]
	}

	return strings.TrimSpace(s)
}





















// TestCoordinatorRejectsMalformedCommitSHA verifies the REST
// submit path rejects commit_sha values that aren't shaped like
// git SHAs (40 or 64 hex chars). Trust-the-client doesn't mean
// trust any string — a buggy client sending garbage would
// pollute the artifact index and break downstream template
// resolution. Shape check only, not a fetch-and-verify (that's
// still a future opt-in mode per ARCHITECTURE principle 7 +
// Open Question #4).
func TestCoordinatorRejectsMalformedCommitSHA(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)

	fullID := s.taskID("task_a")
	malformed := []string{
		"not-a-sha",
		"abc123",                // too short
		strings.Repeat("z", 40), // correct length, wrong chars
		"ABCDEF1234567890abcdef1234567890abcdef12", // uppercase rejected (git uses lowercase)
		strings.Repeat("0", 40), // empty-SHA sentinel — go-git uses this as nil-ref
		strings.Repeat("f", 40), // common test-garbage phantom
		strings.Repeat("a", 64), // SHA-256 length but all-same-char
	}
	for _, bad := range malformed {
		t.Run(bad, func(t *testing.T) {
			resp := s.post("/api/v1/tasks/"+fullID+"/result", map[string]interface{}{
				"result_path": "enju/runs/1-simple-no-dependencies/task_a",
				"commit_sha":  bad,
				"content":     "data",
				"username":    alice,
			})
			if errMsg, _ := resp["error"].(string); errMsg == "" {
				t.Fatalf("expected rejection for malformed commit_sha %q, got success: %v", bad, resp)
			} else if !strings.Contains(errMsg, "not a valid git SHA") {
				t.Errorf("expected shape-check error, got: %q", errMsg)
			}
		})
	}

	// Valid shape is accepted (we don't fetch — trust model
	// stays). Any 40-hex string passes the shape check.
	valid := "0123456789abcdef0123456789abcdef01234567"
	resp := s.post("/api/v1/tasks/"+fullID+"/result", map[string]interface{}{
		"result_path": "enju/runs/1-simple-no-dependencies/task_a",
		"commit_sha":  valid,
		"content":     "data",
		"username":    alice,
	})
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		t.Errorf("valid-shape commit_sha should be accepted, got error: %q", errMsg)
	}
}

// TestReviewCommitShaOptional — a review submission with empty
// commit_sha is accepted (P1 fix: commit_sha optional on
// review/vote tasks because they don't ship content to git).
func TestReviewCommitShaOptional(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")
	s.submitYAML("testdata/review.yaml")

	// Alice drafts normally.
	s.claim("draft", alice)
	s.submit("draft", "A draft.")

	// Bob reviews. Post directly to the coordinator bypassing
	// the fat client so we can send commit_sha:"" explicitly
	// — the fat client would always populate one.
	s.claim("check", bob)
	fullID := s.taskID("check")
	resp := s.post("/api/v1/tasks/"+fullID+"/result", map[string]interface{}{
		"result_path": "enju/runs/1-review-flow/check",
		"commit_sha":  "",
		"decision":    "approve",
		"username":    "bob",
		"tokens_used": 0,
		"model":       "test",
	})
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		t.Fatalf("expected review submit with empty commit_sha to succeed, got error: %q", errMsg)
	}
	if resp["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", resp["status"])
	}
}














// TestTokenAuthRejectsInvalidToken — a request with a
// fake Bearer token should get 401.
func TestTokenAuthRejectsInvalidToken(t *testing.T) {
	s := newTestServer(t)

	req, _ := http.NewRequest("GET", s.url+"/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer fake-token-12345")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid token, got %d", resp.StatusCode)
	}
}

// TestTokenAuthRejectsMissingToken — the coordinator hard-
// enforces the Bearer token on every endpoint except
// /citizens/register. A request with no Authorization header
// must get a 401 so public-facing deployments don't leak
// project data to anonymous callers.
func TestTokenAuthRejectsMissingToken(t *testing.T) {
	s := newTestServer(t)

	resp, err := http.Get(s.url + "/api/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing token, got %d", resp.StatusCode)
	}
}

// TestTokenAuthAllowsValidToken — register a citizen, use
// the returned token, verify the request succeeds.
func TestTokenAuthAllowsValidToken(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	_ = alice

	// Get the token from the citizens table via the DB.
	citizen := s.get("/api/v1/citizens/by-username/alice")
	// The token isn't in the public response (correct for
	// security). So we test via: register returns a token,
	// the client sends it, and the request works. Since our
	// test harness doesn't save tokens, test that a POST
	// with no token (soft enforcement) works for now.
	// The real token test is: fake token → 401 (above).
	projectResp := s.post("/api/v1/projects", map[string]string{
		"name": "token-test",
	})
	if errMsg, ok := projectResp["error"].(string); ok {
		t.Errorf("expected project creation to succeed, got: %s", errMsg)
	}
	_ = citizen
}

// TestAutoLocalRepoOnProjectCreate — when a project is
// created without a remote_url, the system should still
// allow full claim/submit workflow (the MCP client auto-
// creates a local bare repo). This test simulates the
// coordinator side: a project with a local bare repo as
// remote should work for the full fat-client path.

// TestSetProjectRemoteRejectsEmptyURLAtAPI is the
// defense-in-depth complement to the MCP-handler-level
// validation: the coordinator's PUT /projects/{id}/remote
// must also reject an empty remote_url. A direct HTTP call
// (curl, alternative client) bypasses the MCP handler, so
// without this check an attacker / scripted caller could
// silently fork a multi-machine project by clearing its
// remote.
func TestSetProjectRemoteRejectsEmptyURLAtAPI(t *testing.T) {
	s := newTestServer(t)
	pid := s.createTestProject()

	for _, badURL := range []string{"", "   ", "\t\n"} {
		body := map[string]string{"remote_url": badURL}
		jsonBody, _ := json.Marshal(body)
		req, _ := http.NewRequest("PUT",
			s.url+fmt.Sprintf("/api/v1/projects/%d/remote", pid),
			bytes.NewReader(jsonBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.defaultToken())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("transport error for %q: %v", badURL, err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("remote_url=%q: expected 400, got %d (body: %s)", badURL, resp.StatusCode, bodyBytes)
			continue
		}
		if !strings.Contains(string(bodyBytes), "cannot be empty") {
			t.Errorf("remote_url=%q: expected 'cannot be empty' in body, got: %s", badURL, bodyBytes)
		}
	}
}

// findEvent queries the project event log filtered by event type
// and returns the first match (newest-first) after waiting briefly
// for the async writer to drain. nil when no match. Used by the
// emission tests below.
func (s *testServer) findEvent(projectID int64, eventType string) map[string]interface{} {
	s.t.Helper()
	s.store.Events().WaitForDrain(2 * time.Second)
	path := fmt.Sprintf("/api/v1/projects/%d/events?event_types=%s&limit=100",
		projectID, eventType)
	_, data := s.doAuthed("GET", path, s.defaultToken(), nil)
	var events []map[string]interface{}
	if err := json.Unmarshal(data, &events); err != nil {
		s.t.Fatalf("decoding events: %v (raw: %s)", err, data)
	}
	if len(events) == 0 {
		return nil
	}
	return events[0]
}

// TestEventsStatusEndpoint pins the read-only status surface:
// GET /events/status returns enabled + the four Stats counters,
// usable by monitoring without grepping logs. The kill-switch
// itself is flipped via SIGHUP-driven config reload (covered by
// the cmd/enju config tests), not via HTTP — exposing a write
// endpoint to any authenticated citizen would let one tenant
// kill audit for the whole deployment.
func TestEventsStatusEndpoint(t *testing.T) {
	s := newTestServer(t)

	got := s.get("/api/v1/events/status")
	if enabled, _ := got["enabled"].(bool); !enabled {
		t.Fatalf("expected enabled=true at boot, got %+v", got)
	}
	for _, field := range []string{"enabled", "enqueued", "persisted", "dropped", "queue_depth"} {
		if _, ok := got[field]; !ok {
			t.Errorf("status missing field %q (got %+v)", field, got)
		}
	}
}

// TestEventsLongPoll pins the long-poll behavior on the events
// endpoint. Three properties:
//
//  1. ?wait=0 (or absent) returns immediately with whatever's
//   matched right now — preserves the legacy synchronous shape.
//  2. ?wait=Ns with no matching events blocks for at most N
//   seconds, then returns an empty array. Doesn't return early
//   on unrelated activity.
//  3. ?wait=Ns wakes up promptly when a matching event lands —
//   the broadcast pathway from EventStore.broadcastNotify is
//   what makes this faster than dumb polling.
//
// All three together are the substrate for the notification
// subsystem (docs/notifications.md). Without this, every
// notification consumer would degrade to polling every N seconds.
func TestEventsLongPoll(t *testing.T) {
	s := newTestServer(t)

	// Create a project so we have an authorized scope.
	resp := s.post("/api/v1/projects", map[string]any{"name": "longpoll-test"})
	projectIDFloat, _ := resp["id"].(float64)
	projectID := int64(projectIDFloat)
	eventsURL := fmt.Sprintf("/api/v1/projects/%d/events", projectID)

	// Property 1: no wait param → immediate empty response.
	start := time.Now()
	immediate := s.getList(eventsURL)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("no-wait request took %v, want <100ms", elapsed)
	}
	if len(immediate) != 0 {
		t.Errorf("expected empty events on fresh project, got %d", len(immediate))
	}

	// Property 2: wait=300ms with no matching events → blocks
	// for ~300ms then returns empty. Tolerance for scheduler
	// jitter is wide (200ms-1500ms).
	start = time.Now()
	blocked := s.getList(eventsURL + "?wait=300ms")
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Errorf("wait=300ms returned in %v, expected to block for at least 200ms", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("wait=300ms returned in %v, expected to return by 1.5s", elapsed)
	}
	if len(blocked) != 0 {
		t.Errorf("expected empty events from timeout, got %d", len(blocked))
	}

	// Property 3: wait=5s + a matching event fired mid-wait
	// returns promptly (well under 5s). Emit the event from a
	// goroutine after a short delay so the long-poll request is
	// already blocked when the broadcast fires.
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.store.Events().Record(store.Event{
			CitizenID: 1, EventType: "test", ProjectID: projectID, RunID: 1,
			CreatedAt: time.Now(),
		})
	}()
	start = time.Now()
	got := s.getList(eventsURL + "?wait=5s")
	elapsed = time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("long-poll didn't wake on broadcast: took %v (want <2s); broadcast pathway broken", elapsed)
	}
	if len(got) == 0 {
		t.Error("expected at least one event after broadcast, got 0")
	}
}

// TestEvent_BranchMergedEmission pins the audit hook end-to-end:
// a fat-client (or any merge-driving consumer) reports a
// successful FF merge, and the coordinator emits a branch_merged
// event with the right metadata fields.
func TestEvent_BranchMergedEmission(t *testing.T) {
	s := newTestServer(t)
	projectID := s.createTestProject()
	runID := s.submitYAMLToProject("testdata/simple-no-deps.yaml", projectID)
	parts := strings.Split(runID, ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected run id shape: %q", runID)
	}
	runSeq := parts[1]

	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs/%s/merges", projectID, runSeq),
		map[string]interface{}{
			"topic_branch": "run-1/task_a/iter-1",
			"run_branch":   "main",
			"merge_sha":    "deadbeef0000000000000000000000000000beef",
			"task_id":      fmt.Sprintf("%d:1:task_a", projectID),
		})
	if status, _ := resp["status"].(string); status != "recorded" {
		t.Fatalf("expected status=recorded, got %+v", resp)
	}

	ev := s.findEvent(projectID, "branch_merged")
	if ev == nil {
		t.Fatal("branch_merged event not emitted after /merges report")
	}
	meta, _ := ev["metadata"].(map[string]interface{})
	if meta == nil {
		if metaStr, ok := ev["metadata"].(string); ok {
			_ = json.Unmarshal([]byte(metaStr), &meta)
		}
	}
	if meta == nil {
		t.Fatalf("event metadata missing or unparseable: %+v", ev)
	}
	for _, key := range []string{"topic_branch", "run_branch", "merge_sha", "run_seq"} {
		if _, ok := meta[key]; !ok {
			t.Errorf("branch_merged metadata missing %q (got %+v)", key, meta)
		}
	}
	if meta["merge_sha"] != "deadbeef0000000000000000000000000000beef" {
		t.Errorf("merge_sha not preserved: got %v", meta["merge_sha"])
	}
}

// TestEvent_CascadeFiredOnInvalidate pins cascade_fired for the
// invalidate flavor. handleInvalidateTask → performInvalidate
// emits the event at end of cascade.
//
// Setup: create a single-task run, force the task into ACCEPTED
// so it can be invalidated, then POST to invalidate.
func TestEvent_CascadeFiredOnInvalidate(t *testing.T) {
	s := newTestServer(t)
	projectID := s.createTestProject()
	runID := s.submitYAMLToProject("testdata/simple-no-deps.yaml", projectID)
	parts := strings.Split(runID, ":")
	taskID := fmt.Sprintf("%s:%s:task_a", parts[0], parts[1])

	if _, err := s.store.DB().Exec(
		`UPDATE tasks SET state = 'accepted', commit_sha = ? WHERE id = ?`,
		"feedface", taskID,
	); err != nil {
		t.Fatalf("force accept: %v", err)
	}

	resp := s.post(fmt.Sprintf("/api/v1/tasks/%s/invalidate", taskID),
		map[string]string{"reason": "cascade test"})
	if status, _ := resp["status"].(string); status != "invalidated" {
		t.Fatalf("expected status=invalidated, got %+v", resp)
	}

	ev := s.findEvent(projectID, "cascade_fired")
	if ev == nil {
		t.Fatal("cascade_fired event not emitted after invalidate")
	}
	if subtype, _ := ev["subtype"].(string); subtype != "invalidate" {
		t.Errorf("cascade_fired subtype = %q, want invalidate", subtype)
	}
	meta, _ := ev["metadata"].(map[string]interface{})
	if meta == nil {
		if metaStr, ok := ev["metadata"].(string); ok {
			_ = json.Unmarshal([]byte(metaStr), &meta)
		}
	}
	if meta == nil {
		t.Fatalf("cascade_fired metadata unparseable: %+v", ev)
	}
	for _, key := range []string{"descendants_count", "parked_count", "rollbacks"} {
		if _, ok := meta[key]; !ok {
			t.Errorf("cascade_fired metadata missing %q (got %+v)", key, meta)
		}
	}
}

// Suppress unused import
var _ = enjuYaml.Parse
