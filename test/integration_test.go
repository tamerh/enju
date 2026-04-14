package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/api"
	enjuGit "github.com/enju-ai/enju/internal/git"
	"github.com/enju-ai/enju/internal/store"
	enjuYaml "github.com/enju-ai/enju/internal/yaml"
)

// TestMain cleans the shared output directory before running tests.
func TestMain(m *testing.M) {
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

// testServer wraps a running Enju coordinator for testing.
type testServer struct {
	t             *testing.T
	server        *httptest.Server
	url           string
	gitBaseDir    string       // base directory containing per-project git repos
	store         *store.Store // direct store access for testing reaper/internals
	lastRunID     string       // "projectID:runSeq" of last submitted run
	lastProjectID int64
	lastRunSeq    int
}

// projectRepoDir returns the on-disk directory of a project's git repo.
func (s *testServer) projectRepoDir(projectID int64) string {
	return filepath.Join(s.gitBaseDir, fmt.Sprintf("%d", projectID))
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
	gitBaseDir := filepath.Join(testDir, "results")

	// Ensure the symlink exists: test/output -> /tmp/enju-test-output
	outputLink := filepath.Join(".", "output")
	if target, err := os.Readlink(outputLink); err != nil || target != testOutputBase {
		os.Remove(outputLink)
		os.Symlink(testOutputBase, outputLink)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	registry, err := enjuGit.NewRegistry(gitBaseDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	srv := api.NewServer(st, registry, logger)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	return &testServer{t: t, server: ts, url: ts.URL, gitBaseDir: gitBaseDir, store: st}
}

func (s *testServer) get(path string) map[string]interface{} {
	s.t.Helper()
	resp, err := http.Get(s.url + path)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result
}

func (s *testServer) getList(path string) []interface{} {
	s.t.Helper()
	resp, err := http.Get(s.url + path)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var result []interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result
}

func (s *testServer) post(path string, body interface{}) map[string]interface{} {
	s.t.Helper()
	jsonBody, _ := json.Marshal(body)
	resp, err := http.Post(s.url+path, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
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
	return username
}

func (s *testServer) registerWithEmail(name, email string) string {
	s.t.Helper()
	resp := s.post("/api/v1/citizens/register", map[string]string{"name": name, "email": email})
	username, ok := resp["username"].(string)
	if !ok {
		s.t.Fatalf("register failed: %v", resp)
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

// createTestProject creates a fresh test project per call (unique name to avoid conflicts).
func (s *testServer) createTestProject() int64 {
	s.t.Helper()
	// Unique name — timestamp + counter-ish from test server
	name := fmt.Sprintf("test-%d", time.Now().UnixNano())
	resp := s.post("/api/v1/projects", map[string]string{"name": name})
	id, _ := resp["id"].(float64)
	if id == 0 {
		s.t.Fatalf("failed to create test project: %v", resp)
	}
	return int64(id)
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
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", projectID), map[string]string{
		"yaml": string(data),
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

func (s *testServer) submit(taskID, content string) map[string]interface{} {
	s.t.Helper()
	return s.post("/api/v1/tasks/"+s.taskID(taskID)+"/result", map[string]interface{}{
		"content":     content,
		"result_type": "text",
		"tokens_used": 100,
	})
}

// submitWithArtifacts submits a result that also writes one or more artifacts.
// Returns the raw response (which includes "artifacts_written" on success).
func (s *testServer) submitWithArtifacts(taskID, content string, artifacts map[string]string) map[string]interface{} {
	s.t.Helper()
	return s.post("/api/v1/tasks/"+s.taskID(taskID)+"/result", map[string]interface{}{
		"content":     content,
		"artifacts":   artifacts,
		"tokens_used": 100,
	})
}

// readArtifactFile reads an artifact file directly from the project's
// repo on disk, returning the content and whether the file exists.
func (s *testServer) readArtifactFile(projectID int64, path string) (string, bool) {
	s.t.Helper()
	full := filepath.Join(s.projectRepoDir(projectID), "artifacts", path)
	data, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func (s *testServer) submitOutputs(taskID string, outputs map[string]string) map[string]interface{} {
	s.t.Helper()
	taskID = s.taskID(taskID)
	return s.post("/api/v1/tasks/"+taskID+"/result", map[string]interface{}{
		"outputs":     outputs,
		"tokens_used": 100,
	})
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

func (s *testServer) taskInputs(taskID string) map[string]interface{} {
	s.t.Helper()
	return s.get("/api/v1/tasks/" + s.taskID(taskID) + "/inputs")
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

// assertResultFile checks that a result file exists and contains expected content.
// runID is a composite "projectID:runSeq" string. Results live under
// {gitBaseDir}/{projectID}/runs/{runSeq}/{instanceKey?}/{taskDefID}/.
func (s *testServer) assertResultFile(runID, instanceKey, taskDefID, expectedContent string) {
	s.t.Helper()
	parts := strings.SplitN(runID, ":", 2)
	if len(parts) != 2 {
		s.t.Fatalf("assertResultFile: bad runID %q (want projectID:runSeq)", runID)
	}
	projectIDStr, runSeqStr := parts[0], parts[1]

	dir := filepath.Join(s.gitBaseDir, projectIDStr, "runs", runSeqStr)
	if instanceKey != "" {
		dir = filepath.Join(dir, instanceKey, taskDefID)
	} else {
		dir = filepath.Join(dir, taskDefID)
	}

	resultPath := filepath.Join(dir, "result.md")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		s.t.Fatalf("result file not found: %s", resultPath)
	}
	content := string(data)
	if expectedContent != "" && !strings.Contains(content, expectedContent) {
		s.t.Fatalf("result file %s: expected to contain %q, got %q", resultPath, expectedContent, content)
	}

	metaPath := filepath.Join(dir, "metadata.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		s.t.Fatalf("metadata file not found: %s", metaPath)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		s.t.Fatalf("invalid metadata JSON: %v", err)
	}
	if meta["task_id"] == nil {
		s.t.Fatal("metadata missing task_id")
	}
	if meta["citizen"] == nil {
		s.t.Fatal("metadata missing citizen")
	}
}

// assertGitCommits checks the number of commits in a project's repo.
// projectID is the project to inspect; under per-project repos each
// project has its own history.
func (s *testServer) assertGitCommits(projectID int64, expected int) {
	s.t.Helper()
	repoDir := s.projectRepoDir(projectID)
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		s.t.Fatalf("no git repo at %s: %v", repoDir, err)
	}
	cmd := exec.Command("git", "-C", repoDir, "log", "--oneline")
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatalf("git log failed: %v", err)
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

func TestSimpleNoDeps(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.submitYAML("testdata/simple-no-deps.yaml")

	// Both tasks should be ready (no dependencies)
	ready := s.readyTasks(pid)
	if len(ready) != 2 {
		t.Fatalf("expected 2 ready tasks, got %d", len(ready))
	}

	// Claim and submit both
	s.claim("task_a", alice)
	a1 := answer(t, "List 3 colors.", "Red, Blue, Green")
	r1 := s.submit("task_a", a1)
	if r1["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", r1)
	}

	s.claim("task_b", alice)
	a2 := answer(t, "List 3 animals.", "Cat, Dog, Fish")
	r2 := s.submit("task_b", a2)
	if r2["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", r2)
	}

	// Run should be completed
	status := s.runStatus(pid)
	if status["state"] != "completed" {
		t.Fatalf("expected completed, got %v", status["state"])
	}
}

func TestBranchingWithInferredDeps(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	pid := s.submitYAML("testdata/branching.yaml")

	// python_pros and go_pros ready, comparison blocked
	ready := s.readyTasks(pid)
	if len(ready) != 2 {
		t.Fatalf("expected 2 ready, got %d", len(ready))
	}

	// Verify comparison has inferred dependencies
	compTask := s.taskGet("comparison")
	depsStr, _ := compTask["depends_on"].(string)
	if depsStr == "" {
		t.Fatal("comparison should have inferred depends_on")
	}

	// Alice does python_pros
	s.claim("python_pros", alice)
	a1 := answer(t,
		"List 3 advantages of Python for CLI tools. One sentence each.",
		"1. Rich ecosystem. 2. Fast prototyping. 3. Easy distribution.")
	r1 := s.submit("python_pros", a1)
	if r1["newly_ready"] != float64(0) {
		t.Fatalf("comparison should not be ready yet, newly_ready=%v", r1["newly_ready"])
	}

	// Bob does go_pros
	s.claim("go_pros", bob)
	a2 := answer(t,
		"List 3 advantages of Go for CLI tools. One sentence each.",
		"1. Single binary. 2. Native concurrency. 3. Fast startup.")
	r2 := s.submit("go_pros", a2)
	if r2["newly_ready"] != float64(1) {
		t.Fatalf("comparison should be ready now, newly_ready=%v", r2["newly_ready"])
	}

	// Check template resolution
	inputs := s.taskInputs("comparison")
	resolved, _ := inputs["resolved_prompt"].(string)
	if resolved == "" {
		t.Fatal("expected resolved prompt")
	}
	if strings.Contains(resolved, "{{python_pros") {
		t.Fatalf("unresolved placeholder in: %s", resolved)
	}
	if !strings.Contains(resolved, a1) && !llmMode() {
		t.Fatalf("expected upstream content in resolved prompt: %s", resolved)
	}

	// Complete comparison — in LLM mode, use the resolved prompt
	s.claim("comparison", alice)
	a3 := answer(t,
		resolved,
		"Go wins for CLI distribution.")
	r3 := s.submit("comparison", a3)

	if r3["run_completed"] != true {
		t.Fatalf("expected run_completed, got %v", r3)
	}

	status := s.runStatus(pid)
	if status["state"] != "completed" {
		t.Fatalf("expected completed, got %v", status["state"])
	}

	// Verify output files
	s.assertResultFile(pid, "", "python_pros", "ecosystem")
	s.assertResultFile(pid, "", "go_pros", "binary")
	s.assertResultFile(pid, "", "comparison", "")
	// 4 commits: 1 initial (README) + 3 task results
	s.assertGitCommits(s.lastProjectID, 4)
}

func TestForEachExpansion(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.submitYAML("testdata/for-each.yaml")

	// 3 languages × 2 tasks = 6 tasks
	tasks := s.runTasks(pid)
	if len(tasks) != 6 {
		t.Fatalf("expected 6 tasks, got %d", len(tasks))
	}

	// 3 root tasks ready (one per language)
	ready := s.readyTasks(pid)
	if len(ready) != 3 {
		t.Fatalf("expected 3 ready, got %d", len(ready))
	}

	// Complete all tasks — in LLM mode each gets a real answer
	langs := []string{"Python", "Go", "Rust"}
	cannedPros := map[string]string{
		"Python": "1. Easy to learn. 2. Large ecosystem.",
		"Go":     "1. Fast compilation. 2. Great concurrency.",
		"Rust":   "1. Memory safety. 2. Zero-cost abstractions.",
	}
	cannedSummary := map[string]string{
		"Python": "Python is beginner-friendly with many libraries.",
		"Go":     "Go excels at concurrent systems.",
		"Rust":   "Rust guarantees safety without GC.",
	}

	for _, lang := range langs {
		taskID := lang + ":pros"
		s.claim(taskID, alice)
		a := answer(t,
			"List 2 advantages of "+lang+".",
			cannedPros[lang])
		s.submit(taskID, a)

		summaryID := lang + ":summary"
		s.claim(summaryID, alice)

		// For summary, get the resolved prompt
		inputs := s.taskInputs(summaryID)
		resolvedPrompt, _ := inputs["resolved_prompt"].(string)

		// Verify {{lang}} was resolved at creation time
		summaryTask := s.taskGet(summaryID)
		prompt, _ := summaryTask["prompt"].(string)
		if strings.Contains(prompt, "{{lang}}") {
			t.Fatalf("{{lang}} should be resolved: %s", prompt)
		}

		a2 := answer(t, resolvedPrompt, cannedSummary[lang])
		r := s.submit(summaryID, a2)

		if lang == "Rust" {
			if r["run_completed"] != true {
				t.Fatal("expected run_completed after all 6 tasks")
			}
		}
	}
}

func TestLLMFullPipeline(t *testing.T) {
	if !llmMode() {
		t.Skip("skipping LLM pipeline test (set ENJU_LLM_TEST=1)")
	}

	s := newTestServer(t)
	alice := s.register("researcher")
	bob := s.register("reviewer")

	// Read run description from text file
	runDesc, err := os.ReadFile("problems/microservices-vs-monolith.txt")
	if err != nil {
		t.Fatal(err)
	}

	// LLM generates the run YAML from natural language
	yamlPrompt := `You are generating an Enju run definition in YAML format.

The user describes a run in natural language. Convert it to Enju YAML.

Enju YAML format:
- name: run name
- version: 1
- tasks: list of tasks, each with id, type (always "llm_prompt"), and prompt
- Tasks can reference upstream results using {{task_id.content}} in their prompt
- Dependencies are inferred automatically from these references

Return ONLY valid YAML. No explanation, no markdown fences, no extra text.

User's run description:
` + string(runDesc)

	yamlContent := answer(t, yamlPrompt, "")

	// Clean up common LLM output issues
	yamlContent = cleanYAML(yamlContent)
	t.Logf("Generated YAML:\n%s", yamlContent)

	// Validate YAML parses before submitting
	_, err = enjuYaml.Parse([]byte(yamlContent))
	if err != nil {
		t.Fatalf("LLM generated invalid YAML: %v\n\nRaw YAML:\n%s", err, yamlContent)
	}
	t.Log("YAML validation passed")

	// Create a project for this run
	projectID := s.createTestProject()

	// Submit the validated YAML
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", projectID), map[string]string{"yaml": yamlContent})
	pid, ok := resp["id"].(string)
	if !ok {
		t.Fatalf("submit LLM YAML failed: %v", resp)
	}
	t.Logf("Run ID: %s, Tasks: %v", pid, resp["task_count"])

	// Work through all tasks
	for {
		ready := s.readyTasks(pid)
		if len(ready) == 0 {
			break
		}

		for _, task := range ready {
			tm, _ := task.(map[string]interface{})
			taskID, _ := tm["id"].(string)
			prompt, _ := tm["prompt"].(string)

			// Alternate between citizens
			citizen := alice
			if len(taskID)%2 == 0 {
				citizen = bob
			}

			s.claim(taskID, citizen)

			// Get resolved prompt if task has dependencies
			if deps, _ := tm["depends_on"].(string); deps != "" {
				inputs := s.taskInputs(taskID)
				if rp, ok := inputs["resolved_prompt"].(string); ok && rp != "" {
					prompt = rp
				}
			}

			a := answer(t, prompt, "")
			s.submit(taskID, a)
			t.Logf("Completed: %s (%d chars)", taskID, len(a))
		}
	}

	// Verify completion
	status := s.runStatus(pid)
	if status["state"] != "completed" {
		t.Fatalf("expected completed, got %v", status["state"])
	}
	t.Log("LLM pipeline completed successfully")
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

func TestMultipleCitizensCollaborate(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")
	charlie := s.register("charlie")

	pid := s.submitYAML("testdata/branching.yaml")

	s.claim("python_pros", alice)
	s.claim("go_pros", bob)

	s.submit("python_pros", answer(t,
		"List 3 advantages of Python for CLI tools.",
		"Python advantages here"))
	s.submit("go_pros", answer(t,
		"List 3 advantages of Go for CLI tools.",
		"Go advantages here"))

	s.claim("comparison", charlie)
	inputs := s.taskInputs("comparison")
	resolved, _ := inputs["resolved_prompt"].(string)
	r := s.submit("comparison", answer(t, resolved, "Final comparison"))

	if r["run_completed"] != true {
		t.Fatal("expected run completed")
	}

	// Verify all three citizens contributed
	tasks := s.runTasks(pid)
	citizens := make(map[string]bool)
	for _, task := range tasks {
		tm, _ := task.(map[string]interface{})
		if cb, ok := tm["claimed_by"].(string); ok && cb != "" {
			citizens[cb] = true
		}
	}
	if len(citizens) != 3 {
		t.Fatalf("expected 3 different citizens, got %d", len(citizens))
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

func TestCitizenDashboard(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	// Dashboard before any work
	dash := s.get("/api/v1/citizens/by-username/" + alice + "/dashboard")
	citizen, _ := dash["citizen"].(map[string]interface{})
	if citizen["name"] != "alice" {
		t.Fatalf("expected alice, got %v", citizen["name"])
	}
	if citizen["tasks_completed"] != float64(0) {
		t.Fatalf("expected 0 completed, got %v", citizen["tasks_completed"])
	}

	// Do some work
	s.submitYAML("testdata/simple-no-deps.yaml")
	s.claim("task_a", alice)
	s.submit("task_a", "Red, Blue, Green")

	// Dashboard after completing a task
	dash = s.get("/api/v1/citizens/by-username/" + alice + "/dashboard")
	citizen, _ = dash["citizen"].(map[string]interface{})
	if citizen["tasks_completed"] != float64(1) {
		t.Fatalf("expected 1 completed, got %v", citizen["tasks_completed"])
	}

	// Recent tasks should have task_a
	recent, _ := dash["recent_tasks"].([]interface{})
	if len(recent) != 1 {
		t.Fatalf("expected 1 recent task, got %d", len(recent))
	}

	// Claim another task — should show in active
	s.claim("task_b", alice)
	dash = s.get("/api/v1/citizens/by-username/" + alice + "/dashboard")
	active, _ := dash["active_tasks"].([]interface{})
	if len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(active))
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

func TestActionField(t *testing.T) {
	s := newTestServer(t)

	s.submitYAML("testdata/branching.yaml")

	task := s.taskGet("python_pros")
	action, _ := task["action"].(string)
	if action != "answer" {
		t.Fatalf("expected action 'answer', got %q", action)
	}

	// Should not have type or mode fields
	if _, hasType := task["type"]; hasType {
		t.Fatal("should not have 'type' field")
	}
	if _, hasMode := task["mode"]; hasMode {
		t.Fatal("should not have 'mode' field")
	}
}

func TestNamedOutputs(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	s.submitYAML("testdata/named-outputs.yaml")

	// Task detail should show outputs schema
	task := s.taskGet("gene_analysis")
	outputs, _ := task["outputs"].(string)
	if outputs == "" {
		t.Fatal("expected outputs schema on gene_analysis task")
	}
	if !strings.Contains(outputs, "gene_list") || !strings.Contains(outputs, "pathways") {
		t.Fatalf("outputs schema should contain expected fields: %s", outputs)
	}

	// Claim and submit with named outputs
	s.claim("gene_analysis", alice)
	result := s.submitOutputs("gene_analysis", map[string]string{
		"gene_list": "BRCA1, TP53, EGFR",
		"pathways":  "KEGG:hsa04110, KEGG:hsa04115",
		"stats":     "50 genes, p<0.01",
	})
	if result["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", result)
	}

	// Downstream task should see specific field
	inputs := s.taskInputs("drug_targets")
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "BRCA1") {
		t.Fatalf("drug_targets resolved prompt should contain gene_list content, got: %s", resolved)
	}
	// Should NOT contain pathways (which is in a different field)
	if strings.Contains(resolved, "KEGG") {
		t.Fatalf("drug_targets should only see gene_list, not pathways: %s", resolved)
	}

	// Different downstream task sees different field
	pathwayInputs := s.taskInputs("pathway_viz")
	pathwayResolved, _ := pathwayInputs["resolved_prompt"].(string)
	if !strings.Contains(pathwayResolved, "KEGG") {
		t.Fatalf("pathway_viz resolved prompt should contain pathways content, got: %s", pathwayResolved)
	}
	if strings.Contains(pathwayResolved, "BRCA1") {
		t.Fatalf("pathway_viz should only see pathways, not gene_list: %s", pathwayResolved)
	}
}

func TestFreshDBHasNoProjects(t *testing.T) {
	s := newTestServer(t)

	projects := s.getList("/api/v1/projects")
	if len(projects) != 0 {
		t.Fatalf("expected 0 projects on fresh DB, got %d", len(projects))
	}
}

func TestCreateProject(t *testing.T) {
	s := newTestServer(t)

	resp := s.post("/api/v1/projects", map[string]string{
		"name":        "Drug Target Discovery",
		"description": "Long-lived project for drug target analyses",
	})
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("create project failed: %v", resp)
	}

	// Duplicate name should fail
	dup := s.post("/api/v1/projects", map[string]string{"name": "Drug Target Discovery"})
	if _, hasErr := dup["error"]; !hasErr {
		t.Fatal("expected error for duplicate project name")
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

func TestRunInProject(t *testing.T) {
	s := newTestServer(t)

	// Create a project
	pj := s.post("/api/v1/projects", map[string]string{"name": "Bioinformatics Research"})
	projectID, _ := pj["id"].(float64)
	if projectID == 0 {
		t.Fatalf("project creation failed: %v", pj)
	}

	// Submit a run within this project
	yamlData, _ := os.ReadFile("testdata/simple-no-deps.yaml")
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", int64(projectID)), map[string]string{
		"yaml": string(yamlData),
	})

	runProjectID, _ := resp["project_id"].(float64)
	if int64(runProjectID) != int64(projectID) {
		t.Fatalf("expected project_id=%d, got %v", int64(projectID), runProjectID)
	}

	// First run in a new project should have seq=1
	seq, _ := resp["seq"].(float64)
	if int(seq) != 1 {
		t.Fatalf("expected run seq=1, got %v", seq)
	}

	// List runs by project
	runs := s.getList(fmt.Sprintf("/api/v1/projects/%d/runs", int64(projectID)))
	if len(runs) != 1 {
		t.Fatalf("expected 1 run in project, got %d", len(runs))
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

	// Project A gets runs #1, #2
	r1 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid1), map[string]string{"yaml": string(yamlData)})
	seq1, _ := r1["seq"].(float64)
	r2 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid1), map[string]string{"yaml": string(yamlData)})
	seq2, _ := r2["seq"].(float64)

	if int(seq1) != 1 || int(seq2) != 2 {
		t.Fatalf("expected seq 1,2 in project A, got %d,%d", int(seq1), int(seq2))
	}

	// Project B also starts from #1 (independent numbering)
	r3 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid2), map[string]string{"yaml": string(yamlData)})
	seq3, _ := r3["seq"].(float64)

	if int(seq3) != 1 {
		t.Fatalf("expected seq 1 in new project B, got %d", int(seq3))
	}
}

func TestEnvironmentRequirements(t *testing.T) {
	s := newTestServer(t)

	s.submitYAML("testdata/requirements.yaml")

	// Task with inherited run-level requirements
	simple := s.taskGet("simple_task")
	simpleReqs, _ := simple["requirements"].(string)
	if simpleReqs == "" {
		t.Fatal("simple_task should have inherited requirements")
	}
	if !strings.Contains(simpleReqs, "python") {
		t.Fatalf("simple_task requirements should contain python: %s", simpleReqs)
	}
	if !strings.Contains(simpleReqs, "pandas") {
		t.Fatalf("simple_task requirements should contain pandas: %s", simpleReqs)
	}

	// Task with its own requirements (overrides run-level)
	custom := s.taskGet("custom_task")
	customReqs, _ := custom["requirements"].(string)
	if customReqs == "" {
		t.Fatal("custom_task should have its own requirements")
	}
	if !strings.Contains(customReqs, "node") {
		t.Fatalf("custom_task should have node: %s", customReqs)
	}
	if !strings.Contains(customReqs, "chembl") {
		t.Fatalf("custom_task should have chembl: %s", customReqs)
	}
	// Task-level replaces run-level entirely
	if strings.Contains(customReqs, "pandas") {
		t.Fatalf("custom_task should NOT inherit pandas (task-level replaces): %s", customReqs)
	}

	// Task with explicit empty requirements: {} — opts out of run-level
	noReqs := s.taskGet("no_reqs_task")
	noReqsVal, _ := noReqs["requirements"].(string)
	if noReqsVal != "" {
		t.Fatalf("no_reqs_task should have empty requirements (opted out), got: %s", noReqsVal)
	}
}

func TestMultiFileOutputs(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	s.submitYAML("testdata/multi-file-outputs.yaml")

	// Claim and submit with named outputs — each goes to its own file
	s.claim("analyze", alice)
	result := s.submitOutputs("analyze", map[string]string{
		"gene_list": "gene,score\nBRCA1,0.95\nTP53,0.87",
		"pathways":  `{"nodes":["BRCA1","TP53"],"edges":[]}`,
		"summary":   "# Analysis\n\nFound 2 genes.",
	})
	if result["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", result)
	}

	// Verify files exist on disk with correct content.
	// Per-project repo layout: {gitBaseDir}/{projectID}/runs/{runSeq}/analyze
	resultsDir := filepath.Join(
		s.projectRepoDir(s.lastProjectID),
		"runs",
		fmt.Sprintf("%d", s.lastRunSeq),
		"analyze")

	// genes.csv should contain the CSV
	csvData, err := os.ReadFile(filepath.Join(resultsDir, "genes.csv"))
	if err != nil {
		t.Fatalf("genes.csv not found: %v", err)
	}
	if !strings.Contains(string(csvData), "BRCA1") {
		t.Fatalf("genes.csv missing content: %s", csvData)
	}

	// pathways.json
	jsonData, err := os.ReadFile(filepath.Join(resultsDir, "pathways.json"))
	if err != nil {
		t.Fatalf("pathways.json not found: %v", err)
	}
	if !strings.Contains(string(jsonData), "nodes") {
		t.Fatalf("pathways.json missing content: %s", jsonData)
	}

	// summary.md
	mdData, err := os.ReadFile(filepath.Join(resultsDir, "summary.md"))
	if err != nil {
		t.Fatalf("summary.md not found: %v", err)
	}
	if !strings.Contains(string(mdData), "Analysis") {
		t.Fatalf("summary.md missing content: %s", mdData)
	}

	// metadata.json should have file index
	metaData, err := os.ReadFile(filepath.Join(resultsDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metaData), "output_files") {
		t.Fatalf("metadata missing output_files: %s", metaData)
	}
	if !strings.Contains(string(metaData), "genes.csv") {
		t.Fatalf("metadata missing genes.csv reference: %s", metaData)
	}

	// Downstream: use_genes should see the gene list content
	inputs := s.taskInputs("use_genes")
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "BRCA1") {
		t.Fatalf("use_genes should see gene_list content, got: %s", resolved)
	}
	if strings.Contains(resolved, "nodes") {
		t.Fatalf("use_genes should not see pathways content: %s", resolved)
	}

	// use_pathways should see the pathways content
	pathwayInputs := s.taskInputs("use_pathways")
	pathwayResolved, _ := pathwayInputs["resolved_prompt"].(string)
	if !strings.Contains(pathwayResolved, "nodes") {
		t.Fatalf("use_pathways should see pathways content, got: %s", pathwayResolved)
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

func TestUpdateProfile(t *testing.T) {
	s := newTestServer(t)

	alice := s.registerWithEmail("Alice", "alice@example.com")

	// Update name
	resp := s.updateProfile(alice, "Alice Smith", "alice@example.com")
	if resp["status"] != "updated" {
		t.Fatalf("expected updated, got %v", resp)
	}

	// Verify change
	profile := s.getCitizen(alice)
	if profile["name"] != "Alice Smith" {
		t.Fatalf("expected Alice Smith, got %v", profile["name"])
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

// TestArtifactWriteAndRead exercises the full artifact lifecycle:
// bootstrap a file in run #1, refactor it via {{artifact:...}} in run #2,
// and confirm last-write-wins semantics + the inferred read.
func TestArtifactWriteAndRead(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	s.submitYAML("testdata/artifacts.yaml")

	// Bootstrap: writes src/hello.py from scratch.
	s.claim("bootstrap", alice)
	r1 := s.submitWithArtifacts("bootstrap", "Wrote initial hello.py.", map[string]string{
		"src/hello.py": "def main():\n    print(\"hi\")\n",
	})
	if r1["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", r1)
	}
	written, _ := r1["artifacts_written"].([]interface{})
	if len(written) != 1 || written[0] != "src/hello.py" {
		t.Fatalf("expected artifacts_written = [src/hello.py], got %v", r1["artifacts_written"])
	}

	// File should be on disk under the project repo's artifacts/ dir.
	content, ok := s.readArtifactFile(s.lastProjectID, "src/hello.py")
	if !ok {
		t.Fatal("artifact file missing on disk after write")
	}
	if !strings.Contains(content, "print(\"hi\")") {
		t.Fatalf("unexpected initial content: %q", content)
	}

	// Refactor: depends_on inference should NOT add a task dep, but
	// reads_artifacts should be inferred from the {{artifact:...}} ref,
	// so the inputs endpoint should return the current artifact content.
	inputs := s.taskInputs("refactor")
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "print(\"hi\")") {
		t.Fatalf("refactor prompt did not include current artifact content, got: %s", resolved)
	}
	// And artifacts map should be present.
	artifactsMap, _ := inputs["artifacts"].(map[string]interface{})
	if _, present := artifactsMap["src/hello.py"]; !present {
		t.Fatalf("expected artifacts map to include src/hello.py, got %v", artifactsMap)
	}

	// Submit refactor with new content (last-write-wins).
	s.claim("refactor", alice)
	r2 := s.submitWithArtifacts("refactor", "Refactored to use a constant.", map[string]string{
		"src/hello.py": "GREETING = \"hi\"\n\ndef main():\n    print(GREETING)\n",
	})
	if r2["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", r2)
	}
	if r2["run_completed"] != true {
		t.Fatalf("expected run completed after refactor, got %v", r2["run_completed"])
	}

	// On disk, the artifact should now hold the refactored version.
	content2, _ := s.readArtifactFile(s.lastProjectID, "src/hello.py")
	if !strings.Contains(content2, "GREETING") {
		t.Fatalf("artifact was not overwritten by refactor, got: %s", content2)
	}
}

// TestArtifactWriteRejectedForUndeclaredPath confirms the writes_artifacts
// allow-list is enforced — submitting a path the task didn't declare fails.
func TestArtifactWriteRejectedForUndeclaredPath(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	s.submitYAML("testdata/artifacts.yaml")
	s.claim("bootstrap", alice)

	resp := s.submitWithArtifacts("bootstrap", "trying to sneak", map[string]string{
		"src/sneaky.py": "print(\"oops\")",
	})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("expected error for undeclared artifact path, got %v", resp)
	}

	// Task should still be claimed (no state change on rejected submission).
	task := s.taskGet("bootstrap")
	if task["state"] != "claimed" {
		t.Fatalf("expected bootstrap to remain claimed after rejection, got %v", task["state"])
	}
}

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

