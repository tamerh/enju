package service

import "github.com/enju-ai/enju/internal/coordinator/store"

// ProfileResponse is the basic citizen identity shape
// format.Profile reads from its first arg.
type ProfileResponse struct {
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role,omitempty"`
}

// DownstreamImpact is the impact summary nested under
// ProfileContributions.
type DownstreamImpact struct {
	Tasks    int `json:"tasks"`
	Projects int `json:"projects"`
}

// ProfileContributions is the contribution-summary shape
// format.Profile reads from its second arg.
type ProfileContributions struct {
	TasksCompleted int              `json:"tasks_completed"`
	TasksRejected  int              `json:"tasks_rejected"`
	TasksReleased  int              `json:"tasks_released"`
	ReviewsGiven   int              `json:"reviews_given"`
	ReviewApproves int              `json:"review_approves"`
	ReviewRejects  int              `json:"review_rejects"`
	VotesCast      int              `json:"votes_cast"`
	TokensTotal    int64            `json:"tokens_total"`
	ProjectCount   int              `json:"project_count"`
	Downstream     DownstreamImpact `json:"downstream_impact"`
}

// GetMyProfile returns the calling citizen's identity +
// contribution summary. Contributions are best-effort —
// returns nil for the contributions slot when GetContributionSummary
// fails, matching the fat-client's graceful-degrade behaviour.
//
// No model-name decoration on the coord side; the fat-client
// injects its local c.modelName separately. A future MCP-over-
// HTTP transport could surface this via a per-request header.
func GetMyProfile(s *store.Store, caller *store.CitizenRecord) (*ProfileResponse, *ProfileContributions, error) {
	citizen, err := s.GetCitizen(caller.ID)
	if err != nil {
		return nil, nil, err
	}
	if citizen == nil {
		return nil, nil, ErrNotFound
	}
	profile := &ProfileResponse{
		Username: citizen.Username,
		Name:     citizen.Name,
		Email:    citizen.Email,
		Role:     citizen.Role,
	}
	summary, err := s.GetContributionSummary(citizen.ID)
	if err != nil || summary == nil {
		return profile, nil, nil
	}
	downstreamTasks, downstreamProjects, _ := s.GetDownstreamImpact(citizen.ID)
	contrib := &ProfileContributions{
		TasksCompleted: summary.TasksCompleted,
		TasksRejected:  summary.TasksRejected,
		TasksReleased:  summary.TasksReleased,
		ReviewsGiven:   summary.ReviewsGiven,
		ReviewApproves: summary.ReviewApproves,
		ReviewRejects:  summary.ReviewRejects,
		VotesCast:      summary.VotesCast,
		TokensTotal:    summary.TokensTotal,
		ProjectCount:   summary.ProjectCount,
		Downstream: DownstreamImpact{
			Tasks:    downstreamTasks,
			Projects: downstreamProjects,
		},
	}
	return profile, contrib, nil
}
