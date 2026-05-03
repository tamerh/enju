package service

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// DashboardCitizen is the minimal citizen shape format.Dashboard
// reads. Field tags are load-bearing.
type DashboardCitizen struct {
	Username       string `json:"username"`
	Name           string `json:"name,omitempty"`
	Role           string `json:"role,omitempty"`
	Kind           string `json:"kind,omitempty"`
	TasksCompleted int    `json:"tasks_completed"`
	TasksTimedOut  int    `json:"tasks_timed_out"`
	RegisteredAt   string `json:"registered_at,omitempty"`
}

// DashboardTask is the minimal task shape format.Dashboard
// reads from active_tasks / recent_tasks items: id, seq,
// run_id.
type DashboardTask struct {
	ID    string `json:"id"`
	Seq   int    `json:"seq"`
	RunID int64  `json:"run_id"`
}

// DashboardResponse bundles the dashboard payload. Fields keyed
// to match what format.Dashboard expects: "citizen",
// "active_tasks", "recent_tasks".
type DashboardResponse struct {
	Citizen     DashboardCitizen `json:"citizen"`
	ActiveTasks []DashboardTask  `json:"active_tasks"`
	RecentTasks []DashboardTask  `json:"recent_tasks"`
}

// GetMyDashboard returns the calling citizen's stats, active
// claims, and 5 most-recent completions. Re-fetches the
// citizen row so counters reflect the latest state (the
// auth-context snapshot can be stale).
func GetMyDashboard(s *store.Store, caller *store.CitizenRecord) (*DashboardResponse, error) {
	citizen, err := s.GetCitizen(caller.ID)
	if err != nil {
		return nil, err
	}
	if citizen == nil {
		return nil, ErrNotFound
	}
	active, _ := s.ListCitizenActiveTasks(citizen.ID)
	recent, _ := s.ListCitizenCompletedTasks(citizen.ID, 5)

	kind := citizen.Kind
	if kind == "" {
		kind = "human"
	}
	resp := &DashboardResponse{
		Citizen: DashboardCitizen{
			Username:       citizen.Username,
			Name:           citizen.Name,
			Role:           citizen.Role,
			Kind:           kind,
			TasksCompleted: citizen.TasksCompleted,
			TasksTimedOut:  citizen.TasksTimedOut,
			RegisteredAt:   citizen.RegisteredAt.Format(time.RFC3339),
		},
		ActiveTasks: toDashboardTasks(active),
		RecentTasks: toDashboardTasks(recent),
	}
	return resp, nil
}

func toDashboardTasks(tasks []store.TaskRecord) []DashboardTask {
	out := make([]DashboardTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, DashboardTask{
			ID:    t.ID,
			Seq:   t.Seq,
			RunID: t.RunID,
		})
	}
	return out
}