// TestArtifactListAndGetEndpoints exercises the read-only REST API for
// artifacts and verifies the index row is populated correctly.
func TestArtifactListAndGetEndpoints(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	s.submitYAML("testdata/artifacts.yaml")
	s.claim("bootstrap", alice)
	s.submitWithArtifacts("bootstrap", "init", map[string]string{
		"src/hello.py": "print(\"hello\")\n",
	})

	// List
	listURL := fmt.Sprintf("/api/v1/projects/%d/artifacts", s.lastProjectID)
	list := s.getList(listURL)
	if len(list) != 1 {
		t.Fatalf("expected 1 artifact in list, got %d", len(list))
	}
	first, _ := list[0].(map[string]interface{})
	if first["path"] != "src/hello.py" {
		t.Fatalf("expected path src/hello.py, got %v", first["path"])
	}
	if first["last_writer"] != alice {
		t.Fatalf("expected last_writer %q, got %v", alice, first["last_writer"])
	}

	// Get one
	getURL := fmt.Sprintf("/api/v1/projects/%d/artifacts/src/hello.py", s.lastProjectID)
	a := s.get(getURL)
	if !strings.Contains(a["content"].(string), "hello") {
		t.Fatalf("expected content to contain 'hello', got %v", a["content"])
	}
	if a["last_writer"] != alice {
		t.Fatalf("expected last_writer %q, got %v", alice, a["last_writer"])
	}

	// Missing artifact returns an error
	missing := s.get(fmt.Sprintf("/api/v1/projects/%d/artifacts/does/not/exist.txt", s.lastProjectID))
	if _, hasErr := missing["error"]; !hasErr {
		t.Fatalf("expected error for missing artifact, got %v", missing)
	}
}

