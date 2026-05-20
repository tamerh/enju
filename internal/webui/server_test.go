package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/inbox"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// fakeFC is a minimal fatClient implementation for tests. Each
// method returns the canned value the test set up; nothing
// touches the network or git. Real *service.FatClient also
// satisfies the interface, so production wiring stays
// type-checked end-to-end.
type fakeFC struct {
	username    string
	projects    []wire.Project
	matz        []service.MaterializedProject
	archivedProjects  []wire.Project
	archivedErr       error
	setArchivedID     int64
	setArchivedFlag   bool
	setArchivedResult *service.ProjectArchiveResult
	setArchivedErr    error
	projDetail  *service.ProjectDetail
	runs        []wire.Run
	runDetail   *service.RunDetail
	exportMD    string
	exportErr   error
	taskMeta    *service.TaskMeta
	taskResult  string
	iterations  []wire.Iteration
	iterBodies  map[string]string // commitSHA → body
	inbox       *service.InboxResult
	inboxErr    error
	events      []service.EventRow
	err         error
	getProjErr  error
	getRunErr   error
	getTaskErr  error

	// Write-side captures (test asserts what handler called)
	claimResult     *service.ClaimResult
	claimErr        error
	claimedID       string
	releaseErr      error
	releasedID      string
	failedID        string
	failedReason    string
	failErr         error
	submitResult    *service.SubmitResult
	submitParams    service.SubmitParams
	submitCalled    bool

	// Run-level write captures
	pausedPID     int64
	pausedSeq     int
	pauseErr      error
	resumedPID    int64
	resumedSeq    int
	resumeErr     error
	terminatedPID    int64
	terminatedSeq    int
	terminatedReason string
	terminateErr     error

	// Issues captures
	issues             []service.IssueResponse
	issue              *service.IssueResponse
	listedIssuesPID    int64
	listedIssuesOpts   service.IssueListOpts
	listIssuesErr      error
	getIssueErr        error
	filedIssuePID      int64
	filedIssueParams   service.FileIssueParams
	fileIssueResp      *service.FileIssueResponse
	fileIssueErr       error
	triagedIssueSeq    int
	triagedSeverity    string
	triageIssueErr     error
	closedIssueSeq     int
	closedStatus       string
	closedByTaskID     string
	closeIssueErr      error

	// Workflows captures
	workflows           []service.WorkflowSummary
	listWorkflowsErr    error
	loadedWorkflow      *service.LoadedWorkflow
	describeWorkflowErr error
	createdFromPath     string
	createdParams       map[string]interface{}
	createdBranch       string
	createdYAML         string
	createdYAMLBranch   string
	createRunResult     *service.CreateRunFromTemplateResult
	createRunErr        error

	// Create-project captures
	createdProjectParams service.CreateProjectParams
	createProjectResult  *service.CreateProjectResult
	createProjectErr     error
	syncResp             map[string]interface{}
	syncErr              error
	syncedForce          bool
	addedMemberUser      string
	addedMemberRole      string
	addMemberErr         error
	removedMemberUser    string
	removeMemberErr      error
	roleUser             string
	roleValue            string
	roleChanged          bool
	roleErr              error
	setBranch            string
	setBranchWarn        string
	setBranchErr         error
	setRemote            string
	setRemoteWarn        string
	setRemoteErr         error
	remoteStatus         map[string]interface{}
	remoteStatusErr      error
	leftProject          int64
	leftKeepMembership   bool
	leaveSummary         string
	leaveErr             error

	// Execute captures
	executedTaskID  string
	executeTaskErr  error
	executedRunPID  int64
	executedRunSeq  int
	executedRunMax  int
	executedRunPar  int
	executeRunErr   error

	// Artifacts captures
	artifacts             []service.ArtifactResponse
	listArtifactsErr      error
	listedArtifactsBranch string
	listedArtifactsPrefix string
	artifactContent       []byte
	getArtifactErr        error
	gotArtifactPath       string
	gotArtifactBranch     string
	artifactHistory       []byte
	getArtifactHistoryErr error
	untracked             *service.UntrackedArtifactReport
	untrackedErr          error

	// Me captures
	dashboard           *service.DashboardResponse
	getDashboardErr     error
	contributions       *service.ContributionsResponse
	getContributionsErr error
	updateProfileParams service.UpdateProfileParams
	savedProfile        *service.CitizenResponse
	updateProfileErr    error
	agents              []service.AgentSummary
	listAgentsErr       error
	registeredAgent     service.RegisterAgentParams
	registerAgentResult *service.RegisterAgentResult
	registerAgentErr    error
}

func (f *fakeFC) Username() string { return f.username }
func (f *fakeFC) ListProjects(ctx context.Context) ([]wire.Project, error) {
	return f.projects, f.err
}
func (f *fakeFC) ListMaterializedProjects() ([]service.MaterializedProject, error) {
	return f.matz, nil
}
func (f *fakeFC) ListArchivedProjects(ctx context.Context) ([]wire.Project, error) {
	return f.archivedProjects, f.archivedErr
}
func (f *fakeFC) SetProjectArchived(ctx context.Context, id int64, archive bool) (*service.ProjectArchiveResult, error) {
	f.setArchivedID = id
	f.setArchivedFlag = archive
	if f.setArchivedErr != nil {
		return nil, f.setArchivedErr
	}
	if f.setArchivedResult != nil {
		return f.setArchivedResult, nil
	}
	st := "restored"
	if archive {
		st = "archived"
	}
	return &service.ProjectArchiveResult{Name: "proj", Status: st}, nil
}
func (f *fakeFC) GetProject(ctx context.Context, id int64) (*service.ProjectDetail, error) {
	return f.projDetail, f.getProjErr
}
func (f *fakeFC) ListRuns(ctx context.Context, id int64) ([]wire.Run, error) {
	return f.runs, nil
}
func (f *fakeFC) GetRun(ctx context.Context, id int64, seq int) (*service.RunDetail, error) {
	return f.runDetail, f.getRunErr
}
func (f *fakeFC) ExportRunMarkdown(ctx context.Context, id int64, seq int) (string, error) {
	return f.exportMD, f.exportErr
}
func (f *fakeFC) ListEvents(ctx context.Context, id int64, opts service.ListEventsOpts) ([]service.EventRow, error) {
	return f.events, nil
}
func (f *fakeFC) BuildInbox(ctx context.Context, id int64, u string) (*service.InboxResult, error) {
	return f.inbox, f.inboxErr
}
func (f *fakeFC) FetchTaskMeta(ctx context.Context, id string) (*service.TaskMeta, error) {
	return f.taskMeta, f.getTaskErr
}
func (f *fakeFC) ReadTaskResult(ctx context.Context, id string) (string, bool, error) {
	if f.taskResult == "" {
		return "", false, nil
	}
	return f.taskResult, true, nil
}
func (f *fakeFC) ListTaskIterations(ctx context.Context, id string) ([]wire.Iteration, error) {
	return f.iterations, nil
}
func (f *fakeFC) ReadResultAtCommit(ctx context.Context, pid int64, sha, dir string) (string, bool, error) {
	if body, ok := f.iterBodies[sha]; ok {
		return body, true, nil
	}
	return "", false, nil
}
func (f *fakeFC) ClaimTask(ctx context.Context, params service.ClaimParams) (*service.ClaimResult, error) {
	f.claimedID = params.TaskID
	if f.claimResult == nil && f.claimErr == nil {
		return &service.ClaimResult{Data: []byte(`{"ok":true}`)}, nil
	}
	return f.claimResult, f.claimErr
}
func (f *fakeFC) ReleaseTask(ctx context.Context, id string) error {
	f.releasedID = id
	return f.releaseErr
}
func (f *fakeFC) FailTask(ctx context.Context, id, reason string) error {
	f.failedID = id
	f.failedReason = reason
	return f.failErr
}
func (f *fakeFC) SubmitTaskResult(ctx context.Context, params service.SubmitParams) *service.SubmitResult {
	f.submitCalled = true
	f.submitParams = params
	if f.submitResult != nil {
		return f.submitResult
	}
	return &service.SubmitResult{ResponseBody: []byte(`{"state":"accepted"}`)}
}
func (f *fakeFC) PauseRun(ctx context.Context, pid int64, seq int) error {
	f.pausedPID, f.pausedSeq = pid, seq
	return f.pauseErr
}
func (f *fakeFC) ResumeRun(ctx context.Context, pid int64, seq int) error {
	f.resumedPID, f.resumedSeq = pid, seq
	return f.resumeErr
}
func (f *fakeFC) TerminateRun(ctx context.Context, pid int64, seq int, reason string) error {
	f.terminatedPID, f.terminatedSeq, f.terminatedReason = pid, seq, reason
	return f.terminateErr
}
func (f *fakeFC) ListIssues(ctx context.Context, pid int64, opts service.IssueListOpts) ([]service.IssueResponse, error) {
	f.listedIssuesPID = pid
	f.listedIssuesOpts = opts
	return f.issues, f.listIssuesErr
}
func (f *fakeFC) GetIssue(ctx context.Context, pid int64, seq int) (*service.IssueResponse, error) {
	return f.issue, f.getIssueErr
}
func (f *fakeFC) FileIssue(ctx context.Context, pid int64, params service.FileIssueParams) (*service.FileIssueResponse, error) {
	f.filedIssuePID = pid
	f.filedIssueParams = params
	if f.fileIssueErr != nil {
		return nil, f.fileIssueErr
	}
	if f.fileIssueResp != nil {
		return f.fileIssueResp, nil
	}
	return &service.FileIssueResponse{ID: 99, Seq: 1, Slug: "ISSUE-001", Title: params.Title, Status: "open", Severity: "medium"}, nil
}
func (f *fakeFC) TriageIssue(ctx context.Context, pid int64, seq int, severity string) (*service.IssueResponse, error) {
	f.triagedIssueSeq = seq
	f.triagedSeverity = severity
	return f.issue, f.triageIssueErr
}
func (f *fakeFC) CloseIssue(ctx context.Context, pid int64, seq int, status, closedByTaskID string) (*service.IssueResponse, error) {
	f.closedIssueSeq = seq
	f.closedStatus = status
	f.closedByTaskID = closedByTaskID
	return f.issue, f.closeIssueErr
}
func (f *fakeFC) ListWorkflows(ctx context.Context, pid int64) ([]service.WorkflowSummary, error) {
	return f.workflows, f.listWorkflowsErr
}
func (f *fakeFC) DescribeWorkflow(ctx context.Context, pid int64, path string) (*service.LoadedWorkflow, error) {
	return f.loadedWorkflow, f.describeWorkflowErr
}
func (f *fakeFC) CreateRunFromTemplate(ctx context.Context, pid int64, path string, params map[string]interface{}, branch, name, email string) (*service.CreateRunFromTemplateResult, error) {
	f.createdFromPath = path
	f.createdParams = params
	f.createdBranch = branch
	if f.createRunErr != nil {
		return nil, f.createRunErr
	}
	if f.createRunResult != nil {
		return f.createRunResult, nil
	}
	return &service.CreateRunFromTemplateResult{CoordResponse: []byte(`{"seq":3,"name":"new run"}`)}, nil
}
func (f *fakeFC) CreateRunFromYAML(ctx context.Context, pid int64, yamlContent string, params map[string]interface{}, branch, name, email string) (*service.CreateRunFromTemplateResult, error) {
	f.createdYAML = yamlContent
	f.createdYAMLBranch = branch
	if f.createRunErr != nil {
		return nil, f.createRunErr
	}
	if f.createRunResult != nil {
		return f.createRunResult, nil
	}
	return &service.CreateRunFromTemplateResult{CoordResponse: []byte(`{"seq":7,"name":"yaml run"}`)}, nil
}
func (f *fakeFC) CommitAuthor(ctx context.Context) (string, string) {
	return "tamer", "tamer@example.com"
}
func (f *fakeFC) ExecuteComputeTask(ctx context.Context, id string) (*service.ExecuteOutcome, error) {
	f.executedTaskID = id
	if f.executeTaskErr != nil {
		return nil, f.executeTaskErr
	}
	return &service.ExecuteOutcome{TaskID: id, Status: "completed"}, nil
}
func (f *fakeFC) ExecuteRun(ctx context.Context, p service.ExecuteRunParams) (*service.ExecuteRunResult, error) {
	f.executedRunPID, f.executedRunSeq = int64(p.ProjectID), p.RunSeq
	f.executedRunMax, f.executedRunPar = p.MaxTasks, p.Parallel
	if f.executeRunErr != nil {
		return nil, f.executeRunErr
	}
	return &service.ExecuteRunResult{StopReason: "idle"}, nil
}
func (f *fakeFC) ListArtifacts(ctx context.Context, pid int64, opts service.ListArtifactsOpts) ([]service.ArtifactResponse, error) {
	f.listedArtifactsBranch = opts.Branch
	f.listedArtifactsPrefix = opts.Prefix
	return f.artifacts, f.listArtifactsErr
}
func (f *fakeFC) GetArtifactContent(ctx context.Context, pid int64, path, branch string) ([]byte, error) {
	f.gotArtifactPath = path
	f.gotArtifactBranch = branch
	return f.artifactContent, f.getArtifactErr
}
func (f *fakeFC) GetArtifactHistory(ctx context.Context, pid int64, path, branch string) ([]byte, error) {
	return f.artifactHistory, f.getArtifactHistoryErr
}
func (f *fakeFC) ListUntrackedArtifacts(ctx context.Context, pid int64, branch string) (*service.UntrackedArtifactReport, error) {
	return f.untracked, f.untrackedErr
}
func (f *fakeFC) GetDashboard(ctx context.Context) (*service.DashboardResponse, error) {
	return f.dashboard, f.getDashboardErr
}
func (f *fakeFC) GetContributions(ctx context.Context, username string) (*service.ContributionsResponse, error) {
	return f.contributions, f.getContributionsErr
}
func (f *fakeFC) UpdateProfile(ctx context.Context, params service.UpdateProfileParams) (*service.CitizenResponse, error) {
	f.updateProfileParams = params
	if f.updateProfileErr != nil {
		return nil, f.updateProfileErr
	}
	if f.savedProfile != nil {
		return f.savedProfile, nil
	}
	return &service.CitizenResponse{Username: f.username, Name: "saved"}, nil
}
func (f *fakeFC) ListMyAgents(ctx context.Context) ([]service.AgentSummary, error) {
	return f.agents, f.listAgentsErr
}
func (f *fakeFC) RegisterAgent(ctx context.Context, p service.RegisterAgentParams) (*service.RegisterAgentResult, error) {
	f.registeredAgent = p
	if f.registerAgentErr != nil {
		return nil, f.registerAgentErr
	}
	if f.registerAgentResult != nil {
		return f.registerAgentResult, nil
	}
	return &service.RegisterAgentResult{
		Username: "auto-slug", Name: p.Name, ParentName: f.username,
		Token: "tok_secret_once", Warning: "stash it",
	}, nil
}
func (f *fakeFC) CreateProject(ctx context.Context, params service.CreateProjectParams) (*service.CreateProjectResult, error) {
	f.createdProjectParams = params
	if f.createProjectErr != nil {
		return nil, f.createProjectErr
	}
	if f.createProjectResult != nil {
		return f.createProjectResult, nil
	}
	return &service.CreateProjectResult{ProjectID: 42, CoordResponse: []byte(`{"id":42,"name":"x"}`)}, nil
}
func (f *fakeFC) SyncProjectToRemote(ctx context.Context, id int64, force bool) (map[string]interface{}, error) {
	f.syncedForce = force
	return f.syncResp, f.syncErr
}
func (f *fakeFC) AddProjectMember(ctx context.Context, id int64, username, role string) error {
	f.addedMemberUser, f.addedMemberRole = username, role
	return f.addMemberErr
}
func (f *fakeFC) RemoveProjectMember(ctx context.Context, id int64, username string) error {
	f.removedMemberUser = username
	return f.removeMemberErr
}
func (f *fakeFC) SetProjectMemberRole(ctx context.Context, id int64, username, role string) (bool, error) {
	f.roleUser, f.roleValue = username, role
	return f.roleChanged, f.roleErr
}
func (f *fakeFC) SetProjectDefaultBranch(ctx context.Context, id int64, branch string) (string, error) {
	f.setBranch = branch
	return f.setBranchWarn, f.setBranchErr
}
func (f *fakeFC) SetProjectRemote(ctx context.Context, id int64, remoteURL string) (string, error) {
	f.setRemote = remoteURL
	return f.setRemoteWarn, f.setRemoteErr
}
func (f *fakeFC) RemoteStatusReport(ctx context.Context, id int64) (map[string]interface{}, error) {
	return f.remoteStatus, f.remoteStatusErr
}
func (f *fakeFC) LeaveProject(ctx context.Context, id int64, keepMembership bool) (string, error) {
	f.leftProject = id
	f.leftKeepMembership = keepMembership
	return f.leaveSummary, f.leaveErr
}

