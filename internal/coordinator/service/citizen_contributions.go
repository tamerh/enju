package service

import (
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// CitizenContributionsResponse is the wire shape returned by
// the public-citizen-page endpoint. Mirrors the legacy REST
// shape exactly — fields read by /:username pages and by the
// fat-client's MyDashboard accumulator.
type CitizenContributionsResponse struct {
	Username      string         `json:"username"`
	TasksCompleted   int          `json:"tasks_completed"`
	TasksRejected    int          `json:"tasks_rejected"`
	TasksTimedOut    int          `json:"tasks_timed_out"`
	TasksReleased    int          `json:"tasks_released"`
	ReviewsGiven    int          `json:"reviews_given"`
	ReviewApproves   int          `json:"review_approves"`
	ReviewRejects    int          `json:"review_rejects"`
	VotesCast      int          `json:"votes_cast"`
	RunsCreated     int          `json:"runs_created"`
	TokensTotal     int64         `json:"tokens_total"`
	ProjectCount     int          `json:"project_count"`
	TotalContributions  int          `json:"total_contributions"`
	ProjectsThisMonth  int          `json:"projects_this_month"`
	DownstreamImpact   DownstreamImpactView `json:"downstream_impact"`
}

// DownstreamImpactView is the inline downstream-impact block.
type DownstreamImpactView struct {
	Tasks  int `json:"tasks"`
	Projects int `json:"projects"`
}

// CitizenContributions returns the contribution summary +
// derived metrics for one citizen. Public read — no membership
// gate; the citizen's username is enough to look them up.
// Returns ErrNotFound when the citizen doesn't exist.
func CitizenContributions(s *store.Store, username string) (*CitizenContributionsResponse, error) {
	citizen, err := s.GetCitizenByUsername(username)
	if err != nil {
		return nil, err
	}
	if citizen == nil {
		return nil, fmt.Errorf("%w: citizen not found", ErrNotFound)
	}
	summary, err := s.GetContributionSummary(citizen.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get contributions: %w", err)
	}
	totalEvents, _ := s.CountContributionEvents(citizen.ID)
	projectsThisMonth, _ := s.CountProjectsThisMonth(citizen.ID)
	downstreamTasks, downstreamProjects, _ := s.GetDownstreamImpact(citizen.ID)

	return &CitizenContributionsResponse{
		Username:      username,
		TasksCompleted:   summary.TasksCompleted,
		TasksRejected:    summary.TasksRejected,
		TasksTimedOut:    summary.TasksTimedOut,
		TasksReleased:    summary.TasksReleased,
		ReviewsGiven:    summary.ReviewsGiven,
		ReviewApproves:   summary.ReviewApproves,
		ReviewRejects:    summary.ReviewRejects,
		VotesCast:      summary.VotesCast,
		RunsCreated:     summary.RunsCreated,
		TokensTotal:     summary.TokensTotal,
		ProjectCount:     summary.ProjectCount,
		TotalContributions:  totalEvents,
		ProjectsThisMonth:  projectsThisMonth,
		DownstreamImpact: DownstreamImpactView{
			Tasks:  downstreamTasks,
			Projects: downstreamProjects,
		},
	}, nil
}