// TestArtifactFieldsInTaskResponse confirms reads_artifacts and
// writes_artifacts come back in the task JSON so the MCP formatters can
// surface them. Regression guard for the discoverability bug we hit
// during the first poke at the system.
func TestArtifactFieldsInTaskResponse(t *testing.T) {
	s := newTestServer(t)

	s.submitYAML("testdata/artifacts.yaml")

	// "refactor" reads + writes src/hello.py.
	task := s.taskGet("refactor")

	reads, _ := task["reads_artifacts"].([]interface{})
	writes, _ := task["writes_artifacts"].([]interface{})

	if len(writes) != 1 || writes[0] != "src/hello.py" {
		t.Fatalf("expected writes_artifacts = [src/hello.py], got %v", task["writes_artifacts"])
	}
	if len(reads) != 1 || reads[0] != "src/hello.py" {
		t.Fatalf("expected reads_artifacts = [src/hello.py] (inferred from prompt), got %v", task["reads_artifacts"])
	}
}

// TestArtifactPermissiveWrites confirms the citizen can omit declared
// artifacts — writes_artifacts is an upper bound, not a requirement.
func TestArtifactPermissiveWrites(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	// A run with two declared writes; we only submit one.
	yaml := `name: "Permissive writes"
version: 1
tasks:
  - id: maybe
    action: answer
    writes_artifacts:
      - src/a.py
      - src/b.py
    prompt: "Optionally update either or both files."
`
	pid := s.createTestProject()
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml})
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("submit failed: %v", resp)
	}
	s.lastProjectID = pid
	s.lastRunSeq = int(resp["seq"].(float64))

	s.claim("maybe", alice)
	r := s.submitWithArtifacts("maybe", "only updated one", map[string]string{
		"src/a.py": "# only this one\n",
	})
	if r["status"] != "accepted" {
		t.Fatalf("expected accepted, got %v", r)
	}

	// a.py should exist, b.py should not.
	if _, ok := s.readArtifactFile(s.lastProjectID, "src/a.py"); !ok {
		t.Fatal("expected src/a.py to be written")
	}
	if _, ok := s.readArtifactFile(s.lastProjectID, "src/b.py"); ok {
		t.Fatal("expected src/b.py NOT to be written")
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
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": validYAML})
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

	badResp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": badYAML})
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
	goodResp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": goodYAML})
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

// submitAccessControlYAML posts a run where __CITIZEN_X__ placeholders
// are replaced with real citizen usernames before submission. Returns
// the composite run ID in "projectID:runSeq" form.
func (s *testServer) submitAccessControlYAML(path string, aliceUsername, bobUsername string) string {
	s.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatal(err)
	}
	yaml := strings.ReplaceAll(string(data), "__CITIZEN_ALICE__", aliceUsername)
	yaml = strings.ReplaceAll(yaml, "__CITIZEN_BOB__", bobUsername)

	pid := s.createTestProject()
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml})
	seqFloat, _ := resp["seq"].(float64)
	if seqFloat == 0 {
		s.t.Fatalf("submit failed: %v", resp)
	}
	s.lastProjectID = pid
	s.lastRunSeq = int(seqFloat)
	s.lastRunID = fmt.Sprintf("%d:%d", pid, int(seqFloat))
	return s.lastRunID
}

