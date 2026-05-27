package webui

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// runPage is the data shape for views/run.html. ProjectID is
// kept on the envelope (rather than re-fetching the project)
// so links can render `/p/{ProjectID}/...` without a second
// FatClient call. Run is the full detail (header + tasks +
// mermaid). BlockedByText is the format.RenderBlockedBy output —
// rendered server-side so the template stays oblivious to the
// JSON shape on wire.Run.BlockedBy.
type runPage struct {
	pageData
	ProjectID     int64
	Run           *service.RunDetail
	BlockedByText string
	// TopDown drives the DAG orientation toggle. The page defaults
	// to left-to-right (LR reads better for wide fan-out); the
	// "?dag=td" query param (set by the legend checkbox) flips it
	// to top-down. Threaded back into the template so the poll URL
	// and the checkbox both reflect the current choice across the
	// 20s auto-refresh.
	TopDown bool
}

// handleRunView renders /p/{projectID}/r/{runSeq} — run header,
// mermaid DAG diagram, and per-task list. One FatClient call
// (GetRun returns the embedded run + tasks + pre-rendered
// mermaid string).
func (s *Server) handleRunView(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "runSeq"))
	if err != nil || seq <= 0 {
		http.Error(w, "invalid run seq", http.StatusBadRequest)
		return
	}
	run, err := s.fc.GetRun(r.Context(), pid, seq)
	if err != nil {
		s.logger.Error("GetRun failed", "project_id", pid, "run_seq", seq, "error", err)
		s.writeFetchError(w, "run", err)
		return
	}
	if run == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	var blocked string
	if run.State == "waiting" {
		blocked = format.RenderBlockedBy(run.BlockedBy)
	}
	// Orientation is a backend option (format.SetMermaidDirection,
	// default TD). The run page defaults to LR — wide fan-out
	// reads better in a browser — and the legend checkbox sets
	// ?dag=td to flip back to top-down.
	topDown := r.URL.Query().Get("dag") == "td"
	dir := "LR"
	if topDown {
		dir = "TD"
	}
	run.DiagramMermaid = format.SetMermaidDirection(run.DiagramMermaid, dir)
	s.render(w, r, "run.html", runPage{
		pageData:      s.commonPageData(),
		ProjectID:     pid,
		Run:           run,
		BlockedByText: blocked,
		TopDown:       topDown,
	})
}

// dagViewPage is the data shape for views/dag.html — the
// full-bleed standalone DAG page. Same Run payload as the run
// page, but this view defaults to top-down (TD) and the toggle
// flips to LR (the inverse of the embedded run-page default),
// because a dedicated full-width page has the room for a tall
// TD graph that the narrow run-page column doesn't.
type dagViewPage struct {
	pageData
	ProjectID int64
	Run       *service.RunDetail
	LeftRight bool
}

// handleRunDAGView renders /p/{projectID}/r/{runSeq}/dag — the
// DAG alone on a full-width, chrome-light page so large graphs
// can spread edge-to-edge (the run page's centered 960px column
// boxes them in). Defaults to TD; ?dag=lr flips to LR. Opened in
// a new tab from the run page's DAG section.
func (s *Server) handleRunDAGView(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "runSeq"))
	if err != nil || seq <= 0 {
		http.Error(w, "invalid run seq", http.StatusBadRequest)
		return
	}
	run, err := s.fc.GetRun(r.Context(), pid, seq)
	if err != nil {
		s.logger.Error("GetRun failed", "project_id", pid, "run_seq", seq, "error", err)
		s.writeFetchError(w, "run", err)
		return
	}
	if run == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	leftRight := r.URL.Query().Get("dag") == "lr"
	dir := "TD"
	if leftRight {
		dir = "LR"
	}
	run.DiagramMermaid = format.SetMermaidDirection(run.DiagramMermaid, dir)
	s.render(w, r, "dag.html", dagViewPage{
		pageData:  s.commonPageData(),
		ProjectID: pid,
		Run:       run,
		LeftRight: leftRight,
	})
}

// handleExportRun streams the run's Markdown report as a file
// download (mirror of enju_export_run). ExportRunMarkdown is a
// pure read — two coord GETs and a string build, no disk write
// and no git commit — so a GET that produces it has no side
// effects and needs no Origin/CSRF gate. The browser saves it
// via Content-Disposition rather than rendering in-page; the
// report is plain Markdown, not an HTML view.
func (s *Server) handleExportRun(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	seq, err := strconv.Atoi(chi.URLParam(r, "runSeq"))
	if err != nil || seq <= 0 {
		http.Error(w, "invalid run seq", http.StatusBadRequest)
		return
	}
	md, err := s.fc.ExportRunMarkdown(r.Context(), pid, seq)
	if err != nil {
		s.logger.Error("ExportRunMarkdown failed", "project_id", pid, "run_seq", seq, "error", err)
		http.Error(w, "failed to export run: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="run-%d.md"`, seq))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := w.Write([]byte(md)); err != nil {
		s.logger.Error("export run write failed", "project_id", pid, "run_seq", seq, "error", err)
	}
}
