package webui

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// fatClient is the local interface webui programs against. The
// real *service.FatClient satisfies it implicitly via Go's
// structural typing — no adapter, no type assertion. The
// interface acts as the explicit list of methods this package
// touches, so a coord-side surface change becomes a
// compile-time error here rather than a silent runtime miss.
//
// Methods are added as handlers consume them; do not pre-list
// the entire FatClient API. The interface is documentation of
// what the UI actually uses.
type fatClient interface {
	// Identity / accessors
	Username() string

	// Read-side views (the v1 gap-fills + existing reads)
	ListProjects(ctx context.Context) ([]wire.Project, error)
	ListMaterializedProjects() ([]service.MaterializedProject, error)
	GetProject(ctx context.Context, projectID int64) (*service.ProjectDetail, error)
	ListRuns(ctx context.Context, projectID int64) ([]wire.Run, error)
	GetRun(ctx context.Context, projectID int64, runSeq int) (*service.RunDetail, error)
	ListEvents(ctx context.Context, projectID int64, opts service.ListEventsOpts) ([]service.EventRow, error)
	BuildInbox(ctx context.Context, projectID int64, username string) (*service.InboxResult, error)
	FetchTaskMeta(ctx context.Context, taskID string) (*service.TaskMeta, error)
	ReadTaskResult(ctx context.Context, taskID string) (string, bool, error)
	ListTaskIterations(ctx context.Context, taskID string) ([]wire.Iteration, error)
	ReadResultAtCommit(ctx context.Context, projectID int64, commitSHA, resultDir string) (string, bool, error)

	// Write-side actions — task level
	ClaimTask(ctx context.Context, params service.ClaimParams) (*service.ClaimResult, error)
	ReleaseTask(ctx context.Context, taskID string) error
	SubmitTaskResult(ctx context.Context, params service.SubmitParams) *service.SubmitResult
	// FailTask drives the task to terminal `failed` with a
	// required reason (mirror of enju_fail_task). Blocks
	// descendants; recoverable only via invalidate_task.
	FailTask(ctx context.Context, taskID, reason string) error

	// Write-side actions — run level
	PauseRun(ctx context.Context, projectID int64, runSeq int) error
	ResumeRun(ctx context.Context, projectID int64, runSeq int) error
	TerminateRun(ctx context.Context, projectID int64, runSeq int, reason string) error

	// Issues — read + write
	ListIssues(ctx context.Context, projectID int64, opts service.IssueListOpts) ([]service.IssueResponse, error)
	GetIssue(ctx context.Context, projectID int64, seq int) (*service.IssueResponse, error)
	FileIssue(ctx context.Context, projectID int64, params service.FileIssueParams) (*service.FileIssueResponse, error)
	TriageIssue(ctx context.Context, projectID int64, seq int, severity string) (*service.IssueResponse, error)
	CloseIssue(ctx context.Context, projectID int64, seq int, status, closedByTaskID string) (*service.IssueResponse, error)

	// Templates — list, describe, instantiate. Identity is read
	// from the FatClient's coord client (no need to thread
	// authorName/authorEmail through the interface here — the
	// CreateRunFromTemplate signature carries them because
	// commit identity is Caller responsibility, not coord).
	ListTemplates(ctx context.Context, projectID int64) ([]service.TemplateSummary, error)
	DescribeTemplate(ctx context.Context, projectID int64, templatePath string) (*service.LoadedTemplate, error)
	CreateRunFromTemplate(ctx context.Context, projectID int64, templatePath string, params map[string]interface{}, branch, authorName, authorEmail string) (*service.CreateRunFromTemplateResult, error)
	CommitAuthor(ctx context.Context) (name, email string)

	// Projects — write
	CreateProject(ctx context.Context, params service.CreateProjectParams) (*service.CreateProjectResult, error)
	// SyncProjectToRemote pushes local HEAD to the coord-known
	// remote (mirror of enju_project_sync). Fast-forward
	// succeeds; a diverged remote is refused unless force=true
	// (destructive). Errors when no remote is configured —
	// no-origin is first-class post-Phase-8, so the UI gates
	// the action on Project.RemoteURL and treats the error as a
	// friendly note, not a failure.
	SyncProjectToRemote(ctx context.Context, projectID int64, force bool) (map[string]interface{}, error)
	// Project membership writes (mirror of
	// enju_add/remove_project_member, promote_member,
	// demote_owner). Owner-only is enforced coord-side; the
	// handlers surface the coord error as a banner. role "" on
	// add lets the coord default it; SetProjectMemberRole's
	// changed=false means the member already held that role.
	AddProjectMember(ctx context.Context, projectID int64, username, role string) error
	RemoveProjectMember(ctx context.Context, projectID int64, username string) error
	SetProjectMemberRole(ctx context.Context, projectID int64, username, role string) (changed bool, err error)
	// Project settings writes (mirror of
	// enju_set_project_default_branch, enju_set_project_remote).
	// The returned warning is non-fatal — the coord update
	// landed; warning != "" means only the local materialize /
	// mirror step had trouble. RemoteStatusReport is the
	// read-only local-vs-remote comparison (enju_project_remote_status),
	// best-effort: errors in MCP-client mode without a workspace.
	SetProjectDefaultBranch(ctx context.Context, projectID int64, branch string) (warning string, err error)
	SetProjectRemote(ctx context.Context, projectID int64, remoteURL string) (warning string, err error)
	RemoteStatusReport(ctx context.Context, projectID int64) (map[string]interface{}, error)
	// LeaveProject removes the caller's own membership (unless
	// keepMembership) and wipes the local clone (mirror of
	// enju_leave_project). Refused by the coord when the caller
	// is the sole owner — surfaces as err.
	LeaveProject(ctx context.Context, projectID int64, keepMembership bool) (summary string, err error)

	// Compute task execution — single + bulk
	ExecuteComputeTask(ctx context.Context, taskID string) (*service.ExecuteOutcome, error)
	ExecuteRun(ctx context.Context, params service.ExecuteRunParams) (*service.ExecuteRunResult, error)

	// Run export — Markdown report. Pure read (two coord GETs +
	// string build, no disk write / no git commit), so safe to
	// stream straight to the browser as a download on a GET.
	ExportRunMarkdown(ctx context.Context, projectID int64, runSeq int) (string, error)

	// Artifacts — read only (list / get / history)
	ListArtifacts(ctx context.Context, projectID int64, opts service.ListArtifactsOpts) ([]service.ArtifactResponse, error)
	GetArtifactContent(ctx context.Context, projectID int64, path string) ([]byte, error)
	GetArtifactHistory(ctx context.Context, projectID int64, path string) ([]byte, error)
	// ListUntrackedArtifacts is the local-workspace visibility
	// diagnostic (mirror of enju_list_untracked_artifacts):
	// per untracked artifact, is the byte payload actually
	// present in this clone's bigfiles dir, missing, or a
	// symlink into shared storage. Errors in MCP-client mode
	// (no local workspace) — caller treats that as "skip the
	// panel", not a page failure.
	ListUntrackedArtifacts(ctx context.Context, projectID int64, branch string) (*service.UntrackedArtifactReport, error)

	// "Me" — calling citizen's dashboard / contributions / profile
	GetDashboard(ctx context.Context) (*service.DashboardResponse, error)
	GetContributions(ctx context.Context, username string) (*service.ContributionsResponse, error)
	UpdateProfile(ctx context.Context, params service.UpdateProfileParams) (*service.CitizenResponse, error)
}