// TestAccessControlDefaultOpen confirms the default behavior — tasks
// with neither assign_to nor require_role remain claimable by anyone.
// This is the GitHub-like default and the most important guarantee.
func TestAccessControlDefaultOpen(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	// Alice and Bob are both plain citizens. Neither has a role beyond
	// "citizen", but open_task has no restrictions — either can claim.
	charlie := s.register("charlie")
	resp := s.claim("open_task", charlie)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("open task should be claimable by any citizen, got error: %v", resp)
	}
}

// TestAccessControlAssignToAllowsListedCitizen confirms a claim from a
// citizen in the assign_to list succeeds.
func TestAccessControlAssignToAllowsListedCitizen(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	resp := s.claim("assigned_task", alice)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("alice should be able to claim her assigned task, got: %v", resp)
	}
	task, _ := resp["task"].(map[string]interface{})
	if task["state"] != "claimed" {
		t.Fatalf("expected claimed state, got %v", task["state"])
	}
}

// TestAccessControlAssignToRejectsOtherCitizen confirms a claim from a
// citizen NOT in the assign_to list is rejected with a clear error.
func TestAccessControlAssignToRejectsOtherCitizen(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	// Bob tries to claim Alice's task.
	resp := s.claim("assigned_task", bob)
	errMsg, hasErr := resp["error"].(string)
	if !hasErr {
		t.Fatalf("bob should NOT be able to claim alice's task, got: %v", resp)
	}
	if !strings.Contains(errMsg, "assigned") {
		t.Fatalf("expected error mentioning assignment, got: %s", errMsg)
	}

	// Task should still be in the 'ready' state — a rejected claim
	// must not change task state.
	task := s.taskGet("assigned_task")
	if task["state"] != "ready" {
		t.Fatalf("expected task to remain ready after rejected claim, got %v", task["state"])
	}
}