func newTestServer(t *testing.T, fc *fakeFC) *Server {
	t.Helper()
	s, err := New(Config{FC: fc, Port: 8080})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestHealth: server constructs, /health returns 200 "ok". No
// FatClient calls, no template render — just proves the package
// boots.
func TestHealth(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if string(body) != "ok" {
		t.Fatalf("body: got %q, want %q", string(body), "ok")
	}
}

// TestLandingFullPage: GET / without HX-Request returns the
// full layout (contains <html>) plus the project list.
func TestLandingFullPage(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projects: []wire.Project{
			{ID: 1, Name: "alpha", DefaultBranch: "main", RunCount: 3},
			{ID: 2, Name: "beta", DefaultBranch: "main", RunCount: 0},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"<html", "Projects", "alpha", "beta", "@tamer"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// Static assets must be cache-busted (?v=<hash>) so the
	// immutable-1y browser cache refetches them after a rebuild.
	// Un-versioned bare URLs would mean stale app.js/app.css.
	for _, bare := range []string{`href="/static/app.css"`, `src="/static/app.js"`} {
		if strings.Contains(body, bare) {
			t.Errorf("asset URL %s is not cache-busted (missing ?v=)", bare)
		}
	}
	if !strings.Contains(body, "/static/app.css?v=") ||
		!strings.Contains(body, "/static/app.js?v=") {
		t.Errorf("expected ?v= cache-bust on app.css/app.js; body: %q", body)
	}
}

// TestLandingPartial: GET / with HX-Request: true returns only
// the page's main block — no <html>, no <head>, just the inner
// content.
func TestLandingPartial(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projects: []wire.Project{{ID: 1, Name: "alpha"}},
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<html") {
		t.Errorf("partial render leaked <html>; body:\n%s", body)
	}
	if !strings.Contains(body, "alpha") {
		t.Errorf("partial render missing project name; body:\n%s", body)
	}
}

// TestLandingEmpty: zero projects → renders with the empty-
// state copy.
func TestLandingEmpty(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No projects yet") {
		t.Errorf("empty-state copy missing; body:\n%s", body)
	}
}

// TestProjectView: GET /p/{pid} renders project header + runs.
func TestProjectView(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projDetail: &service.ProjectDetail{
			Project: wire.Project{
				ID:            1,
				Name:          "webui-toy",
				Description:   "A toy project",
				DefaultBranch: "main",
				RunCount:      1,
			},
			Members: []wire.Member{{Username: "tamer", Role: "owner"}},
		},
		runs: []wire.Run{
			{Seq: 1, Name: "Hello run", State: "active", Branch: "main", TaskCount: 3},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"webui-toy", "A toy project", "Hello run", "@tamer", "active", "/p/1/r/1"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestProjectViewBadID: non-numeric id returns 400.
func TestProjectViewBadID(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/p/notanint", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
}

// projDetailWithRemote is a minimal project detail whose remote
// is configured, so the sync UI renders.
func projDetailWithRemote() *service.ProjectDetail {
	return &service.ProjectDetail{
		Project: wire.Project{
			ID: 1, Name: "webui-toy", DefaultBranch: "main",
			RemoteURL: "git@example.com:org/webui-toy.git",
		},
		Members: []wire.Member{{Username: "tamer", Role: "owner"}},
	}
}

// TestProjectSyncShowsButtonOnlyWithRemote: the sync controls
// appear when a remote is configured and collapse to the
// no-origin note when it isn't.
func TestProjectSyncShowsButtonOnlyWithRemote(t *testing.T) {
	withRemote := newTestServer(t, &fakeFC{username: "tamer", projDetail: projDetailWithRemote()})
	rr := httptest.NewRecorder()
	withRemote.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	if !strings.Contains(rr.Body.String(), "/p/1/sync") {
		t.Errorf("expected sync form when remote configured")
	}

	noRemote := newTestServer(t, &fakeFC{
		username: "tamer",
		projDetail: &service.ProjectDetail{
			Project: wire.Project{ID: 1, Name: "local-only", DefaultBranch: "main"},
		},
	})
	rr2 := httptest.NewRecorder()
	noRemote.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	body := rr2.Body.String()
	if strings.Contains(body, `action="/p/1/sync"`) {
		t.Errorf("sync form should be hidden with no remote")
	}
	if !strings.Contains(body, "No remote configured") {
		t.Errorf("expected the no-origin note")
	}
}

// TestProjectSyncPushed: a successful sync re-renders the
// project page with the format.ProjectSyncResult one-liner as a
// success banner; force defaults to false.
func TestProjectSyncPushed(t *testing.T) {
	fc := &fakeFC{
		username:   "tamer",
		projDetail: projDetailWithRemote(),
		syncResp: map[string]interface{}{
			"project_id": float64(1),
			"remote_url": "git@example.com:org/webui-toy.git",
			"result":     "pushed",
		},
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/sync", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.syncedForce {
		t.Errorf("force should default to false")
	}
	if !strings.Contains(rr.Body.String(), "pushed to git@example.com:org/webui-toy.git") {
		t.Errorf("expected pushed banner; body: %q", rr.Body.String())
	}
}

// TestProjectSyncForce: the force-push form sets force=true.
func TestProjectSyncForce(t *testing.T) {
	fc := &fakeFC{
		username:   "tamer",
		projDetail: projDetailWithRemote(),
		syncResp: map[string]interface{}{
			"project_id": float64(1), "remote_url": "r", "result": "force_pushed",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("force=true")
	req := httptest.NewRequest(http.MethodPost, "/p/1/sync", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if !fc.syncedForce {
		t.Errorf("force should be true when force=true posted")
	}
	if !strings.Contains(rr.Body.String(), "force-pushed") {
		t.Errorf("expected force-pushed banner; body: %q", rr.Body.String())
	}
}

// TestProjectSyncNoRemoteError: a hard error (no remote) becomes
// a friendly banner on a 200 page, not a 5xx.
func TestProjectSyncNoRemoteError(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username:   "tamer",
		projDetail: &service.ProjectDetail{Project: wire.Project{ID: 1, Name: "local-only"}},
		syncErr:    fmt.Errorf("project has no remote URL configured on the coordinator"),
	})
	req := httptest.NewRequest(http.MethodPost, "/p/1/sync", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no remote URL configured") {
		t.Errorf("expected the error surfaced as a banner; body: %q", rr.Body.String())
	}
}

// ownerProjDetail is a project whose only member is @tamer as
// owner — so renderProjectPage computes IsOwner=true and the
// roster controls render.
func ownerProjDetail() *service.ProjectDetail {
	return &service.ProjectDetail{
		Project: wire.Project{ID: 1, Name: "webui-toy", DefaultBranch: "main"},
		Members: []wire.Member{
			{Username: "tamer", Role: "owner"},
			{Username: "bob", Role: "member"},
		},
	}
}

// TestProjectMemberControlsOwnerGated: an owner sees add/
// remove/role forms; a non-owner viewer sees the roster only.
func TestProjectMemberControlsOwnerGated(t *testing.T) {
	owner := newTestServer(t, &fakeFC{username: "tamer", projDetail: ownerProjDetail()})
	rr := httptest.NewRecorder()
	owner.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`action="/p/1/members"`,
		`action="/p/1/members/bob/remove"`,
		`action="/p/1/members/bob/role"`,
		"Add member",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner view missing %q", want)
		}
	}

	// Same project, viewer is a plain member → no controls.
	viewer := newTestServer(t, &fakeFC{username: "bob", projDetail: ownerProjDetail()})
	rr2 := httptest.NewRecorder()
	viewer.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	if strings.Contains(rr2.Body.String(), `action="/p/1/members"`) {
		t.Errorf("non-owner should not see roster controls")
	}
	if !strings.Contains(rr2.Body.String(), "@bob") {
		t.Errorf("non-owner should still see the roster")
	}
}

// TestAddProjectMember: POST adds the member and banners the
// outcome; role defaults through when blank.
func TestAddProjectMember(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail()}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/members",
		strings.NewReader("username=carol&role="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.addedMemberUser != "carol" || fc.addedMemberRole != "" {
		t.Errorf("AddProjectMember got (%q,%q)", fc.addedMemberUser, fc.addedMemberRole)
	}
	if !strings.Contains(rr.Body.String(), "Added @carol to the project as member") {
		t.Errorf("expected add banner; body: %q", rr.Body.String())
	}
}

// TestAddProjectMemberMissingUsername: blank username is
// rejected with a banner; the service is not called.
func TestAddProjectMemberMissingUsername(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail()}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/members",
		strings.NewReader("username=+"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if fc.addedMemberUser != "" {
		t.Errorf("service should not be called; got %q", fc.addedMemberUser)
	}
	if !strings.Contains(rr.Body.String(), "username is required") {
		t.Errorf("expected validation banner; body: %q", rr.Body.String())
	}
}

// TestRemoveProjectMember: POST removes by username from the
// path and banners it.
func TestRemoveProjectMember(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail()}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/members/bob/remove", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.removedMemberUser != "bob" {
		t.Errorf("RemoveProjectMember got %q, want bob", fc.removedMemberUser)
	}
	if !strings.Contains(rr.Body.String(), "Removed @bob from the project") {
		t.Errorf("expected remove banner; body: %q", rr.Body.String())
	}
}