// Config holds the boot-time settings for the UI server. All
// fields except DevViewsFS / DevStaticFS are required.
type Config struct {
	// FC is the FatClient handle this server consumes. Must
	// already be constructed with a valid coord client and
	// workspace. webui never builds one itself.
	FC fatClient

	// Logger is the slog handle for request logging and
	// diagnostics. nil falls back to slog.Default().
	Logger *slog.Logger

	// Dev toggles dev-mode behaviors: re-parse templates per
	// request, no-cache headers, ?debug overlay. Production
	// builds set this to false.
	Dev bool

	// DevViewsFS / DevStaticFS override the embedded FS in dev
	// mode. Set them to os.DirFS(...) at the project's source
	// paths so template/static edits show on save without
	// rebuilding the binary. Ignored when Dev=false.
	DevViewsFS  fs.FS
	DevStaticFS fs.FS

	// Port is the bound port. Required for the Origin-check
	// middleware to construct the allowlist
	// (http://127.0.0.1:PORT, http://localhost:PORT). Bind
	// happens in cmd/enju, not here, so we accept the value as
	// configuration rather than peeking at a listener.
	Port int
}

// Server is the UI HTTP server. One instance per process. Holds
// only references — no per-request state lives here.
type Server struct {
	fc       fatClient
	tmpl     *templateSet
	staticFS fs.FS
	logger   *slog.Logger
	dev      bool
	port     int
}

// New constructs a Server from cfg. Parses templates (in
// production) and prepares the static FS. Does not bind a port
// — pass the returned Handler() to http.ListenAndServe.
func New(cfg Config) (*Server, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.FC == nil {
		return nil, fmt.Errorf("webui: Config.FC is required")
	}

	viewsFS, err := pickViewsFS(cfg.Dev, cfg.DevViewsFS)
	if err != nil {
		return nil, fmt.Errorf("webui: views fs: %w", err)
	}
	tmpl, err := newTemplateSet(cfg.Dev, viewsFS)
	if err != nil {
		return nil, fmt.Errorf("webui: parse templates: %w", err)
	}

	staticFS, err := pickStaticFS(cfg.Dev, cfg.DevStaticFS)
	if err != nil {
		return nil, fmt.Errorf("webui: static fs: %w", err)
	}

	return &Server{
		fc:       cfg.FC,
		tmpl:     tmpl,
		staticFS: staticFS,
		logger:   logger,
		dev:      cfg.Dev,
		port:     cfg.Port,
	}, nil
}

// Handler returns the HTTP handler this server exposes. Wires
// middleware, routes, and static asset serving.
func (s *Server) Handler() http.Handler {
	return s.router()
}