// --- Iteration 3: cascade invalidation ---

// TestInvalidateCascadesDownstream runs the full cascade flow end-to-end:
// complete all three tasks in branching.yaml, then invalidate python_pros
// and verify that (a) python_pros goes back to ready, (b) comparison
// (which depends on python_pros) drops to pending, (c) go_pros (which
// doesn't depend on python_pros) stays accepted untouched.
func TestInvalidateCascadesDownstream(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.submitYAML("testdata/branching.yaml")

	// Complete all three tasks.
	s.claim("python_pros", alice)
	s.submit("python_pros", "1. ecosystem. 2. simple. 3. fast proto.")
	s.claim("go_pros", alice)
	s.submit("go_pros", "1. single binary. 2. speed. 3. concurrency.")
	s.claim("comparison", alice)
	s.submit("comparison", "Use Go for CLI tools.")

	// Run should be completed.
	if status := s.runStatus(pid); status["state"] != "completed" {
		t.Fatalf("expected completed, got %v", status["state"])
	}

	// Invalidate python_pros with a reason.
	resp := s.invalidate("python_pros", "LLM hallucinated an advantage")
	if resp["status"] != "invalidated" {
		t.Fatalf("expected invalidated, got %v", resp)
	}
	if int(resp["changed"].(float64)) != 2 {
		t.Fatalf("expected 2 tasks changed (target + 1 descendant), got %v", resp["changed"])
	}
	desc, _ := resp["descendants"].([]interface{})
	if len(desc) != 1 {
		t.Fatalf("expected 1 descendant (comparison), got %d: %v", len(desc), desc)
	}
	// The descendant list uses fully-qualified task IDs.
	if !strings.HasSuffix(desc[0].(string), ":comparison") {
		t.Fatalf("expected descendant to be comparison, got %v", desc[0])
	}

	// State transitions:
	pp := s.taskGet("python_pros")
	if pp["state"] != "ready" {
		t.Fatalf("expected python_pros READY, got %v", pp["state"])
	}
	// Claim fields cleared.
	if pp["claimed_by"] != nil && pp["claimed_by"] != "" {
		t.Fatalf("expected python_pros.claimed_by cleared, got %v", pp["claimed_by"])
	}

	comp := s.taskGet("comparison")
	if comp["state"] != "pending" {
		t.Fatalf("expected comparison PENDING, got %v", comp["state"])
	}

	// go_pros was independent of python_pros — it should be untouched.
	gp := s.taskGet("go_pros")
	if gp["state"] != "accepted" {
		t.Fatalf("expected go_pros still ACCEPTED (not touched by cascade), got %v", gp["state"])
	}

	// Run should have flipped from completed back to active.
	if status := s.runStatus(pid); status["state"] != "active" {
		t.Fatalf("expected run active after invalidation, got %v", status["state"])
	}
}