// TestSetProjectMemberRolePromote: role=owner with changed=true
// banners "promoted".
func TestSetProjectMemberRolePromote(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail(), roleChanged: true}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/members/bob/role",
		strings.NewReader("role=owner"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.roleUser != "bob" || fc.roleValue != "owner" {
		t.Errorf("SetProjectMemberRole got (%q,%q)", fc.roleUser, fc.roleValue)
	}
	if !strings.Contains(rr.Body.String(), "@bob promoted to owner") {
		t.Errorf("expected promote banner; body: %q", rr.Body.String())
	}
}

// TestSetProjectMemberRoleNoChange: changed=false banners the
// no-op rather than implying a mutation.
func TestSetProjectMemberRoleNoChange(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail(), roleChanged: false}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/members/bob/role",
		strings.NewReader("role=member"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "already member — no change") {
		t.Errorf("expected no-change banner; body: %q", rr.Body.String())
	}
}

// TestSetProjectMemberRoleBadRole: a role other than
// owner/member is rejected before hitting the service.
func TestSetProjectMemberRoleBadRole(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail()}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/members/bob/role",
		strings.NewReader("role=admin"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if fc.roleUser != "" {
		t.Errorf("service should not be called for bad role")
	}
	// The banner is HTML-escaped by the template, so the quotes
	// render as entities — assert the quote-free prefix.
	if !strings.Contains(rr.Body.String(), "role must be") {
		t.Errorf("expected bad-role banner; body: %q", rr.Body.String())
	}
}

// TestAddProjectMemberCoordError: a coord rejection (e.g.
// non-owner) surfaces as a banner on a 200, not a 5xx.
func TestAddProjectMemberCoordError(t *testing.T) {
	fc := &fakeFC{
		username:     "tamer",
		projDetail:   ownerProjDetail(),
		addMemberErr: fmt.Errorf("only owners can add members"),
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/members",
		strings.NewReader("username=carol"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "only owners can add members") {
		t.Errorf("expected coord error bannered; body: %q", rr.Body.String())
	}
}

// TestSetProjectDefaultBranch: POST sets the branch and banners
// it; a non-fatal warning is appended.
func TestSetProjectDefaultBranch(t *testing.T) {
	fc := &fakeFC{
		username:      "tamer",
		projDetail:    ownerProjDetail(),
		setBranchWarn: "could not push new branch",
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/default-branch",
		strings.NewReader("branch=develop"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.setBranch != "develop" {
		t.Errorf("SetProjectDefaultBranch got %q, want develop", fc.setBranch)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Default branch set to") || !strings.Contains(body, "could not push new branch") {
		t.Errorf("expected branch banner + warning; body: %q", body)
	}
}

// TestSetProjectDefaultBranchMissing: blank branch is rejected
// before the service.
func TestSetProjectDefaultBranchMissing(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail()}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/default-branch",
		strings.NewReader("branch=+"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if fc.setBranch != "" {
		t.Errorf("service should not be called; got %q", fc.setBranch)
	}
	if !strings.Contains(rr.Body.String(), "branch is required") {
		t.Errorf("expected validation banner; body: %q", rr.Body.String())
	}
}

// TestSetProjectRemote: POST sets the remote and banners it.
func TestSetProjectRemote(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjDetail()}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/remote",
		strings.NewReader("remote_url=git@example.com:o/r.git"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.setRemote != "git@example.com:o/r.git" {
		t.Errorf("SetProjectRemote got %q", fc.setRemote)
	}
	if !strings.Contains(rr.Body.String(), "Remote set to git@example.com:o/r.git") {
		t.Errorf("expected remote banner; body: %q", rr.Body.String())
	}
}

// TestSetProjectRemoteError: service rejection (e.g. empty)
// surfaces as a banner on a 200, not a 5xx.
func TestSetProjectRemoteError(t *testing.T) {
	fc := &fakeFC{
		username:     "tamer",
		projDetail:   ownerProjDetail(),
		setRemoteErr: fmt.Errorf("remote_url cannot be empty"),
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/remote",
		strings.NewReader("remote_url=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "remote_url cannot be empty") {
		t.Errorf("expected error banner; body: %q", rr.Body.String())
	}
}

// TestProjectRemoteStatusLine: when a remote is configured the
// page renders the format.ProjectRemoteStatus one-liner; the
// settings forms are owner-gated.
func TestProjectRemoteStatusLine(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		projDetail: &service.ProjectDetail{
			Project: wire.Project{
				ID: 1, Name: "p", DefaultBranch: "main",
				RemoteURL: "git@example.com:o/r.git",
			},
			Members: []wire.Member{{Username: "tamer", Role: "owner"}},
		},
		remoteStatus: map[string]interface{}{
			"project_id": float64(1),
			"status":     "ahead",
			"ahead_by":   float64(2),
			"behind_by":  float64(0),
		},
	}
	s := newTestServer(t, fc)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "Remote status:") {
		t.Errorf("expected remote status line; body: %q", body)
	}
	if !strings.Contains(body, `action="/p/1/default-branch"`) ||
		!strings.Contains(body, `action="/p/1/remote"`) {
		t.Errorf("expected owner settings forms; body: %q", body)
	}
}

// TestProjectSettingsNonOwner: a non-owner sees the read-only
// settings summary, not the forms, and no remote-status fetch
// crash when none is configured.
func TestProjectSettingsNonOwner(t *testing.T) {
	fc := &fakeFC{
		username: "bob",
		projDetail: &service.ProjectDetail{
			Project: wire.Project{ID: 1, Name: "p", DefaultBranch: "main"},
			Members: []wire.Member{
				{Username: "tamer", Role: "owner"},
				{Username: "bob", Role: "member"},
			},
		},
	}
	s := newTestServer(t, fc)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	body := rr.Body.String()
	if strings.Contains(body, `action="/p/1/default-branch"`) {
		t.Errorf("non-owner should not see settings forms")
	}
	if !strings.Contains(body, "Changing these is owner-only") {
		t.Errorf("expected read-only settings summary; body: %q", body)
	}
}

// TestProjectShowsLeaveButton: the leave control is available
// to any member (not owner-gated).
func TestProjectShowsLeaveButton(t *testing.T) {
	fc := &fakeFC{
		username: "bob",
		projDetail: &service.ProjectDetail{
			Project: wire.Project{ID: 1, Name: "p", DefaultBranch: "main"},
			Members: []wire.Member{
				{Username: "tamer", Role: "owner"},
				{Username: "bob", Role: "member"},
			},
		},
	}
	s := newTestServer(t, fc)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	if !strings.Contains(rr.Body.String(), `action="/p/1/leave"`) {
		t.Errorf("expected leave form for a plain member")
	}
}

// TestProjectOverviewIsAdminFree: the overview is runs-first and
// carries no admin write forms — those moved to /settings. It
// links to settings and shows a read-only members strip.
func TestProjectOverviewIsAdminFree(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username:   "tamer",
		projDetail: ownerProjDetail(),
		runs:       []wire.Run{{Seq: 1, Name: "Hello", State: "active", Branch: "main"}},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1", nil))
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	// Runs are present; a settings link is present; the
	// read-only roster shows members.
	for _, want := range []string{"Hello", "/p/1/workflows", "/p/1/settings", "@tamer"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	// No admin write forms leak onto the overview.
	for _, gone := range []string{
		`action="/p/1/members"`, `action="/p/1/default-branch"`,
		`action="/p/1/remote"`, `action="/p/1/sync"`, `action="/p/1/leave"`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("overview must not contain admin form %q", gone)
		}
	}
}

// TestProjectSettingsSections: the settings page renders the
// four section headings for an owner.
func TestProjectSettingsSections(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer", projDetail: projDetailWithRemote()})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"<h2>Members</h2>", "<h2>General</h2>",
		"<h2>Maintenance</h2>", "<h2>Danger zone</h2>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing section %q", want)
		}
	}
}

// ownerProjArchived returns an owner-viewed project detail with
// the archived flag toggled.
func ownerProjArchived(archived bool) *service.ProjectDetail {
	return &service.ProjectDetail{
		Project: wire.Project{ID: 1, Name: "webui-toy", DefaultBranch: "main", Archived: archived},
		Members: []wire.Member{{Username: "tamer", Role: "owner"}},
	}
}

// TestLandingHidesArchivedWithFooterLink: the default landing
// shows active projects + a forward link to /archived; it never
// fetches archived itself.
func TestLandingHidesArchivedWithFooterLink(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projects: []wire.Project{{ID: 1, Name: "active-one"}},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "active-one") {
		t.Errorf("expected active project listed")
	}
	if !strings.Contains(body, `href="/archived"`) ||
		!strings.Contains(body, "View archived projects") {
		t.Errorf("expected forward link to /archived; body: %q", body)
	}
}

// TestArchivedProjectsView: /archived lists archived projects in
// the archived-mode landing (heading + back link, no create
// form, no forward link).
func TestArchivedProjectsView(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username:         "tamer",
		archivedProjects: []wire.Project{{ID: 9, Name: "old-thing", Archived: true}},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/archived", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Archived projects", "old-thing", `href="/p/9"`, "← Active projects"} {
		if !strings.Contains(body, want) {
			t.Errorf("archived view missing %q", want)
		}
	}
	if strings.Contains(body, `action="/projects"`) {
		t.Errorf("archived view must not show the create-project form")
	}
}

// TestLandingSortsByActivity: the project table orders rows by
// last activity descending, flooring zero LastActivityAt to
// CreatedAt so older-coord/no-activity projects still sort.
func TestLandingSortsByActivity(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	mid := time.Now().Add(-2 * 24 * time.Hour)
	fresh := time.Now().Add(-15 * time.Minute)
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projects: []wire.Project{
			{ID: 1, Name: "stale-one", CreatedAt: old, LastActivityAt: old},
			{ID: 2, Name: "hot-one", CreatedAt: mid, LastActivityAt: fresh},
			{ID: 3, Name: "warm-one", CreatedAt: mid, LastActivityAt: mid},
			// LastActivityAt zero → must floor to CreatedAt.
			{ID: 4, Name: "legacy-floor", CreatedAt: time.Now().Add(-1 * time.Hour)},
		},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	order := []string{"hot-one", "legacy-floor", "warm-one", "stale-one"}
	prev := 0
	for _, name := range order {
		idx := strings.Index(body, name)
		if idx < 0 {
			t.Fatalf("missing %q in body", name)
		}
		if idx < prev {
			t.Fatalf("wrong sort order: %q appeared before previous; want order %v", name, order)
		}
		prev = idx
	}
}

// TestRelativeTime: the human label coarsens correctly across
// the breakpoints (minute / hour / day / week+) and floors a
// zero time to a dash.
func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, "—"},
		{"sub-minute", now.Add(-30 * time.Second), "just now"},
		{"minutes", now.Add(-5 * time.Minute), "5m ago"},
		{"hours", now.Add(-3 * time.Hour), "3h ago"},
		{"days", now.Add(-2 * 24 * time.Hour), "2d ago"},
		{"week-plus drops to date", now.Add(-30 * 24 * time.Hour), "Apr 20"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := relativeTime(now, c.t)
			if got != c.want {
				t.Errorf("relativeTime(%v) = %q, want %q", c.t, got, c.want)
			}
		})
	}
}

