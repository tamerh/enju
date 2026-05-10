package webui

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/go-chi/chi/v5"
)

// taskPage is the data shape consumed by views/task.html.
// Carries the TaskMeta plus a few derived fields the template
// would otherwise need helpers for: the depends_on list parsed
// out of TaskMeta.DependsOn (which is a comma-separated string
// per the wire format), and the parsed vote options when the
// action is vote.
type taskPage struct {
	pageData
	ProjectID   int64
	Task        *service.TaskMeta
	DependsOn   []string
	VoteOptions []taskVoteOption
	// ClaimedByMe is true when state=claimed AND the calling
	// citizen holds the open claim. Drives whether write-action
	// forms appear: only the claimant should see Submit /
	// Release / Run-script / verdict forms. Other viewers (the
	// owner watching a bot work, a teammate observing) see
	// "claimed by @bot-name" but no actionable form.
	ClaimedByMe bool
	// Result is the rendered result.md content, fetched from
	// git at Task.CommitSHA. Empty when no submission yet
	// (task hasn't completed / no commit) or when the file
	// couldn't be read. The template branches on len() so an
	// empty value just hides the section.
	Result string
	// ActionError is set when a write action (claim, release,
	// review, submit, execute) failed and we want to surface the
	// message inline on the task page rather than dumping a
	// bare banner. Same failure-path pattern as the file-issue
	// form on the issues list page.
	ActionError string
	// Iterations is the chronological history of every claim
	// attempt on this task: who claimed, when, what they
	// submitted, what verdict they received. Reverse-
	// chronological (newest first) per the coord ordering.
	// Each row carries Body (the iter's result.md content read
	// from git) so the user can see what got submitted at each
	// iter without separate clicks. Empty slice when the task
	// hasn't been claimed yet.
	Iterations []iterationView
}

// iterationView is the per-iteration shape templates consume.
// Wraps wire.Iteration with the body content fetched from git
// at this iteration's commit SHA, so the template doesn't need
// to know how to read git.
type iterationView struct {
	wire.Iteration
	ShortSHA string // 12-char prefix for display
	Body     string // result.md content at this iter's commit; empty when unreadable
}

// taskVoteOption is the rendered shape of a single vote option.
// We parse VoteOptionsJSON in the handler so the template stays
// declarative.
type taskVoteOption struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Activates []string `json:"activates"`
}

// handleTaskView renders /p/{projectID}/t/{taskID}. Read-only
// view of the task: prompt, action, state, dependencies,
// action-specific schema (review target, vote options,
// compute script). One FatClient call (FetchTaskMeta).
func (s *Server) handleTaskView(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || pid <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	taskID := chi.URLParam(r, "taskID")
	if taskID == "" {
		http.Error(w, "missing task id", http.StatusBadRequest)
		return
	}
	meta, err := s.fc.FetchTaskMeta(r.Context(), taskID)
	if err != nil {
		s.logger.Error("FetchTaskMeta failed", "task_id", taskID, "error", err)
		http.Error(w, "failed to load task: "+err.Error(), http.StatusBadGateway)
		return
	}
	if meta == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	// Best-effort load of the submitted result. Failures here
	// (no clone, file missing) just leave Result empty so the
	// section hides. Hard errors land in the log; the user
	// still gets the page rather than a 5xx.
	result, _, rerr := s.fc.ReadTaskResult(r.Context(), taskID)
	if rerr != nil {
		s.logger.Warn("ReadTaskResult failed; rendering without result body",
			"task_id", taskID, "error", rerr)
	}
	iterations := s.loadIterationViews(r, meta)
	s.render(w, r, "task.html", taskPage{
		pageData:    s.commonPageData(),
		ProjectID:   pid,
		Task:        meta,
		DependsOn:   splitDeps(meta.DependsOn),
		VoteOptions: parseVoteOptions(meta.VoteOptionsJSON, s.logger),
		ClaimedByMe: meta.State == "claimed" && meta.ClaimedBy == s.fc.Username(),
		Result:      result,
		Iterations:  iterations,
	})
}

// loadIterationViews fetches the iteration list and reads each
// iter's body from git. Best-effort throughout — list failure
// returns nil (section hides), per-iter body failure leaves
// that row's Body empty (just the metadata renders). All
// failures log so a stuck section is debuggable.
func (s *Server) loadIterationViews(r *http.Request, meta *service.TaskMeta) []iterationView {
	if meta == nil {
		return nil
	}
	iters, err := s.fc.ListTaskIterations(r.Context(), meta.ID)
	if err != nil {
		s.logger.Warn("ListTaskIterations failed; rendering without history",
			"task_id", meta.ID, "error", err)
		return nil
	}
	out := make([]iterationView, 0, len(iters))
	for _, it := range iters {
		v := iterationView{Iteration: it}
		if len(it.CommitSHA) >= 12 {
			v.ShortSHA = it.CommitSHA[:12]
		} else {
			v.ShortSHA = it.CommitSHA
		}
		if it.CommitSHA != "" && meta.ResultDir != "" {
			body, _, berr := s.fc.ReadResultAtCommit(
				r.Context(), meta.ProjectID, it.CommitSHA, meta.ResultDir)
			if berr != nil {
				s.logger.Warn("ReadResultAtCommit failed for iter; body omitted",
					"task_id", meta.ID, "iter_seq", it.Seq, "error", berr)
			} else {
				v.Body = body
			}
		}
		out = append(out, v)
	}
	return out
}

// splitDeps turns the wire-format "a,b,c" depends_on string
// into a slice. Empty input returns nil so templates can
// `{{if .DependsOn}}` cleanly.
func splitDeps(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseVoteOptions decodes the JSON-encoded options list. On
// any error returns nil and logs a warning — the page still
// renders, just without the options block. (A vote task
// without parseable options is a coordinator-side bug; logging
// surfaces it without breaking the UI.)
func parseVoteOptions(raw string, logger *slog.Logger) []taskVoteOption {
	if raw == "" {
		return nil
	}
	var out []taskVoteOption
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		if logger != nil {
			logger.Warn("vote options json decode failed", "error", err, "raw", raw)
		}
		return nil
	}
	return out
}