// TestInvalidateAllowsReclaimAndProgression verifies the happy path
// after an invalidation: the target can be re-claimed and re-submitted,
// and downstream tasks get promoted back to ready as their upstreams
// re-complete.
func TestInvalidateAllowsReclaimAndProgression(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.submitYAML("testdata/branching.yaml")

	// Complete everything.
	s.claim("python_pros", alice)
	s.submit("python_pros", "original python answer")
	s.claim("go_pros", alice)
	s.submit("go_pros", "original go answer")
	s.claim("comparison", alice)
	s.submit("comparison", "original comparison")

	// Invalidate python_pros.
	s.invalidate("python_pros", "needs redo")

	// Re-claim python_pros and submit a corrected result.
	resp := s.claim("python_pros", alice)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("re-claim after invalidation failed: %v", resp)
	}
	submitResp := s.submit("python_pros", "corrected python answer")
	if submitResp["status"] != "accepted" {
		t.Fatalf("re-submit failed: %v", submitResp)
	}

	// comparison should now be READY (both upstreams accepted again).
	// Note: submitting python_pros should have auto-promoted comparison
	// back from PENDING to READY via UpdateReadyTasks.
	comp := s.taskGet("comparison")
	if comp["state"] != "ready" {
		t.Fatalf("expected comparison READY after upstream re-completes, got %v", comp["state"])
	}

	// Re-claim and finish comparison.
	s.claim("comparison", alice)
	final := s.submit("comparison", "updated comparison with corrected python info")
	if final["status"] != "accepted" {
		t.Fatalf("final comparison submission failed: %v", final)
	}

	// Run should be completed again.
	if status := s.runStatus(pid); status["state"] != "completed" {
		t.Fatalf("expected completed after re-run, got %v", status["state"])
	}
}

// TestInvalidateRollsBackArtifactToPriorWriter is the regression test
// for iteration 3.1's bug fix. Setup:
//   - Run #1 task write_v1 writes notes/intro.md = "version ONE"
//   - Run #2 task write_v2 reads + overwrites to "version TWO"
//   - Invalidate 1:2:write_v2
// Expected:
//   - notes/intro.md rolls back to "version ONE"
//   - list_artifacts shows 1:1:write_v1 as last writer
//   - Re-claiming 1:2:write_v2 sees v1 in its resolved prompt
func TestInvalidateRollsBackArtifactToPriorWriter(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.createTestProject()

	// Run 1: create the artifact.
	v1YAML := `name: "Writer v1"
version: 1
tasks:
  - id: write_v1
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write the first version."
`
	r1 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": v1YAML})
	if _, hasErr := r1["error"]; hasErr {
		t.Fatalf("r1 submit failed: %v", r1)
	}
	s.lastProjectID = pid
	s.lastRunSeq = 1

	s.claim("write_v1", alice)
	s.submitWithArtifacts("write_v1", "v1 result", map[string]string{
		"notes/intro.md": "version ONE",
	})

	// Confirm v1 is on disk.
	content1, ok := s.readArtifactFile(pid, "notes/intro.md")
	if !ok || content1 != "version ONE" {
		t.Fatalf("expected v1 on disk, got ok=%v content=%q", ok, content1)
	}

	// Run 2: read v1, overwrite with v2.
	v2YAML := `name: "Writer v2"
version: 1
tasks:
  - id: write_v2
    action: answer
    reads_artifacts: [notes/intro.md]
    writes_artifacts: [notes/intro.md]
    prompt: "Read {{artifact:notes/intro.md}} and replace with v2."
`
	r2 := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": v2YAML})
	if _, hasErr := r2["error"]; hasErr {
		t.Fatalf("r2 submit failed: %v", r2)
	}
	s.lastRunSeq = 2

	s.claim("write_v2", alice)
	s.submitWithArtifacts("write_v2", "v2 result", map[string]string{
		"notes/intro.md": "version TWO",
	})

	// Confirm v2 is on disk.
	content2, _ := s.readArtifactFile(pid, "notes/intro.md")
	if content2 != "version TWO" {
		t.Fatalf("expected v2 on disk, got %q", content2)
	}

	// Now invalidate write_v2. Rollback should restore notes/intro.md
	// to v1 — the state it was in before write_v2 ran.
	resp := s.invalidate("write_v2", "wrong direction")
	if resp["status"] != "invalidated" {
		t.Fatalf("invalidate failed: %v", resp)
	}

	// Check the response includes artifact rollback info.
	rolled, _ := resp["artifacts_rolled_back"].([]interface{})
	if len(rolled) != 1 {
		t.Fatalf("expected 1 artifact rolled back, got %v", resp["artifacts_rolled_back"])
	}
	rbEntry, _ := rolled[0].(map[string]interface{})
	if rbEntry["path"] != "notes/intro.md" {
		t.Fatalf("expected path notes/intro.md, got %v", rbEntry)
	}
	if rbEntry["deleted"] == true {
		t.Fatalf("expected restore, not delete, got %v", rbEntry)
	}
	restoredTask, _ := rbEntry["restored_from_task"].(string)
	if !strings.HasSuffix(restoredTask, ":write_v1") {
		t.Fatalf("expected restore from write_v1, got %v", restoredTask)
	}

	// On-disk content should be v1 again.
	contentAfter, ok := s.readArtifactFile(pid, "notes/intro.md")
	if !ok {
		t.Fatal("artifact missing after rollback")
	}
	if contentAfter != "version ONE" {
		t.Fatalf("expected v1 after rollback, got %q", contentAfter)
	}

	// Artifact index should now point to write_v1.
	list := s.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", pid))
	if len(list) != 1 {
		t.Fatalf("expected 1 artifact in index, got %d", len(list))
	}
	entry, _ := list[0].(map[string]interface{})
	lastTask, _ := entry["last_task_id"].(string)
	if !strings.HasSuffix(lastTask, ":write_v1") {
		t.Fatalf("expected last_task_id to be write_v1 after rollback, got %v", lastTask)
	}

	// Re-claim write_v2 and check its inputs block. It should now see
	// v1 content, not v2.
	s.claim("write_v2", alice)
	inputs := s.taskInputs("write_v2")
	artifacts, _ := inputs["artifacts"].(map[string]interface{})
	if artifacts["notes/intro.md"] != "version ONE" {
		t.Fatalf("re-claim should see v1 in artifacts map, got %v", artifacts["notes/intro.md"])
	}
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "version ONE") {
		t.Fatalf("re-claim resolved prompt should contain v1, got: %s", resolved)
	}
	if strings.Contains(resolved, "version TWO") {
		t.Fatalf("re-claim resolved prompt should NOT contain v2, got: %s", resolved)
	}
}