// TestSettingsArchiveControls: owner sees the Archive
// disclosure on an active project, and Restore + an archived
// banner on an archived one.
func TestSettingsArchiveControls(t *testing.T) {
	active := newTestServer(t, &fakeFC{username: "tamer", projDetail: ownerProjArchived(false)})
	rr := httptest.NewRecorder()
	active.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	b := rr.Body.String()
	if !strings.Contains(b, `action="/p/1/archive"`) || !strings.Contains(b, "Archive project") {
		t.Errorf("active project should offer Archive; body: %q", b)
	}
	if strings.Contains(b, `action="/p/1/restore"`) {
		t.Errorf("active project must not offer Restore")
	}

	arch := newTestServer(t, &fakeFC{username: "tamer", projDetail: ownerProjArchived(true)})
	rr2 := httptest.NewRecorder()
	arch.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/p/1/settings", nil))
	b2 := rr2.Body.String()
	if !strings.Contains(b2, `action="/p/1/restore"`) || !strings.Contains(b2, "is <strong>archived</strong>") {
		t.Errorf("archived project should show Restore + banner; body: %q", b2)
	}
	if strings.Contains(b2, `action="/p/1/archive"`) {
		t.Errorf("archived project must not offer Archive")
	}
}

// TestArchiveRestoreActions: POST archive/restore call the
// service with the right flag and banner the coord status; a
// coord refusal is a banner, not a 5xx.
func TestArchiveRestoreActions(t *testing.T) {
	fc := &fakeFC{username: "tamer", projDetail: ownerProjArchived(false)}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/archive", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.setArchivedID != 1 || !fc.setArchivedFlag {
		t.Errorf("archive: SetProjectArchived(pid=%d, archive=%v)", fc.setArchivedID, fc.setArchivedFlag)
	}
	if !strings.Contains(rr.Body.String(), "Project archived") {
		t.Errorf("expected archived banner; body: %q", rr.Body.String())
	}

	fc2 := &fakeFC{username: "tamer", projDetail: ownerProjArchived(true)}
	s2 := newTestServer(t, fc2)
	req2 := httptest.NewRequest(http.MethodPost, "/p/1/restore", nil)
	req2.Header.Set("Origin", "http://127.0.0.1:8080")
	rr2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr2, req2)
	if fc2.setArchivedFlag {
		t.Errorf("restore should call SetProjectArchived(archive=false)")
	}

	// Coord refusal (e.g. non-terminal runs) → banner on 200.
	fc3 := &fakeFC{username: "tamer", projDetail: ownerProjArchived(false),
		setArchivedErr: fmt.Errorf("project has 2 non-terminal runs")}
	s3 := newTestServer(t, fc3)
	req3 := httptest.NewRequest(http.MethodPost, "/p/1/archive", nil)
	req3.Header.Set("Origin", "http://127.0.0.1:8080")
	rr3 := httptest.NewRecorder()
	s3.Handler().ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("refusal status: got %d, want 200", rr3.Code)
	}
	if !strings.Contains(rr3.Body.String(), "non-terminal runs") {
		t.Errorf("expected coord refusal bannered; body: %q", rr3.Body.String())
	}
}

// TestLeaveProjectFull: a full leave (membership removed)
// redirects away — the project is no longer viewable for this
// user. HX-Request → HX-Redirect header; plain → 303.
func TestLeaveProjectFull(t *testing.T) {
	fc := &fakeFC{
		username:     "tamer",
		projDetail:   ownerProjDetail(),
		leaveSummary: "membership removed; local clone removed",
	}
	s := newTestServer(t, fc)

	// HTMX path.
	req := httptest.NewRequest(http.MethodPost, "/p/1/leave", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Header().Get("HX-Redirect") != "/" {
		t.Errorf("HX-Redirect: got %q, want /", rr.Header().Get("HX-Redirect"))
	}
	if fc.leftProject != 1 || fc.leftKeepMembership {
		t.Errorf("LeaveProject got (pid=%d keep=%v)", fc.leftProject, fc.leftKeepMembership)
	}

	// Non-HTMX path → 303 to /.
	fc2 := &fakeFC{username: "tamer", projDetail: ownerProjDetail(), leaveSummary: "ok"}
	s2 := newTestServer(t, fc2)
	req2 := httptest.NewRequest(http.MethodPost, "/p/1/leave", nil)
	req2.Header.Set("Origin", "http://127.0.0.1:8080")
	rr2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusSeeOther || rr2.Header().Get("Location") != "/" {
		t.Errorf("non-HTMX: got %d Location=%q, want 303 /", rr2.Code, rr2.Header().Get("Location"))
	}
}

// TestLeaveProjectKeepMembership: keep_membership=true wipes the
// clone only — membership intact, so the page stays with a
// notice rather than redirecting.
func TestLeaveProjectKeepMembership(t *testing.T) {
	fc := &fakeFC{
		username:     "tamer",
		projDetail:   ownerProjDetail(),
		leaveSummary: "local clone removed — membership kept",
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/leave",
		strings.NewReader("keep_membership=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !fc.leftKeepMembership {
		t.Errorf("expected keep_membership=true passed through")
	}
	if !strings.Contains(rr.Body.String(), "membership kept") {
		t.Errorf("expected keep-membership notice; body: %q", rr.Body.String())
	}
	if rr.Header().Get("HX-Redirect") != "" {
		t.Errorf("keep-membership must not redirect")
	}
}

// TestLeaveProjectSoleOwnerRefused: a coord refusal (sole
// owner) surfaces as a banner on a re-rendered page, not a 5xx
// and not a redirect.
func TestLeaveProjectSoleOwnerRefused(t *testing.T) {
	fc := &fakeFC{
		username:   "tamer",
		projDetail: ownerProjDetail(),
		leaveErr:   fmt.Errorf("cannot remove the sole remaining owner — promote another member first"),
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/leave", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if rr.Header().Get("HX-Redirect") != "" {
		t.Errorf("a refused leave must not redirect")
	}
	if !strings.Contains(rr.Body.String(), "sole remaining owner") {
		t.Errorf("expected sole-owner refusal bannered; body: %q", rr.Body.String())
	}
}

// TestRunView: GET /p/{pid}/r/{seq} renders run header, mermaid
// block, and task list with per-task links to /t/{taskID}.
func TestRunView(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		runDetail: &service.RunDetail{
			Run: wire.Run{
				ID: 10, ProjectID: 1, Seq: 1, Name: "Hello run",
				State: "active", Branch: "main", TaskCount: 3,
			},
			Tasks: []service.TaskSummary{
				{ID: "1:1:draft", Action: "answer", State: "ready", AssignedTo: []string{"tamer"}},
				{ID: "1:1:review", Action: "review", State: "pending", DependsOn: []string{"draft"}},
			},
			DiagramMermaid: "graph TD\n  draft --> review",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/r/1", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Hello run",
		"1:1:draft",
		"1:1:review",
		"graph TD",
		"/p/1/t/1:1:draft",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestRunViewBadSeq: non-numeric run seq returns 400.
func TestRunViewBadSeq(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/p/1/r/notanint", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
}

// TestRunViewWorkflowYAML: the frozen-YAML disclosure renders
// when the run carries YAML (coord honored ?include=yaml) and
// is omitted otherwise (older coord / not requested).
func TestRunViewWorkflowYAML(t *testing.T) {
	withYAML := newTestServer(t, &fakeFC{
		username: "tamer",
		runDetail: &service.RunDetail{
			Run: wire.Run{
				Seq: 1, Name: "r", State: "active",
				YAML: "name: smoke\nversion: 1\ntasks:\n  - id: t\n    action: answer",
			},
		},
	})
	rr := httptest.NewRecorder()
	withYAML.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/r/1", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "Workflow YAML") || !strings.Contains(body, "name: smoke") {
		t.Errorf("expected workflow-yaml disclosure with content; body: %q", body)
	}

	noYAML := newTestServer(t, &fakeFC{
		username:  "tamer",
		runDetail: &service.RunDetail{Run: wire.Run{Seq: 1, Name: "r", State: "active"}},
	})
	rr2 := httptest.NewRecorder()
	noYAML.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/p/1/r/1", nil))
	if strings.Contains(rr2.Body.String(), "Workflow YAML") {
		t.Errorf("disclosure must be omitted when run has no YAML")
	}
}

// TestRunLivePollGatedOnNonTerminal: the #run-live region
// auto-polls (hx-trigger every 20s) while the run is active,
// and carries NO poll attribute once terminal so the swap
// self-stops.
func TestRunLivePollGatedOnNonTerminal(t *testing.T) {
	mk := func(state string) string {
		s := newTestServer(t, &fakeFC{
			username:  "tamer",
			runDetail: &service.RunDetail{Run: wire.Run{Seq: 1, Name: "r", State: state}},
		})
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/r/1", nil))
		return rr.Body.String()
	}
	active := mk("active")
	if !strings.Contains(active, `id="run-live"`) {
		t.Fatal("expected #run-live wrapper")
	}
	if !strings.Contains(active, `hx-trigger="every 20s"`) ||
		!strings.Contains(active, `hx-select="#run-live"`) {
		t.Errorf("active run should auto-poll #run-live; body: %q", active)
	}
	done := mk("completed")
	if strings.Contains(done, `hx-trigger="every 20s"`) {
		t.Errorf("terminal run must not carry the poll attribute (self-stop)")
	}
	if !strings.Contains(done, `id="run-live"`) {
		t.Errorf("the #run-live wrapper should still render for a terminal run")
	}
}

// TestExportRun: GET /p/{pid}/r/{seq}/export.md streams the
// Markdown report as an attachment with the right headers, and
// the run page links to it.
func TestExportRun(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		exportMD: "# Run: Hello run\n\nProject: #1, Run: #1\n",
		runDetail: &service.RunDetail{
			Run: wire.Run{ID: 10, ProjectID: 1, Seq: 1, Name: "Hello run", State: "completed"},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/p/1/r/1/export.md", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type: got %q", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != `attachment; filename="run-1.md"` {
		t.Errorf("Content-Disposition: got %q", cd)
	}
	if !strings.Contains(rr.Body.String(), "# Run: Hello run") {
		t.Errorf("body missing report content: %q", rr.Body.String())
	}

	// Run page should link to the download.
	req2 := httptest.NewRequest(http.MethodGet, "/p/1/r/1", nil)
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if !strings.Contains(rr2.Body.String(), "/p/1/r/1/export.md") {
		t.Errorf("run page missing export link")
	}
}

// TestExportRunError: a FatClient export failure surfaces as a
// 502, not a 200 with a bogus file.
func TestExportRunError(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username:  "tamer",
		exportErr: fmt.Errorf("coord unreachable"),
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/r/1/export.md", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", rr.Code)
	}
}

// TestTaskViewAnswer: GET /p/{pid}/t/{taskID} renders prompt +
// state + dependencies for an answer task.
func TestTaskViewAnswer(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID:        "1:1:draft",
			ProjectID: 1,
			RunSeq:    1,
			TaskDefID: "draft",
			State:     "ready",
			Action:    "answer",
			Prompt:    "Write a one-paragraph greeting",
			Branch:    "main",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:draft", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"1:1:draft", "Write a one-paragraph greeting", "ready", "answer"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestTaskViewReview: review task surfaces ReviewsTarget.
func TestTaskViewReview(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:rev", ProjectID: 1, RunSeq: 1, TaskDefID: "rev",
			Action: "review", State: "pending",
			ReviewsTarget: "draft",
			DependsOn:     "draft",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:rev", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{"Review target", "draft", "Depends on"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestTaskViewVote: vote options decode + render.
func TestTaskViewVote(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:judge", ProjectID: 1, RunSeq: 1, TaskDefID: "judge",
			Action: "vote", State: "ready",
			VoteOptionsJSON: `[{"id":"good","label":"Looks good"},{"id":"bad","label":"Reject"}]`,
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:judge", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{"Options", "good", "Looks good", "bad", "Reject"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestTaskViewWithResult: when a task has a CommitSHA + a
// readable result, the page renders the "Submitted result"
// section with the body.
func TestTaskViewWithResult(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1, TaskDefID: "draft",
			State: "accepted", Action: "answer",
			ResultDir: ".enju/runs/1-toy/draft", CommitSHA: "abc123def456789",
		},
		taskResult: "Hello, world. This is the rendered answer.",
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:draft", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{
		"Submitted result",
		"Hello, world. This is the rendered answer.",
		"abc123def456",
		".enju/runs/1-toy/draft/result.md",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestTaskViewProducedFiles: a task that wrote artifacts shows
// a "Files produced" section listing only its own outputs
// (artifact-index last_task_id == this task), each linking to
// the artifact viewer. A different task's artifact is excluded.
func TestTaskViewProducedFiles(t *testing.T) {
	tracked := true
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:plot", ProjectID: 1, RunSeq: 1, TaskDefID: "plot",
			State: "accepted", Action: "compute", Branch: "mustache-engine-1",
		},
		artifacts: []service.ArtifactResponse{
			{Path: "figures/fig1.png", LastTaskID: "1:1:plot", CommitSHA: "abc1234567890def", Tracked: &tracked, UpdatedAt: "2026-05-18"},
			{Path: "data/raw.csv", LastTaskID: "1:1:ingest", CommitSHA: "ffff000011112222", Tracked: &tracked, UpdatedAt: "2026-05-17"},
		},
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:plot", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	// The index is branch-keyed; the task page must query the
	// task's run branch, not the project default.
	if fc.listedArtifactsBranch != "mustache-engine-1" {
		t.Errorf("ListArtifacts branch: got %q, want the task's run branch", fc.listedArtifactsBranch)
	}
	if !strings.Contains(body, "Files produced") {
		t.Errorf("expected Files produced section")
	}
	if !strings.Contains(body, `<details class="produced-file">`) ||
		!strings.Contains(body, "figures/fig1.png") {
		t.Errorf("expected a collapsible produced-file row; body: %q", body)
	}
	// Lazy body fetches the content fragment on expand, carrying
	// path + the task's run branch; trigger is the details toggle.
	if !strings.Contains(body, `hx-get="/p/1/t/1:1:plot/file?path=figures%2Ffig1.png`) ||
		!strings.Contains(body, "branch=mustache-engine-1") ||
		!strings.Contains(body, `hx-trigger="toggle once from:closest details"`) {
		t.Errorf("expected lazy hx-get for the file content on its run branch; body: %q", body)
	}
	// Full-page link-out still available (history/metadata).
	if !strings.Contains(body, `href="/p/1/artifacts/show/figures/fig1.png?branch=mustache-engine-1"`) {
		t.Errorf("expected the full-page link-out on the run branch; body: %q", body)
	}
	if strings.Contains(body, "data/raw.csv") {
		t.Errorf("another task's artifact must not appear here")
	}
}

// TestTaskFileFragment: the lazy endpoint returns a bare
// highlighter-ready <pre> (data-lang by extension) with the
// content, and a muted note when empty.
func TestTaskFileFragment(t *testing.T) {
	// D4: the fragment now requires the task to exist AND the
	// path to be one of its produced artifacts; content is read
	// on the task's OWN run branch, not a caller ?branch=.
	tracked := true
	taskMeta := func() *service.TaskMeta {
		return &service.TaskMeta{
			ID: "1:1:plot", ProjectID: 1, RunSeq: 1, TaskDefID: "plot",
			State: "accepted", Action: "compute", Branch: "mustache-engine-1",
		}
	}
	owned := []service.ArtifactResponse{
		{Path: "src/context.py", LastTaskID: "1:1:plot", Tracked: &tracked},
	}

	// Happy path: real task, owned path → highlighted pre, read
	// on the task's run branch (the caller's ?branch is ignored).
	raw, _ := json.Marshal(map[string]interface{}{
		"path": "src/context.py", "content": "def f():\n    return 1\n",
	})
	fc := &fakeFC{username: "tamer", taskMeta: taskMeta(),
		artifacts: owned, artifactContent: raw}
	s := newTestServer(t, fc)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/p/1/t/1:1:plot/file?path=src/context.py&branch=attacker-supplied", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<html") {
		t.Errorf("fragment must not include the layout; got: %q", body)
	}
	if !strings.Contains(body, `<pre class="result-content" data-lang="py">`) ||
		!strings.Contains(body, "def f():") {
		t.Errorf("expected highlighter-ready pre with content; got: %q", body)
	}
	if fc.gotArtifactBranch != "mustache-engine-1" {
		t.Errorf("must read on the task's run branch, not caller ?branch; got %q", fc.gotArtifactBranch)
	}

	// Unknown task → (task not found); no content fetch.
	noTask := &fakeFC{username: "tamer", getTaskErr: fmt.Errorf("task not found")}
	s2 := newTestServer(t, noTask)
	rr2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet,
		"/p/1/t/bogus/file?path=secret.txt", nil))
	if !strings.Contains(rr2.Body.String(), "task not found") {
		t.Errorf("unknown task should be rejected; got: %q", rr2.Body.String())
	}
	if noTask.gotArtifactPath != "" {
		t.Errorf("must not fetch content for an unknown task; fetched %q", noTask.gotArtifactPath)
	}

	// Real task but a path it did NOT produce → rejected. The
	// taskID segment is no longer a decorative pass-through to
	// arbitrary repo files.
	fc3 := &fakeFC{username: "tamer", taskMeta: taskMeta(), artifacts: owned}
	s3 := newTestServer(t, fc3)
	rr3 := httptest.NewRecorder()
	s3.Handler().ServeHTTP(rr3, httptest.NewRequest(http.MethodGet,
		"/p/1/t/1:1:plot/file?path=../../etc/passwd", nil))
	if !strings.Contains(rr3.Body.String(), "not a file this task produced") {
		t.Errorf("unowned path must be rejected; got: %q", rr3.Body.String())
	}
	if fc3.gotArtifactPath != "" {
		t.Errorf("must not fetch an unowned path; fetched %q", fc3.gotArtifactPath)
	}

	// Owned but empty/binary → muted note, not an empty <pre>.
	empty, _ := json.Marshal(map[string]interface{}{"path": "src/context.py", "content": ""})
	fc4 := &fakeFC{username: "tamer", taskMeta: taskMeta(),
		artifacts: owned, artifactContent: empty}
	s4 := newTestServer(t, fc4)
	rr4 := httptest.NewRecorder()
	s4.Handler().ServeHTTP(rr4, httptest.NewRequest(http.MethodGet,
		"/p/1/t/1:1:plot/file?path=src/context.py", nil))
	if !strings.Contains(rr4.Body.String(), "not viewable here") {
		t.Errorf("empty content should yield a muted note; got: %q", rr4.Body.String())
	}
}

// TestTaskViewNoProducedFiles: an answer task that wrote
// nothing has no Files-produced section (it just hides).
func TestTaskViewNoProducedFiles(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1, TaskDefID: "draft",
			State: "accepted", Action: "answer",
		},
		// artifacts left nil — nothing produced.
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:draft", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), "Files produced") {
		t.Errorf("no-artifact task should not show the Files produced section")
	}
}

