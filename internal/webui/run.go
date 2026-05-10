package webui

import (
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
		http.Error(w, "failed to load run: "+err.Error(), http.StatusBadGateway)
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
	s.render(w, r, "run.html", runPage{
		pageData:      s.commonPageData(),
		ProjectID:     pid,
		Run:           run,
		BlockedByText: blocked,
	})
}