// TestInvalidateFirstWriterDeletesArtifact verifies the "no prior
// writer" case: if the invalidated task was the artifact's first
// writer, the artifact is deleted entirely on rollback.
func TestInvalidateFirstWriterDeletesArtifact(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.createTestProject()

	yaml := `name: "Creator"
version: 1
tasks:
  - id: create
    action: answer
    writes_artifacts: [config/settings.yaml]
    prompt: "Create the config."
`
	s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml})
	s.lastProjectID = pid
	s.lastRunSeq = 1

	s.claim("create", alice)
	s.submitWithArtifacts("create", "made config", map[string]string{
		"config/settings.yaml": "key: value",
	})

	// Sanity: file exists.
	if _, ok := s.readArtifactFile(pid, "config/settings.yaml"); !ok {
		t.Fatal("expected config file to exist before invalidation")
	}

	// Invalidate — no prior writer, expect deletion.
	resp := s.invalidate("create", "bad config")
	rolled, _ := resp["artifacts_rolled_back"].([]interface{})
	if len(rolled) != 1 {
		t.Fatalf("expected 1 artifact rolled back, got %v", resp["artifacts_rolled_back"])
	}
	rbEntry, _ := rolled[0].(map[string]interface{})
	if rbEntry["deleted"] != true {
		t.Fatalf("expected deletion (no prior writer), got %v", rbEntry)
	}

	// File should be gone from the working tree.
	if _, ok := s.readArtifactFile(pid, "config/settings.yaml"); ok {
		t.Fatal("expected config file to be deleted after rollback")
	}

	// Artifact index row should be gone.
	list := s.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", pid))
	if len(list) != 0 {
		t.Fatalf("expected empty artifact index after deletion, got %d entries", len(list))
	}
}

// TestInvalidateWalkerSkipsPreviouslyInvalidatedWriter is the
// regression test for the iteration 3.2 walker bug: rolling back an
// artifact must skip commits whose author task is currently in any
// non-ACCEPTED state, not just the task being invalidated right now.
//
// Scenario:
//   - write_v1 writes v1 (accepted)
//   - write_v2 writes v2 (accepted)
//   - Invalidate write_v2 → artifact rolls back to v1, write_v2 now READY
//   - Invalidate write_v1 → walker must NOT restore v2 (write_v2 is
//     currently READY, so its commit is a ghost revision)
//   - Expected: artifact is deleted (no valid prior writer)
func TestInvalidateWalkerSkipsPreviouslyInvalidatedWriter(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.createTestProject()
	s.lastProjectID = pid

	// Run 1: write v1
	yaml1 := `name: "v1"
version: 1
tasks:
  - id: write_v1
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write v1."
`
	s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml1})
	s.lastRunSeq = 1
	s.claim("write_v1", alice)
	s.submitWithArtifacts("write_v1", "first", map[string]string{"notes/intro.md": "version ONE"})

	// Run 2: write v2
	yaml2 := `name: "v2"
version: 1
tasks:
  - id: write_v2
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write v2."
`
	s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml2})
	s.lastRunSeq = 2
	s.claim("write_v2", alice)
	s.submitWithArtifacts("write_v2", "second", map[string]string{"notes/intro.md": "version TWO"})

	// Confirm v2 is current.
	if c, _ := s.readArtifactFile(pid, "notes/intro.md"); c != "version TWO" {
		t.Fatalf("expected v2 on disk, got %q", c)
	}

	// Invalidate write_v2. Should roll back to v1. write_v2 is now READY.
	s.lastRunSeq = 2
	resp := s.invalidate("write_v2", "wrong")
	if resp["status"] != "invalidated" {
		t.Fatalf("first invalidate failed: %v", resp)
	}
	if c, _ := s.readArtifactFile(pid, "notes/intro.md"); c != "version ONE" {
		t.Fatalf("expected v1 after first rollback, got %q", c)
	}

	// Sanity: write_v2 should now be READY, write_v1 still ACCEPTED.
	s.lastRunSeq = 2
	if state := s.taskGet("write_v2")["state"]; state != "ready" {
		t.Fatalf("expected write_v2 READY after invalidation, got %v", state)
	}
	s.lastRunSeq = 1
	if state := s.taskGet("write_v1")["state"]; state != "accepted" {
		t.Fatalf("expected write_v1 still ACCEPTED, got %v", state)
	}

	// NOW invalidate write_v1. The walker should NOT resurrect v2
	// (whose author task is still READY from the earlier invalidation).
	// The only other candidate is write_v1 itself, which is being
	// invalidated right now. So the walker should find no valid prior
	// writer and DELETE the artifact.
	resp = s.invalidate("write_v1", "original was wrong too")
	if resp["status"] != "invalidated" {
		t.Fatalf("second invalidate failed: %v", resp)
	}

	rolled, _ := resp["artifacts_rolled_back"].([]interface{})
	if len(rolled) != 1 {
		t.Fatalf("expected 1 artifact rolled back, got %v", resp["artifacts_rolled_back"])
	}
	rbEntry, _ := rolled[0].(map[string]interface{})
	if rbEntry["deleted"] != true {
		t.Fatalf("expected artifact DELETED (no valid prior writer), got %v", rbEntry)
	}

	// File should be gone.
	if _, ok := s.readArtifactFile(pid, "notes/intro.md"); ok {
		t.Fatal("expected artifact deleted from disk after second invalidation")
	}

	// Artifact index row should be gone.
	list := s.getList(fmt.Sprintf("/api/v1/projects/%d/artifacts", pid))
	if len(list) != 0 {
		t.Fatalf("expected empty index after deletion, got %d entries: %v", len(list), list)
	}
}

// TestInvalidateCascadesAcrossRunsViaArtifactReads is the regression
// test for iteration 3.2's cross-run cascade bug: invalidating a task
// that wrote an artifact should also cascade tasks in other runs that
// declared reads_artifacts: [that path] and have accepted results.
//
// Scenario:
//   - Run 1: write_v1 writes notes/intro.md = v1 (accepted)
//   - Run 2: summarize reads notes/intro.md, completes (accepted)
//   - Invalidate write_v1
//   - Expected: summarize cascades to PENDING, response lists it under
//     artifact_readers
func TestInvalidateCascadesAcrossRunsViaArtifactReads(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	pid := s.createTestProject()
	s.lastProjectID = pid

	// Run 1: create the artifact.
	yaml1 := `name: "writer"
version: 1
tasks:
  - id: write_v1
    action: answer
    writes_artifacts: [notes/intro.md]
    prompt: "Write it."
`
	s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml1})
	s.lastRunSeq = 1
	s.claim("write_v1", alice)
	s.submitWithArtifacts("write_v1", "made v1", map[string]string{"notes/intro.md": "version ONE"})

	// Run 2: reader task that consumes the artifact.
	yaml2 := `name: "reader"
version: 1
tasks:
  - id: summarize
    action: answer
    reads_artifacts: [notes/intro.md]
    prompt: "Summarize {{artifact:notes/intro.md}}"
`
	s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml2})
	s.lastRunSeq = 2
	s.claim("summarize", alice)
	s.submit("summarize", "summary of version ONE")

	// Sanity: summarize is accepted.
	if state := s.taskGet("summarize")["state"]; state != "accepted" {
		t.Fatalf("expected summarize accepted, got %v", state)
	}

	// Invalidate write_v1. summarize reads the artifact, so it should
	// cascade to PENDING even though it lives in a different run.
	s.lastRunSeq = 1
	resp := s.invalidate("write_v1", "testing cross-run cascade")
	if resp["status"] != "invalidated" {
		t.Fatalf("invalidate failed: %v", resp)
	}

	// The response should list summarize under artifact_readers.
	readers, _ := resp["artifact_readers"].([]interface{})
	if len(readers) != 1 {
		t.Fatalf("expected 1 artifact_reader, got %v", resp["artifact_readers"])
	}
	if !strings.HasSuffix(readers[0].(string), ":summarize") {
		t.Fatalf("expected summarize in artifact_readers, got %v", readers[0])
	}

	// summarize should no longer be ACCEPTED. Currently it lands in
	// READY rather than PENDING because cross-run readers have no
	// task-level depends_on — UpdateReadyTasks auto-promotes any
	// PENDING task with empty task deps. The correctness contract is
	// still satisfied:
	//   - not in accepted state
	//   - result_path cleared
	//   - run flipped from completed to active
	//   - visible in list_ready_tasks as new work
	//
	// Teaching the scheduler about artifact dependencies so
	// cross-run readers stay in a blocked-until-re-consumed state is
	// a future refinement tracked in the buildout plan.
	s.lastRunSeq = 2
	state := s.taskGet("summarize")["state"]
	if state == "accepted" {
		t.Fatalf("expected summarize to NOT be accepted after cross-run cascade, got accepted")
	}
	if state != "ready" {
		t.Fatalf("expected summarize READY (auto-promoted from PENDING) after cross-run cascade, got %v", state)
	}

	// Result path should have been cleared.
	task := s.taskGet("summarize")
	if task["result_path"] != nil && task["result_path"] != "" {
		t.Fatalf("expected result_path cleared, got %v", task["result_path"])
	}

	// Run 2 should have flipped from completed back to active.
	run2Status := s.get("/api/v1/projects/" + fmt.Sprintf("%d", pid) + "/runs/2")
	if run2Status["state"] != "active" {
		t.Fatalf("expected run 2 active after cross-run cascade, got %v", run2Status["state"])
	}
}