// TestTaskViewIterationHistory: when iterations exist, the
// page renders each iter's metadata, verdict label, and body
// (read via ReadResultAtCommit per-iter).
func TestTaskViewIterationHistory(t *testing.T) {
	t1 := time.Date(2026, 5, 4, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 4, 15, 0, 0, 0, time.UTC)
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1, TaskDefID: "draft",
			State: "ready", Action: "answer",
			ResultDir: ".enju/runs/1-toy/draft",
		},
		iterations: []wire.Iteration{
			{
				Seq: 2, Citizen: "tamer", Outcome: "active",
				ClaimedAt: t2, CommitSHA: "newer123",
			},
			{
				Seq: 1, Citizen: "tamer", Outcome: "completed",
				ClaimedAt: t1, SubmittedAt: &t1, CommitSHA: "older456",
				ReviewDecision: "request_changes",
			},
		},
		iterBodies: map[string]string{
			"older456": "First draft text",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:draft", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Iteration history",
		"iter 2",
		"iter 1",
		"request_changes", // verdict badge
		"First draft text", // body inlined for older iter
		"newer123",         // short SHA
		"older456",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestTaskViewWithoutResult: empty result hides the section.
func TestTaskViewWithoutResult(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1, State: "ready",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/1:1:draft", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), "Submitted result") {
		t.Errorf("Submitted result section should not appear when result is empty")
	}
}

// TestTaskViewMissing: returns 404 when FetchTaskMeta returns nil.
func TestTaskViewMissing(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/p/1/t/nope", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rr.Code)
	}
}

