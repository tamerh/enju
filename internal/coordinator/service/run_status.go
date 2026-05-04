package service

import (
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// RunStatusRun is the run-shape format.RunStatus and
// format.RunStatusMermaid consume. Carries run metadata + the
// (non-standard) "_project_name" key used as a render-time
// header decoration. Distinct from RunResponse so the project
// name lookup stays opt-in.
type RunStatusRun struct {
	ID              int64  `json:"id"`
	ProjectID       int64  `json:"project_id"`
	ProjectName     string `json:"_project_name,omitempty"`
	Seq             int    `json:"seq"`
	Name            string `json:"name"`
	State           string `json:"state"`
	Branch          string `json:"branch,omitempty"`
	Slug            string `json:"slug,omitempty"`
	CreatedAt       string `json:"created_at"`
	SourcePath      string `json:"source_path,omitempty"`
	SourceCommitSHA string `json:"source_commit_sha,omitempty"`
	TaskCount       int    `json:"task_count"`
}

// RunStatusTask is the task-shape format.RunStatus and
// format.RunStatusMermaid consume. Minimal fields the
// formatters actually read.
type RunStatusTask struct {
	ID          string `json:"id"`
	TaskDefID   string `json:"task_def_id"`
	InstanceKey string `json:"instance_key,omitempty"`
	State       string `json:"state"`
	ClaimedBy   string `json:"claimed_by,omitempty"`
	DependsOn   string `json:"depends_on,omitempty"`
	FailReason  string `json:"fail_reason,omitempty"`
	SkipReason  string `json:"skip_reason,omitempty"`
}

// RunStatus bundles the data needed to render enju_run_status.
// Wire shape isn't standardized — both run + tasks are passed
// to the formatters separately, this struct is a transport-
// agnostic carrier.
type RunStatus struct {
	Run   RunStatusRun
	Tasks []RunStatusTask
}

// GetRunStatus returns the run + task projections needed for
// the run_status render. Membership-gated through the run's
// parent project.
func GetRunStatus(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int) (*RunStatus, error) {
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
	tasks, err := s.ListTasksByRun(run.ID)
	if err != nil {
		return nil, err
	}
	var projectName string
	if proj, _ := s.GetProject(run.ProjectID); proj != nil {
		projectName = proj.Name
	}
	out := &RunStatus{
		Run: RunStatusRun{
			ID:              run.ID,
			ProjectID:       run.ProjectID,
			ProjectName:     projectName,
			Seq:             run.Seq,
			Name:            run.Name,
			State:           string(run.State),
			Branch:          run.Branch,
			Slug:            run.Slug,
			CreatedAt:       run.CreatedAt.Format(time.RFC3339),
			SourcePath:      run.SourcePath,
			SourceCommitSHA: run.SourceCommitSHA,
			TaskCount:       len(tasks),
		},
		Tasks: make([]RunStatusTask, 0, len(tasks)),
	}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, RunStatusTask{
			ID:          t.ID,
			TaskDefID:   t.TaskDefID,
			InstanceKey: t.InstanceKey,
			State:       string(t.State),
			ClaimedBy:   CitizenUsername(s, t.ClaimedBy),
			DependsOn:   strings.TrimSpace(t.DependsOn),
			FailReason:  t.FailReason,
			SkipReason:  t.SkipReason,
		})
	}
	return out, nil
}