// TestTaskInputsSurfacesMissingArtifacts verifies that when a task
// declares reads_artifacts for a path that doesn't exist on disk, the
// inputs response surfaces it via a missing_artifacts array rather
// than silently omitting it. This gives MCP formatters a signal to
// render a warning block so claimers can see the contract was broken.
func TestTaskInputsSurfacesMissingArtifacts(t *testing.T) {
	s := newTestServer(t)
	s.register("alice")

	pid := s.createTestProject()
	s.lastProjectID = pid

	yaml := `name: "Missing read"
version: 1
tasks:
  - id: reader
    action: answer
    reads_artifacts: [doesnt/exist.md]
    prompt: "Use {{artifact:doesnt/exist.md}} to summarize."
`
	s.post(fmt.Sprintf("/api/v1/projects/%d/runs", pid), map[string]string{"yaml": yaml})
	s.lastRunSeq = 1

	inputs := s.taskInputs("reader")

	// Artifacts map should be empty (nothing was found).
	if arts, _ := inputs["artifacts"].(map[string]interface{}); len(arts) != 0 {
		t.Fatalf("expected empty artifacts map, got %v", arts)
	}

	// missing_artifacts should list the declared path.
	missing, _ := inputs["missing_artifacts"].([]interface{})
	if len(missing) != 1 {
		t.Fatalf("expected 1 missing artifact, got %v", inputs["missing_artifacts"])
	}
	if missing[0] != "doesnt/exist.md" {
		t.Fatalf("expected doesnt/exist.md in missing, got %v", missing[0])
	}

	// The resolved prompt should STILL contain the literal
	// {{artifact:...}} reference as a secondary signal — it never
	// got substituted because the artifact didn't exist.
	resolved, _ := inputs["resolved_prompt"].(string)
	if !strings.Contains(resolved, "{{artifact:doesnt/exist.md}}") {
		t.Fatalf("expected unresolved reference in prompt, got: %s", resolved)
	}
}

// TestInvalidateRejectsNonAcceptedTarget verifies the API surface
// rejects invalidation of tasks that haven't been accepted.
func TestInvalidateRejectsNonAcceptedTarget(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")

	s.submitYAML("testdata/branching.yaml")

	// python_pros is in state READY (not ACCEPTED yet). Invalidating
	// should fail with a clear error.
	resp := s.invalidate("python_pros", "testing rejection")
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("expected error invalidating non-accepted task, got: %v", resp)
	}

	// Task state should be unchanged.
	pp := s.taskGet("python_pros")
	if pp["state"] != "ready" {
		t.Fatalf("expected python_pros still READY, got %v", pp["state"])
	}

	// Completing it and then invalidating should work.
	s.claim("python_pros", alice)
	s.submit("python_pros", "now accepted")
	resp = s.invalidate("python_pros", "try again")
	if resp["status"] != "invalidated" {
		t.Fatalf("expected invalidated after task was accepted, got %v", resp)
	}
}

// --- Iteration 1: assign_to + require_role (historical section) ---

// TestAccessControlAssignToListRejectsWithBothNames confirms that when
// a task has a multi-citizen assign_to list, a stranger's rejection
// error lists every allowed citizen. This exercises the list-form
// error path the user couldn't reach from a single MCP session.
func TestAccessControlAssignToListRejectsWithBothNames(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")
	charlie := s.register("charlie")

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	// both_task has assign_to: [alice, bob]. charlie is not in the list.
	resp := s.claim("both_task", charlie)
	errMsg, hasErr := resp["error"].(string)
	if !hasErr {
		t.Fatalf("charlie should be rejected from both_task, got: %v", resp)
	}
	if !strings.Contains(errMsg, alice) {
		t.Fatalf("expected error to mention alice, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, bob) {
		t.Fatalf("expected error to mention bob, got: %s", errMsg)
	}
}

// TestAccessControlRequireRoleAllowsMatchingCitizen confirms a claim
// from a citizen with the matching role succeeds.
func TestAccessControlRequireRoleAllowsMatchingCitizen(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	// Promote Alice to reviewer directly via the store — there's no
	// admin API for role changes yet.
	if err := s.store.SetCitizenRole(s.citizenID(alice), "reviewer"); err != nil {
		t.Fatal(err)
	}

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	resp := s.claim("role_task", alice)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("alice (reviewer) should be able to claim role_task, got: %v", resp)
	}
}

// TestAccessControlRequireRoleRejectsPlainCitizen confirms a claim from
// a citizen WITHOUT the matching role is rejected.
func TestAccessControlRequireRoleRejectsPlainCitizen(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	// Bob is a plain citizen — his role is "citizen" by default.
	resp := s.claim("role_task", bob)
	errMsg, hasErr := resp["error"].(string)
	if !hasErr {
		t.Fatalf("plain citizen should NOT be able to claim role_task, got: %v", resp)
	}
	if !strings.Contains(errMsg, "role") {
		t.Fatalf("expected error mentioning role, got: %s", errMsg)
	}
}

// TestAccessControlBothRestrictionsMustMatch confirms a task with both
// assign_to and require_role requires BOTH to pass. The rejection from
// assign_to happens first (by implementation order), but either one
// failing should block the claim.
func TestAccessControlBothRestrictionsMustMatch(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")
	charlie := s.register("charlie")

	// Alice is a reviewer, Bob is a plain citizen.
	if err := s.store.SetCitizenRole(s.citizenID(alice), "reviewer"); err != nil {
		t.Fatal(err)
	}

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	// Charlie fails on assign_to (not in the list).
	resp := s.claim("both_task", charlie)
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("charlie should fail assign_to check, got: %v", resp)
	}

	// Bob is in assign_to but lacks the reviewer role.
	resp = s.claim("both_task", bob)
	errMsg, hasErr := resp["error"].(string)
	if !hasErr {
		t.Fatalf("bob should fail require_role check, got: %v", resp)
	}
	if !strings.Contains(errMsg, "role") {
		t.Fatalf("expected error about role, got: %s", errMsg)
	}

	// Alice is in assign_to AND has the reviewer role — passes.
	resp = s.claim("both_task", alice)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("alice should pass both checks, got: %v", resp)
	}
}

// TestAccessControlScalarAssignTo confirms the parser accepts
// `assign_to: X` scalar form as well as `assign_to: [X]` list form.
// The access-control.yaml fixture uses the scalar form for
// assigned_task, so running that test exercises the scalar path. This
// test verifies the exact shape comes through in the task response.
func TestAccessControlScalarAssignTo(t *testing.T) {
	s := newTestServer(t)
	alice := s.register("alice")
	bob := s.register("bob")

	s.submitAccessControlYAML("testdata/access-control.yaml", alice, bob)

	task := s.taskGet("assigned_task")
	assignees, _ := task["assign_to"].([]interface{})
	if len(assignees) != 1 || assignees[0] != alice {
		t.Fatalf("expected assign_to = [%s], got %v", alice, task["assign_to"])
	}
}

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

// Suppress unused import
var _ = enjuYaml.Parse
