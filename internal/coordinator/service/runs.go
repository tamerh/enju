package service

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// RunResponse is the wire shape for one run. Used by REST,
// MCP, and future Web UI. Field names + JSON tags are
// load-bearing — format.RunList and format.RunStatus consume
// these key names.
type RunResponse struct {
	ID              int64    `json:"id"`
	ProjectID       int64    `json:"project_id,omitempty"`
	Seq             int      `json:"seq"`
	Name            string   `json:"name"`
	State           string   `json:"state"`
	TaskCount       int      `json:"task_count"`
	Branch          string   `json:"branch,omitempty"`
	Slug            string   `json:"slug,omitempty"`
	CreatedAt       string   `json:"created_at"`
	SourcePath      string   `json:"source_path,omitempty"`
	SourceCommitSHA string   `json:"source_commit_sha,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// ToRunResponse builds the wire shape from a store record + a
// pre-computed task count. Exported so other service functions
// (single-run reads, run_status) can produce the same shape.
func ToRunResponse(r store.RunRecord, taskCount int) RunResponse {
	return RunResponse{
		ID:              r.ID,
		ProjectID:       r.ProjectID,
		Seq:             r.Seq,
		Name:            r.Name,
		State:           string(r.State),
		TaskCount:       taskCount,
		Branch:          r.Branch,
		Slug:            r.Slug,
		CreatedAt:       r.CreatedAt.Format(time.RFC3339),
		SourcePath:      r.SourcePath,
		SourceCommitSHA: r.SourceCommitSHA,
	}
}

// ListRuns returns every run the caller can see across all
// projects: legacy zero-member projects plus member-gated
// projects the caller belongs to. Builds the membership
// allow-set once for the loop.
func ListRuns(s store.CoordinatorStore, caller *store.CitizenRecord) ([]RunResponse, error) {
	runs, err := s.ListRuns()
	if err != nil {
		return nil, err
	}
	allowed := map[int64]bool{}
	memberProjects, _ := s.ListProjectsForCitizen(caller.ID)
	for _, p := range memberProjects {
		allowed[p.ID] = true
	}
	out := make([]RunResponse, 0, len(runs))
	for _, r := range runs {
		total, _ := s.CountProjectMembers(r.ProjectID)
		if total > 0 && !allowed[r.ProjectID] {
			continue
		}
		tasks, _ := s.ListTasksByRun(r.ID)
		out = append(out, ToRunResponse(r, len(tasks)))
	}
	return out, nil
}

// ListRunsByProject returns every run in one project. Returns
// ErrNotMember if the caller isn't on the membership list (and
// the project isn't a legacy zero-member open project).
func ListRunsByProject(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64) ([]RunResponse, error) {
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, ErrNotMember
	}
	runs, err := s.ListRunsByProject(projectID)
	if err != nil {
		return nil, err
	}
	out := make([]RunResponse, 0, len(runs))
	for _, r := range runs {
		tasks, _ := s.ListTasksByRun(r.ID)
		out = append(out, ToRunResponse(r, len(tasks)))
	}
	return out, nil
}

// GetRun returns one run by its (project, seq) pair. Returns
// ErrNotFound when the run doesn't exist; ErrNotMember when
// the caller can't read the parent project.
func GetRun(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int) (*RunResponse, error) {
	run, err := s.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	if !CanReadProject(s, run.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	tasks, _ := s.ListTasksByRun(run.ID)
	resp := ToRunResponse(*run, len(tasks))
	return &resp, nil
}

// CanReadProject is the read-side membership gate: legacy
// zero-member projects are open, member-gated projects require
// the caller on the list. Exported because both service
// functions and (via mcphandlers callerCanReadProject) the
// older path reuse the same rule.
func CanReadProject(s store.CoordinatorStore, projectID, citizenID int64) bool {
	total, err := s.CountProjectMembers(projectID)
	if err != nil {
		return false
	}
	if total == 0 {
		return true
	}
	members, err := s.ListProjectMembers(projectID)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.CitizenID == citizenID {
			return true
		}
	}
	return false
}