// TestInboxGlobal: GET /inbox walks ListProjects + BuildInbox
// per project and merges rows with project context.
func TestInboxGlobal(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projects: []wire.Project{
			{ID: 1, Name: "alpha"},
			{ID: 2, Name: "beta"},
		},
		// Single shared inbox returned for both projects in the
		// fake — proves the merge: each row should appear with
		// its own project tag attached, and we expect 2 rows
		// total (one per project) since the fake returns the
		// same one row both times.
		inbox: &service.InboxResult{
			ProjectClonePresent: true,
			Rows: []inbox.InboxRow{{TaskID: "X:Y:t", Action: "answer"}},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"X:Y:t",
		"alpha",
		"beta",
		"href=\"/p/1\"", // project tag links
		"href=\"/p/2\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestInboxGlobalCloneIssues: a project without a clone is
// surfaced under "Skipped projects" rather than silently dropped.
func TestInboxGlobalCloneIssues(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projects: []wire.Project{{ID: 5, Name: "no-clone-yet"}},
		inbox:    &service.InboxResult{ProjectClonePresent: false},
	})
	req := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{"Skipped projects", "no-clone-yet", "no local clone"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestInboxNoClone: BuildInbox returning ProjectClonePresent=false
// renders the friendly "materialize first" copy.
func TestInboxNoClone(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		inbox:    &service.InboxResult{ProjectClonePresent: false},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/inbox", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not yet materialized") {
		t.Errorf("expected 'not yet materialized' copy")
	}
}

// TestInboxRows: BuildInbox returning rows renders the inbox
// list with task ids + upstream content.
func TestInboxRows(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		inbox: &service.InboxResult{
			ProjectClonePresent: true,
			Rows: []inbox.InboxRow{{
				TaskID: "1:1:review",
				Action: "review",
				Upstream: []inbox.InboxUpstreamRow{{
					TaskID:    "1:1:draft",
					Action:    "answer",
					CommitSHA: "abc1234567890def",
					Content:   "Hello from the draft author!",
				}},
			}},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/inbox", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"1:1:review",
		"1:1:draft",
		"abc123456789", // truncated to 12 chars
		"Hello from the draft author",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Non-truncation: we should NOT see the full SHA.
	if strings.Contains(body, "abc1234567890def") {
		t.Errorf("commit sha not truncated to 12 chars")
	}
}

// TestInboxEmpty: clone present but no rows → "no tasks waiting" copy.
func TestInboxEmpty(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		inbox:    &service.InboxResult{ProjectClonePresent: true},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/inbox", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "No tasks waiting") {
		t.Errorf("expected empty-state copy")
	}
}

// TestEvents: /p/{pid}/events renders rows + custom limit query
// param.
func TestEvents(t *testing.T) {
	ts := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		events: []service.EventRow{
			{Seq: 7, Timestamp: ts, Type: "task_ready", TaskID: "1:1:draft", Citizen: "tamer"},
			{Seq: 6, Timestamp: ts, Type: "run_created"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/events?limit=20", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"task_ready",
		"run_created",
		"1:1:draft",
		"@tamer",
		"latest 20", // limit echoed in subhead
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestEventsForMe: ?for_me=true filters to events naming the
// caller (citizen or assign_to). Events for someone else
// are dropped.
func TestEventsForMe(t *testing.T) {
	ts := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		events: []service.EventRow{
			{Seq: 10, Timestamp: ts, Type: "task_completed", Citizen: "tamer"},
			{Seq: 9, Timestamp: ts, Type: "task_completed", Citizen: "alice"},
			{Seq: 8, Timestamp: ts, Type: "task_ready", AssignTo: "tamer"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/events?for_me=true", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	if strings.Contains(body, "@alice") {
		t.Errorf("for_me filter leaked alice's events")
	}
	if !strings.Contains(body, "filtered to events naming you") {
		t.Errorf("for_me subhead missing")
	}
	// Active chip should be "For me", not "All"
	if !strings.Contains(body, `btn btn-active" href="/p/1/events?for_me=true`) {
		t.Errorf("For me chip not marked active")
	}
}

// TestGlobalEvents: /events walks ListProjects + ListEvents per
// project, merges, shows project tag per row.
func TestGlobalEvents(t *testing.T) {
	ts := time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC)
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		projects: []wire.Project{
			{ID: 1, Name: "alpha"},
			{ID: 2, Name: "beta"},
		},
		events: []service.EventRow{
			{Seq: 10, Timestamp: ts, Type: "task_ready"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"task_ready",
		"alpha",
		"beta",
		"Cross-project event feed",
		`href="/p/1"`,
		`href="/p/2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestNotificationsRedirect: legacy /p/{pid}/notifications →
// 302 to /p/{pid}/events?for_me=true.
func TestNotificationsRedirect(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/p/7/notifications", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/p/7/events?for_me=true" {
		t.Errorf("redirect target: got %q, want /p/7/events?for_me=true", loc)
	}
}

// TestOriginCheckGetPasses: GET / has no Origin header and is
// allowed (read methods are exempt; same-origin link clicks
// don't always set Origin).
func TestOriginCheckGetPasses(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
}

// TestOriginCheckPostMissingOrigin: a write request without an
// Origin header is rejected with 403.
func TestOriginCheckPostMissingOrigin(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rr.Code)
	}
}

// TestOriginCheckPostBadOrigin: a write request with an Origin
// outside the allowlist is rejected.
func TestOriginCheckPostBadOrigin(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	req.Header.Set("Origin", "http://evil.com")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rr.Code)
	}
}

// TestOriginCheckPostAllowed127: a write request from the
// 127.0.0.1 origin passes the middleware (the 404 that follows
// is from the missing route, which is expected — we only care
// the Origin gate let it through).
func TestOriginCheckPostAllowed127(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	// chi 404s the unknown route — but importantly NOT 403.
	if rr.Code == http.StatusForbidden {
		t.Fatalf("Origin check should have allowed 127.0.0.1:8080; got 403")
	}
}

// TestOriginCheckPostAllowedLocalhost: same as above but with
// the localhost variant in Origin.
func TestOriginCheckPostAllowedLocalhost(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == http.StatusForbidden {
		t.Fatalf("Origin check should have allowed localhost:8080; got 403")
	}
}

// --- Write actions ---

// TestClaimAction: POST /p/{pid}/t/{tid}/claim with valid Origin
// passes the middleware, calls Session.ClaimTask, re-renders
// the task page.
func TestClaimAction(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1,
			TaskDefID: "draft", State: "claimed", Action: "answer",
			Branch: "main",
		},
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:draft/claim", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.claimedID != "1:1:draft" {
		t.Errorf("ClaimTask called with task_id %q, want 1:1:draft", fc.claimedID)
	}
}

// TestReleaseAction: POST /release calls ReleaseTask.
func TestReleaseAction(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1,
			TaskDefID: "draft", State: "ready", Action: "answer",
		},
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:draft/release", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.releasedID != "1:1:draft" {
		t.Errorf("ReleaseTask called with %q, want 1:1:draft", fc.releasedID)
	}
}

// TestFailTask: POST /fail with a reason drives FailTask with
// the task ID + reason, and the page re-renders.
func TestFailTask(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1,
			TaskDefID: "draft", State: "claimed", Action: "answer",
			ClaimedBy: "tamer",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("reason=upstream+artifact+missing")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:draft/fail", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.failedID != "1:1:draft" {
		t.Errorf("FailTask task id: got %q, want 1:1:draft", fc.failedID)
	}
	if fc.failedReason != "upstream artifact missing" {
		t.Errorf("FailTask reason: got %q", fc.failedReason)
	}
}

// TestFailTaskMissingReason: an empty reason is rejected with
// the validation banner; FailTask is not called (mirror of the
// MCP tool's required `reason`).
func TestFailTaskMissingReason(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1,
			TaskDefID: "draft", State: "claimed", Action: "answer",
			ClaimedBy: "tamer",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("reason=+++")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:draft/fail", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.failedID != "" {
		t.Errorf("FailTask should not be called on empty reason; got id %q", fc.failedID)
	}
	if !strings.Contains(rr.Body.String(), "reason is required") {
		t.Errorf("expected validation banner, body: %q", rr.Body.String())
	}
}

// TestReviewSubmit: POST /review with valid form invokes
// SubmitTaskResult with the right Decision and Content.
func TestReviewSubmit(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:rev", ProjectID: 1, RunSeq: 1,
			TaskDefID: "rev", State: "claimed", Action: "review",
			ReviewsTarget: "draft",
			Branch:        "main",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("decision=approve&content=looks+good")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:rev/review", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !fc.submitCalled {
		t.Fatalf("SubmitTaskResult not called")
	}
	if fc.submitParams.Decision != "approve" {
		t.Errorf("Decision: got %q, want approve", fc.submitParams.Decision)
	}
	if fc.submitParams.Content != "looks good" {
		t.Errorf("Content: got %q, want 'looks good'", fc.submitParams.Content)
	}
}

// TestReviewSubmitMissingDecision: empty decision returns the
// error banner without calling SubmitTaskResult.
func TestReviewSubmitMissingDecision(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:rev", ProjectID: 1, RunSeq: 1,
			TaskDefID: "rev", Action: "review",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("decision=&content=oops")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:rev/review", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.submitCalled {
		t.Errorf("SubmitTaskResult should not be called on validation failure")
	}
	if !strings.Contains(rr.Body.String(), "decision is required") {
		t.Errorf("expected validation banner; got:\n%s", rr.Body.String())
	}
}

// TestSubmitAnswer: POST /submit on an answer task carries the
// content into SubmitParams; option stays empty.
func TestSubmitAnswer(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1,
			TaskDefID: "draft", State: "claimed", Action: "answer",
			Branch: "main",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("content=hello+world")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:draft/submit", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !fc.submitCalled {
		t.Fatalf("SubmitTaskResult not called")
	}
	if fc.submitParams.Content != "hello world" {
		t.Errorf("Content: got %q, want 'hello world'", fc.submitParams.Content)
	}
	if fc.submitParams.Option != "" {
		t.Errorf("Option should be empty for answer; got %q", fc.submitParams.Option)
	}
	if fc.submitParams.Decision != "" {
		t.Errorf("Decision should be empty for answer; got %q", fc.submitParams.Decision)
	}
}

// TestSubmitAnswerEmpty: empty content is rejected with the
// validation banner; SubmitTaskResult is not called.
func TestSubmitAnswerEmpty(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:draft", ProjectID: 1, RunSeq: 1, Action: "answer", State: "claimed",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("content=%20%20%20") // whitespace only
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:draft/submit", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.submitCalled {
		t.Errorf("SubmitTaskResult should not be called on empty content")
	}
	if !strings.Contains(rr.Body.String(), "answer content is required") {
		t.Errorf("expected validation banner; got:\n%s", rr.Body.String())
	}
}

// TestSubmitVote: POST /submit on a vote task carries the
// option into SubmitParams.
func TestSubmitVote(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:judge", ProjectID: 1, RunSeq: 1,
			TaskDefID: "judge", State: "claimed", Action: "vote",
			Branch: "main",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("option=good&content=looks+fine")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:judge/submit", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.submitParams.Option != "good" {
		t.Errorf("Option: got %q, want 'good'", fc.submitParams.Option)
	}
	if fc.submitParams.Content != "looks fine" {
		t.Errorf("Content: got %q, want 'looks fine'", fc.submitParams.Content)
	}
}

// TestSubmitVoteMissingOption: empty option is rejected.
func TestSubmitVoteMissingOption(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:judge", ProjectID: 1, Action: "vote", State: "claimed",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("option=&content=oops")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:judge/submit", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.submitCalled {
		t.Errorf("SubmitTaskResult should not be called on missing option")
	}
	if !strings.Contains(rr.Body.String(), "option is required") {
		t.Errorf("expected validation banner; got:\n%s", rr.Body.String())
	}
}

// TestSubmitWrongAction: posting /submit on a review task is
// refused with a clear message — review goes through /review.
func TestSubmitWrongAction(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:rev", ProjectID: 1, Action: "review", State: "claimed",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("content=oops")
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:rev/submit", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.submitCalled {
		t.Errorf("SubmitTaskResult should not be called on review action through /submit")
	}
	if !strings.Contains(rr.Body.String(), "submit endpoint handles answer and vote only") {
		t.Errorf("expected wrong-action banner; got:\n%s", rr.Body.String())
	}
}

// TestPauseRun: POST /pause invokes Session.PauseRun with the
// right (project, run) coordinates and re-renders the page.
func TestPauseRun(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		runDetail: &service.RunDetail{
			Run: wire.Run{Seq: 1, Name: "r", State: "paused"},
		},
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/r/1/pause", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.pausedPID != 1 || fc.pausedSeq != 1 {
		t.Errorf("PauseRun called with (%d,%d), want (1,1)", fc.pausedPID, fc.pausedSeq)
	}
}

// TestResumeRun: parallel to TestPauseRun.
func TestResumeRun(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		runDetail: &service.RunDetail{
			Run: wire.Run{Seq: 1, Name: "r", State: "active"},
		},
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/r/1/resume", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.resumedPID != 1 || fc.resumedSeq != 1 {
		t.Errorf("ResumeRun called with (%d,%d), want (1,1)", fc.resumedPID, fc.resumedSeq)
	}
}

// TestTerminateRun: form-encoded reason flows through to
// TerminateRun. Terminated run state renders the terminal
// state badge with no action buttons.
func TestTerminateRun(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		runDetail: &service.RunDetail{
			Run: wire.Run{Seq: 1, Name: "r", State: "terminated"},
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("reason=stuck+in+infinite+loop")
	req := httptest.NewRequest(http.MethodPost, "/p/1/r/1/terminate", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.terminatedReason != "stuck in infinite loop" {
		t.Errorf("reason: got %q, want 'stuck in infinite loop'", fc.terminatedReason)
	}
	// Terminated state should NOT render action buttons.
	if strings.Contains(rr.Body.String(), `action="/p/1/r/1/pause"`) {
		t.Errorf("pause button rendered on terminated run")
	}
	if strings.Contains(rr.Body.String(), `action="/p/1/r/1/terminate"`) {
		t.Errorf("terminate button rendered on terminated run")
	}
}

// TestRunButtonsByState: pause button on active, resume on
// paused, terminate on both — but none on terminated.
func TestRunButtonsByState(t *testing.T) {
	cases := []struct {
		state   string
		wantBtn map[string]bool // path → should appear
	}{
		{"active", map[string]bool{
			"/p/1/r/1/pause": true, "/p/1/r/1/resume": false, "/p/1/r/1/terminate": true,
		}},
		{"waiting", map[string]bool{
			"/p/1/r/1/pause": true, "/p/1/r/1/resume": false, "/p/1/r/1/terminate": true,
		}},
		{"paused", map[string]bool{
			"/p/1/r/1/pause": false, "/p/1/r/1/resume": true, "/p/1/r/1/terminate": true,
		}},
		{"completed", map[string]bool{
			"/p/1/r/1/pause": false, "/p/1/r/1/resume": false, "/p/1/r/1/terminate": false,
		}},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			s := newTestServer(t, &fakeFC{
				username: "tamer",
				runDetail: &service.RunDetail{
					Run: wire.Run{Seq: 1, Name: "r", State: c.state},
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/p/1/r/1", nil)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			body := rr.Body.String()
			for path, shouldAppear := range c.wantBtn {
				present := strings.Contains(body, `action="`+path+`"`)
				if present != shouldAppear {
					t.Errorf("state=%s path=%s: present=%v want=%v",
						c.state, path, present, shouldAppear)
				}
			}
		})
	}
}

// --- Issues ---

// TestIssuesList: GET /p/{pid}/issues renders the table when
// issues exist and pipes the optional ?status= filter through
// to ListIssues.
func TestIssuesList(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		issues: []service.IssueResponse{
			{ID: "ISSUE-001", Seq: 1, Title: "foo broken", Status: "open", Severity: "high", FiledBy: "tamer", FiledAt: "2026-05-05"},
			{ID: "ISSUE-002", Seq: 2, Title: "bar slow", Status: "triaged", Severity: "medium", FiledBy: "alice", FiledAt: "2026-05-05"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/issues?status=open", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"ISSUE-001", "foo broken", "ISSUE-002", "bar slow", "high", "@tamer"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestIssueDetail: GET /p/{pid}/issues/{seq}.
func TestIssueDetail(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		issue: &service.IssueResponse{
			ID: "ISSUE-001", Seq: 1, Title: "foo broken",
			Body: "details here", Status: "open", Severity: "high",
			FiledBy: "tamer", FiledAt: "2026-05-05",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/issues/1", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{"foo broken", "details here", "ISSUE-001", "high"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestFileIssue: POST /p/{pid}/issues with form-encoded fields
// flows into FileIssueParams; on success returns 303 to the
// new issue's detail page.
func TestFileIssue(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		fileIssueResp: &service.FileIssueResponse{
			ID: 42, Seq: 7, Slug: "ISSUE-007",
			Title: "TLS cert expires soon", Status: "open", Severity: "high",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("title=TLS+cert+expires+soon&body=happens+in+prod&severity=high&found_in_run_seq=3")
	req := httptest.NewRequest(http.MethodPost, "/p/1/issues", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/p/1/issues/7" {
		t.Errorf("redirect: got %q, want /p/1/issues/7", loc)
	}
	if fc.filedIssueParams.Title != "TLS cert expires soon" {
		t.Errorf("title: got %q", fc.filedIssueParams.Title)
	}
	if fc.filedIssueParams.FoundInRunSeq != 3 {
		t.Errorf("FoundInRunSeq: got %d, want 3", fc.filedIssueParams.FoundInRunSeq)
	}
}

// TestFileIssueMissingTitle: empty title rejected with banner.
func TestFileIssueMissingTitle(t *testing.T) {
	fc := &fakeFC{username: "tamer"}
	s := newTestServer(t, fc)
	body := strings.NewReader("title=&body=oops")
	req := httptest.NewRequest(http.MethodPost, "/p/1/issues", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if !strings.Contains(rr.Body.String(), "title is required") {
		t.Errorf("expected validation banner; got:\n%s", rr.Body.String())
	}
	if fc.filedIssuePID != 0 {
		t.Errorf("FileIssue should not have been called")
	}
}

// TestTriageIssue: severity flows through.
func TestTriageIssue(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		issue: &service.IssueResponse{
			ID: "ISSUE-001", Seq: 1, Title: "x", Status: "triaged", Severity: "high",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("severity=high")
	req := httptest.NewRequest(http.MethodPost, "/p/1/issues/1/triage", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.triagedSeverity != "high" {
		t.Errorf("severity: got %q, want 'high'", fc.triagedSeverity)
	}
}

// TestCloseIssue: status + closed_by_task_id flow through.
func TestCloseIssue(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		issue: &service.IssueResponse{
			ID: "ISSUE-001", Seq: 1, Title: "x", Status: "closed",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("status=closed&closed_by_task_id=1:2:fix-foo")
	req := httptest.NewRequest(http.MethodPost, "/p/1/issues/1/close", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.closedStatus != "closed" {
		t.Errorf("status: got %q, want 'closed'", fc.closedStatus)
	}
	if fc.closedByTaskID != "1:2:fix-foo" {
		t.Errorf("closed_by_task_id: got %q", fc.closedByTaskID)
	}
}

// --- Me ---

func TestMePage(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		dashboard: &service.DashboardResponse{
			Citizen: service.DashboardCitizen{
				Username: "tamer", Name: "Tamer Gur", Role: "citizen", Kind: "human",
				TasksCompleted: 12, RegisteredAt: "2026-04-01T00:00:00Z",
			},
			ActiveTasks: []service.DashboardTask{
				{ID: "1:1:draft", Seq: 1, RunID: 1},
			},
			RecentTasks: []service.DashboardTask{
				{ID: "1:1:review", Seq: 2, RunID: 1},
			},
		},
		contributions: &service.ContributionsResponse{
			Username: "tamer", TasksCompleted: 12, ReviewsGiven: 5, VotesCast: 3,
			TokensTotal: 10000, ProjectCount: 4,
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Tamer Gur", "@tamer", "1:1:draft", "1:1:review", "12", "5", "10000"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestUpdateProfile(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		savedProfile: &service.CitizenResponse{
			Username: "tamer", Name: "Tamer Updated", Email: "new@example.com",
		},
		dashboard: &service.DashboardResponse{
			Citizen: service.DashboardCitizen{Username: "tamer", Name: "Tamer Updated"},
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=Tamer+Updated&email=new%40example.com")
	req := httptest.NewRequest(http.MethodPost, "/me/profile", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.updateProfileParams.Name == nil || *fc.updateProfileParams.Name != "Tamer Updated" {
		t.Errorf("name: got %v, want 'Tamer Updated'", fc.updateProfileParams.Name)
	}
	if fc.updateProfileParams.Email == nil || *fc.updateProfileParams.Email != "new@example.com" {
		t.Errorf("email: got %v, want 'new@example.com'", fc.updateProfileParams.Email)
	}
	if !strings.Contains(rr.Body.String(), "Profile updated") {
		t.Errorf("expected success banner")
	}
}

// --- Agents (/me roster + register) ---

// TestMeAgentsRoster: GET /me lists the caller's agents and
// always shows the register affordance.
func TestMeAgentsRoster(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		agents: []service.AgentSummary{
			{Username: "rev-bot", Name: "Reviewer", Role: "citizen", Registered: "2026-05-10",
				Tokens: []service.AgentToken{{Label: "ci", IssuedAt: "2026-05-10"}}},
		},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/me", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Agents", "@rev-bot", "Reviewer", "ci", `action="/me/agents"`} {
		if !strings.Contains(body, want) {
			t.Errorf("roster missing %q", want)
		}
	}
}

// TestMeAgentsEmpty: no agents → explicit empty state, register
// form still reachable.
func TestMeAgentsEmpty(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/me", nil))
	if !strings.Contains(rr.Body.String(), "don't own any agents yet") {
		t.Errorf("expected empty-roster copy")
	}
}

// TestRegisterAgent: POST /me/agents registers and reveals the
// one-time token exactly once, with the stash warning.
func TestRegisterAgent(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		registerAgentResult: &service.RegisterAgentResult{
			Username: "tamers-reviewer", Name: "Tamer's Reviewer", ParentName: "tamer",
			Token: "tok_ONLY_ONCE_abc123", Label: "ci-server", Warning: "cannot be retrieved later",
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=Tamer%27s+Reviewer&label=ci-server")
	req := httptest.NewRequest(http.MethodPost, "/me/agents", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.registeredAgent.Name != "Tamer's Reviewer" || fc.registeredAgent.Label != "ci-server" {
		t.Errorf("RegisterAgent params: %+v", fc.registeredAgent)
	}
	body2 := rr.Body.String()
	for _, want := range []string{"tok_ONLY_ONCE_abc123", "shown <em>once</em>", "@tamers-reviewer", "cannot be retrieved later"} {
		if !strings.Contains(body2, want) {
			t.Errorf("token reveal missing %q; body: %q", want, body2)
		}
	}
}

// TestRegisterAgentMissingName: blank name rejected before the
// service; form repopulates.
func TestRegisterAgentMissingName(t *testing.T) {
	fc := &fakeFC{username: "tamer"}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=+&role=citizen")
	req := httptest.NewRequest(http.MethodPost, "/me/agents", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if fc.registeredAgent.Name != "" {
		t.Errorf("service should not be called; got %+v", fc.registeredAgent)
	}
	if !strings.Contains(rr.Body.String(), "agent name is required") {
		t.Errorf("expected validation banner; body: %q", rr.Body.String())
	}
}

// TestRegisterAgentError: a coord rejection surfaces as a banner
// on a 200 with the form repopulated, not a 5xx.
func TestRegisterAgentError(t *testing.T) {
	fc := &fakeFC{
		username:         "tamer",
		registerAgentErr: fmt.Errorf("username already taken in this tenant"),
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=Dup&username=rev-bot")
	req := httptest.NewRequest(http.MethodPost, "/me/agents", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body2 := rr.Body.String()
	if !strings.Contains(body2, "username already taken in this tenant") {
		t.Errorf("expected coord error bannered; body: %q", body2)
	}
	if !strings.Contains(body2, `value="Dup"`) {
		t.Errorf("expected the submitted name repopulated")
	}
}

// --- Artifacts ---

func boolPtr(b bool) *bool { return &b }

// TestArtifactsList: rows render with path, tracked label,
// last writer / task / commit.
func TestArtifactsList(t *testing.T) {
	tracked := true
	untracked := false
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		artifacts: []service.ArtifactResponse{
			{Path: "figures/fig1.png", LastWriter: "tamer", LastTaskID: "1:1:plot", CommitSHA: "abc1234567890def", Tracked: &tracked, UpdatedAt: "2026-05-05"},
			{Path: "data/big.bam", Tracked: &untracked, UpdatedAt: "2026-05-05"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/artifacts", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"figures/fig1.png", "data/big.bam", "tracked", "untracked", "@tamer", "abc123456789"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	_ = boolPtr // keep helper available for future tests
}

// TestArtifactsUntrackedPanel: the untracked-visibility panel
// renders its rows + present/missing states when the report is
// available.
func TestArtifactsUntrackedPanel(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		untracked: &service.UntrackedArtifactReport{
			ResolvedBranch: "main",
			SharedRoot:     "/srv/enju-shared",
			Rows: []service.UntrackedArtifactRow{
				{Path: "data/big.bam", Producer: "1:1:align", LocalState: "present"},
				{Path: "data/missing.bam", Producer: "1:1:align", LocalState: "missing"},
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/artifacts", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Untracked artifact visibility",
		"data/big.bam", "data/missing.bam",
		">present<", ">missing<",
		"/srv/enju-shared",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestArtifactsUntrackedUnavailable: when the diagnostic errors
// (MCP-client mode / no workspace) the page still renders 200
// and shows the muted explanation, not a 502.
func TestArtifactsUntrackedUnavailable(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username:     "tamer",
		untrackedErr: fmt.Errorf("enju_list_untracked_artifacts requires a local workspace (MCP client mode)"),
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/artifacts", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Local visibility check unavailable here") {
		t.Errorf("expected unavailable note; body: %q", body)
	}
	if !strings.Contains(body, "requires a local workspace") {
		t.Errorf("expected the underlying reason surfaced; body: %q", body)
	}
}

// TestArtifactView: GET /p/{pid}/artifacts/show/{path} renders
// metadata + content (decoded from the byte stream).
func TestArtifactView(t *testing.T) {
	meta := map[string]interface{}{
		"path":         "figures/fig1.png",
		"commit_sha":   "abc1234567890def",
		"last_writer":  "tamer",
		"last_task_id": "1:1:plot",
		"content":      "<binary contents would normally be elided>",
	}
	raw, _ := json.Marshal(meta)
	s := newTestServer(t, &fakeFC{
		username:        "tamer",
		artifactContent: raw,
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/artifacts/show/figures/fig1.png", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"figures/fig1.png", "binary contents would normally", "abc123456789", "@tamer"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestArtifactViewCollapsibleAndLang: a source file renders
// inside the collapsible .file-view (open) with the right
// data-lang; a markdown/text file gets no data-lang (plain).
func TestArtifactViewCollapsibleAndLang(t *testing.T) {
	mk := func(path string) string {
		raw, _ := json.Marshal(map[string]interface{}{
			"path": path, "content": "x = 1  # hi\n",
		})
		s := newTestServer(t, &fakeFC{username: "tamer", artifactContent: raw})
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/p/1/artifacts/show/"+path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	py := mk("src/context.py")
	if !strings.Contains(py, `<details class="file-view" open>`) {
		t.Errorf("python artifact should be in an open collapsible; body: %q", py)
	}
	if !strings.Contains(py, `data-lang="py"`) {
		t.Errorf("python artifact should carry data-lang=py")
	}

	md := mk("README.md")
	if !strings.Contains(md, `<details class="file-view" open>`) {
		t.Errorf("markdown should still be collapsible")
	}
	if strings.Contains(md, "data-lang=") {
		t.Errorf("markdown must NOT get a highlighter lang (plain): %q", md)
	}
}

// TestArtifactHistory: GET /p/{pid}/artifacts/history/{path}
// renders entries from the history blob.
func TestArtifactHistory(t *testing.T) {
	wrap := map[string]interface{}{
		"path": "figures/fig1.png",
		"entries": []interface{}{
			map[string]interface{}{
				"commit_sha": "abc1234567890def",
				"task_id":    "1:1:plot",
				"owner":      "tamer",
				"timestamp":  "2026-05-05",
				"annotation": "current",
				"subject":    "Task 1:1:plot by @tamer: result + 1 artifact",
			},
		},
	}
	raw, _ := json.Marshal(wrap)
	s := newTestServer(t, &fakeFC{
		username:        "tamer",
		artifactHistory: raw,
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/artifacts/history/figures/fig1.png", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"figures/fig1.png", "abc123456789", "1:1:plot", "current"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// --- Execute compute task ---

// TestExecuteTask: POST /execute calls ExecuteComputeTask and
// re-renders the task page.
func TestExecuteTask(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		taskMeta: &service.TaskMeta{
			ID: "1:1:script", ProjectID: 1, RunSeq: 1, TaskDefID: "script",
			State: "claimed", Action: "compute", Branch: "main",
		},
	}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:script/execute", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.executedTaskID != "1:1:script" {
		t.Errorf("ExecuteComputeTask called with %q, want 1:1:script", fc.executedTaskID)
	}
}

// TestExecuteRun: POST /execute with optional max_tasks +
// parallel form params flows into ExecuteRunParams.
func TestExecuteRun(t *testing.T) {
	fc := &fakeFC{
		username:  "tamer",
		runDetail: &service.RunDetail{Run: wire.Run{Seq: 1, Name: "r", State: "active"}},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("max_tasks=10&parallel=4")
	req := httptest.NewRequest(http.MethodPost, "/p/1/r/1/execute", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.executedRunPID != 1 || fc.executedRunSeq != 1 {
		t.Errorf("ExecuteRun called with (%d,%d), want (1,1)", fc.executedRunPID, fc.executedRunSeq)
	}
	if fc.executedRunMax != 10 || fc.executedRunPar != 4 {
		t.Errorf("ExecuteRun params: max=%d parallel=%d, want (10, 4)", fc.executedRunMax, fc.executedRunPar)
	}
}

// --- Create project ---

// TestCreateProject: form-encoded fields flow into
// CreateProjectParams; success → 303 redirect to /p/{newID}.
func TestCreateProject(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		createProjectResult: &service.CreateProjectResult{
			ProjectID: 9, CoordResponse: []byte(`{"id":9,"name":"new"}`),
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=alpha&description=test+project&default_branch=main")
	req := httptest.NewRequest(http.MethodPost, "/projects", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/p/9" {
		t.Errorf("redirect: got %q, want /p/9", loc)
	}
	if fc.createdProjectParams.Name != "alpha" {
		t.Errorf("name: got %q", fc.createdProjectParams.Name)
	}
	if fc.createdProjectParams.Description != "test project" {
		t.Errorf("description: got %q", fc.createdProjectParams.Description)
	}
	if fc.createdProjectParams.DefaultBranch != "main" {
		t.Errorf("default_branch: got %q", fc.createdProjectParams.DefaultBranch)
	}
}

// TestCreateProjectMissingName: empty name returns the
// landing page with the form preserved + error banner; no
// CreateProject call.
func TestCreateProjectMissingName(t *testing.T) {
	fc := &fakeFC{username: "tamer"}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=&description=oops")
	req := httptest.NewRequest(http.MethodPost, "/projects", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.createdProjectParams.Name != "" || fc.createdProjectParams.Description != "" {
		// Description-only being set means we got into CreateProject;
		// we shouldn't have. Empty captures = pass.
		if fc.createProjectResult != nil {
			t.Errorf("CreateProject should not have been called")
		}
	}
	body2 := rr.Body.String()
	if !strings.Contains(body2, "name is required") {
		t.Errorf("expected validation banner; got:\n%s", body2)
	}
}

// TestCreateProjectWithPath: form's path field flows into
// CreateProjectParams.Path. The fake doesn't actually init a
// dir (it short-circuits to ProjectID:9), but we verify the
// param threads through cleanly.
func TestCreateProjectWithPath(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		createProjectResult: &service.CreateProjectResult{
			ProjectID: 9, CoordResponse: []byte(`{"id":9,"name":"new"}`),
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=alpha&path=%2Ftmp%2Falpha")
	req := httptest.NewRequest(http.MethodPost, "/projects", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rr.Code)
	}
	if fc.createdProjectParams.Path != "/tmp/alpha" {
		t.Errorf("path: got %q, want /tmp/alpha", fc.createdProjectParams.Path)
	}
}

// TestCreateProjectWithRemoteURL: form's remote_url field flows
// into params.
func TestCreateProjectWithRemoteURL(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		createProjectResult: &service.CreateProjectResult{
			ProjectID: 9, CoordResponse: []byte(`{"id":9}`),
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=alpha&remote_url=git%40github.com%3Aorg%2Frepo.git")
	req := httptest.NewRequest(http.MethodPost, "/projects", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rr.Code)
	}
	if fc.createdProjectParams.RemoteURL != "git@github.com:org/repo.git" {
		t.Errorf("remote_url: got %q", fc.createdProjectParams.RemoteURL)
	}
}

// TestCreateProjectError: backend error re-renders landing
// with banner + form repopulated.
func TestCreateProjectError(t *testing.T) {
	fc := &fakeFC{
		username:         "tamer",
		createProjectErr: fmt.Errorf("name already taken"),
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("name=dupe&description=x")
	req := httptest.NewRequest(http.MethodPost, "/projects", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body2 := rr.Body.String()
	if !strings.Contains(body2, "name already taken") {
		t.Errorf("expected error banner; got:\n%s", body2)
	}
	if !strings.Contains(body2, `value="dupe"`) {
		t.Errorf("expected form repopulated with 'dupe'")
	}
}

// --- Workflows ---

// TestWorkflowsList: GET /p/{pid}/workflows lists the YAML paths
// (path-only — no parse on list, so no Name/Description/Params).
func TestWorkflowsList(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		workflows: []service.WorkflowSummary{
			{Path: "enju.yaml"},
			{Path: "workflows/gwas/enju.yaml"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/workflows", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Workflows", "enju.yaml", "workflows/gwas/enju.yaml",
		`href="/p/1/workflows/show/enju.yaml"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestWorkflowDetail: GET /p/{pid}/workflows/show/{path}
// renders the detail page with the parsed details + declared
// params (DescribeWorkflow DOES parse).
func TestWorkflowDetail(t *testing.T) {
	s := newTestServer(t, &fakeFC{
		username: "tamer",
		loadedWorkflow: &service.LoadedWorkflow{
			Path:      "enju.yaml",
			BundleDir: ".",
			Raw:       []byte("name: LLM Eval\nversion: 1\n"),
			Details: service.WorkflowDetails{
				Path:        "enju.yaml",
				Name:        "LLM Eval",
				Description: "judge benchmark items",
				Params: []service.ParamSummary{
					{Name: "items", Type: "list<string>", Required: true, Description: "item ids"},
				},
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/p/1/workflows/show/enju.yaml", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"LLM Eval", "judge benchmark items", `name="p_items"`, "Create run", "name: LLM Eval"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestCreateRunFromWorkflow: form submit flows params + branch
// into CreateRunFromTemplate (service method retained its name);
// success → 303 redirect to new run.
func TestCreateRunFromWorkflow(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		loadedWorkflow: &service.LoadedWorkflow{
			Path:      "enju.yaml",
			BundleDir: ".",
			Details: service.WorkflowDetails{
				Params: []service.ParamSummary{
					{Name: "items", Type: "list<string>"},
					{Name: "judge", Type: "string"},
				},
			},
		},
		createRunResult: &service.CreateRunFromTemplateResult{
			CoordResponse: []byte(`{"seq":7}`),
		},
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("p_items=i01,i02,i03&p_judge=strict&branch=auto")
	req := httptest.NewRequest(http.MethodPost, "/p/1/workflows/run/enju.yaml", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/p/1/r/7" {
		t.Errorf("redirect: got %q, want /p/1/r/7", loc)
	}
	if fc.createdFromPath != "enju.yaml" {
		t.Errorf("path: got %q", fc.createdFromPath)
	}
	if fc.createdBranch != "auto" {
		t.Errorf("branch: got %q, want auto", fc.createdBranch)
	}
	items, ok := fc.createdParams["items"].([]string)
	if !ok || len(items) != 3 || items[0] != "i01" {
		t.Errorf("items param: got %#v, want [i01 i02 i03]", fc.createdParams["items"])
	}
	if v, _ := fc.createdParams["judge"].(string); v != "strict" {
		t.Errorf("judge param: got %#v", fc.createdParams["judge"])
	}
}

// TestCreateRunFromWorkflowError: failure re-renders detail page
// with banner + repopulated form, no redirect.
func TestCreateRunFromWorkflowError(t *testing.T) {
	fc := &fakeFC{
		username: "tamer",
		loadedWorkflow: &service.LoadedWorkflow{
			Path:      "enju.yaml",
			BundleDir: ".",
			Details: service.WorkflowDetails{
				Params: []service.ParamSummary{{Name: "items", Type: "list<string>"}},
			},
		},
		createRunErr: fmt.Errorf("missing required param"),
	}
	s := newTestServer(t, fc)
	body := strings.NewReader("p_items=&branch=")
	req := httptest.NewRequest(http.MethodPost, "/p/1/workflows/run/enju.yaml", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (re-rendered detail)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing required param") {
		t.Errorf("expected error banner with backend message")
	}
}

const validWorkflowYAML = `name: "ui smoke"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "say hi"
`

// TestNewRunForm: GET renders the paste form.
func TestNewRunForm(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/p/1/new-run", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"New run from YAML", `name="yaml"`, `value="create"`, `value="validate"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form missing %q", want)
		}
	}
}

// TestNewRunValidateOnly: action=validate parses but does NOT
// create; valid YAML → green confirmation, no service call.
func TestNewRunValidateOnly(t *testing.T) {
	fc := &fakeFC{username: "tamer"}
	s := newTestServer(t, fc)
	form := url.Values{"yaml": {validWorkflowYAML}, "action": {"validate"}}
	req := httptest.NewRequest(http.MethodPost, "/p/1/new-run", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if fc.createdYAML != "" {
		t.Errorf("validate must not create a run; got createdYAML=%q", fc.createdYAML)
	}
	if !strings.Contains(rr.Body.String(), "Valid workflow") {
		t.Errorf("expected validation confirmation; body: %q", rr.Body.String())
	}
}

// TestNewRunInvalidYAML: a parse error is surfaced and the run
// is NOT created; the paste is preserved.
func TestNewRunInvalidYAML(t *testing.T) {
	fc := &fakeFC{username: "tamer"}
	s := newTestServer(t, fc)
	bad := "name: [unterminated\ntasks: nope"
	form := url.Values{"yaml": {bad}, "action": {"create"}}
	req := httptest.NewRequest(http.MethodPost, "/p/1/new-run", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.createdYAML != "" {
		t.Errorf("invalid YAML must not reach CreateRunFromYAML; got %q", fc.createdYAML)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "invalid workflow YAML") {
		t.Errorf("expected parse-error banner; body: %q", body)
	}
	if !strings.Contains(body, "unterminated") {
		t.Errorf("expected the paste preserved in the textarea; body: %q", body)
	}
}

// TestNewRunCreateRedirects: valid YAML + action=create calls
// the service and redirects to the created run.
func TestNewRunCreateRedirects(t *testing.T) {
	fc := &fakeFC{
		username:        "tamer",
		createRunResult: &service.CreateRunFromTemplateResult{CoordResponse: []byte(`{"seq":7}`)},
	}
	s := newTestServer(t, fc)
	form := url.Values{"yaml": {validWorkflowYAML}, "branch": {"auto"}, "action": {"create"}}
	req := httptest.NewRequest(http.MethodPost, "/p/1/new-run", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if fc.createdYAML == "" {
		t.Fatalf("CreateRunFromYAML not called")
	}
	if fc.createdYAMLBranch != "auto" {
		t.Errorf("branch passthrough: got %q, want auto", fc.createdYAMLBranch)
	}
	if rr.Header().Get("HX-Redirect") != "/p/1/r/7" {
		t.Errorf("HX-Redirect: got %q, want /p/1/r/7", rr.Header().Get("HX-Redirect"))
	}
}

// TestNewRunCreateError: a coord rejection (e.g. params block
// the inline path doesn't collect) re-renders with the error
// and the paste intact, not a 5xx.
func TestNewRunCreateError(t *testing.T) {
	fc := &fakeFC{
		username:     "tamer",
		createRunErr: fmt.Errorf("missing required param: items"),
	}
	s := newTestServer(t, fc)
	form := url.Values{"yaml": {validWorkflowYAML}, "action": {"create"}}
	req := httptest.NewRequest(http.MethodPost, "/p/1/new-run", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing required param: items") {
		t.Errorf("expected coord error bannered; body: %q", rr.Body.String())
	}
}

// TestActionsRejectedWithoutOrigin: confirms write actions are
// CSRF-gated end-to-end. POST without Origin → 403, even for
// the ClaimTask path.
func TestActionsRejectedWithoutOrigin(t *testing.T) {
	fc := &fakeFC{username: "tamer"}
	s := newTestServer(t, fc)
	req := httptest.NewRequest(http.MethodPost, "/p/1/t/1:1:draft/claim", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rr.Code)
	}
	if fc.claimedID != "" {
		t.Errorf("ClaimTask should not have been called; got task_id %q", fc.claimedID)
	}
}

// TestStaticServe: /static/app.css resolves to the embedded CSS.
func TestStaticServe(t *testing.T) {
	s := newTestServer(t, &fakeFC{username: "tamer"})
	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Placeholder stylesheet") {
		t.Errorf("expected placeholder stylesheet content; got:\n%s", body)
	}
}
