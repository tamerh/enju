package service

// "Me" surface — read + write for the calling citizen's own
// dashboard / contributions / profile. Wraps the existing
// /citizens/by-username/{username}/{dashboard,contributions,profile}
// coord endpoints.
//
// Wire shapes mirror coord-side service types field-for-field
// so the JSON round-trip is direct. Defining them here keeps
// webui's import boundary at fatclient/service.

import (
	"context"
	"encoding/json"
	"fmt"
)

// DashboardCitizen mirrors coord/service.DashboardCitizen.
type DashboardCitizen struct {
	Username       string `json:"username"`
	Name           string `json:"name,omitempty"`
	Role           string `json:"role,omitempty"`
	Kind           string `json:"kind,omitempty"`
	TasksCompleted int    `json:"tasks_completed"`
	TasksTimedOut  int    `json:"tasks_timed_out"`
	RegisteredAt   string `json:"registered_at,omitempty"`
}

// DashboardTask mirrors coord/service.DashboardTask.
type DashboardTask struct {
	ID    string `json:"id"`
	Seq   int    `json:"seq"`
	RunID int64  `json:"run_id"`
}

// DashboardResponse mirrors coord/service.DashboardResponse.
type DashboardResponse struct {
	Citizen     DashboardCitizen `json:"citizen"`
	ActiveTasks []DashboardTask  `json:"active_tasks"`
	RecentTasks []DashboardTask  `json:"recent_tasks"`
}

// ContributionsResponse mirrors
// coord/service.CitizenContributionsResponse — the per-citizen
// stats summary backing the profile page.
type ContributionsResponse struct {
	Username           string               `json:"username"`
	TasksCompleted     int                  `json:"tasks_completed"`
	TasksRejected      int                  `json:"tasks_rejected"`
	TasksTimedOut      int                  `json:"tasks_timed_out"`
	TasksReleased      int                  `json:"tasks_released"`
	ReviewsGiven       int                  `json:"reviews_given"`
	ReviewApproves     int                  `json:"review_approves"`
	ReviewRejects      int                  `json:"review_rejects"`
	VotesCast          int                  `json:"votes_cast"`
	RunsCreated        int                  `json:"runs_created"`
	TokensTotal        int64                `json:"tokens_total"`
	ProjectCount       int                  `json:"project_count"`
	TotalContributions int                  `json:"total_contributions"`
	ProjectsThisMonth  int                  `json:"projects_this_month"`
	DownstreamImpact   DownstreamImpactView `json:"downstream_impact"`
}

// DownstreamImpactView mirrors coord/service.DownstreamImpactView.
type DownstreamImpactView struct {
	Tasks    int `json:"tasks"`
	Projects int `json:"projects"`
}

// CitizenResponse is the lean shape returned by the profile
// PUT endpoint and the GET-by-username read. Field set is the
// public-citizen view (no token, no internal id).
type CitizenResponse struct {
	Username      string `json:"username"`
	Name          string `json:"name,omitempty"`
	Email         string `json:"email,omitempty"`
	Role          string `json:"role,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Score         int    `json:"score,omitempty"`
	TasksDone     int    `json:"tasks_completed,omitempty"`
	TokensContrib int    `json:"tokens_contributed,omitempty"`
	RegisteredAt  string `json:"registered_at,omitempty"`
}

// UpdateProfileParams is the input for UpdateProfile.
//
// Both fields are optional; nil-pointer / empty means "leave as
// is." The coord-side handler accepts JSON {name?, email?} and
// only updates fields that were sent. We mirror that here with
// pointers so empty-string can be distinguished from
// not-supplied if a caller wants to clear a field.
type UpdateProfileParams struct {
	Name  *string `json:"name,omitempty"`
	Email *string `json:"email,omitempty"`
}

// GetDashboard returns the calling citizen's dashboard
// (active claims + recent completions + summary counters).
// Targets the calling citizen via the persisted Username on
// the coord client.
func (s *FatClient) GetDashboard(ctx context.Context) (*DashboardResponse, error) {
	username := s.coord.Username()
	if username == "" {
		return nil, fmt.Errorf("not authenticated")
	}
	data, err := s.coord.Get(ctx, "/api/v1/citizens/by-username/"+username+"/dashboard")
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out DashboardResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode dashboard: %w", err)
	}
	return &out, nil
}

// GetContributions returns the per-citizen contribution
// summary. Username is the lookup key — the contributions page
// is public (no membership gate), so this also works for
// looking up other citizens' totals if a UI surface needs it.
func (s *FatClient) GetContributions(ctx context.Context, username string) (*ContributionsResponse, error) {
	if username == "" {
		return nil, fmt.Errorf("username is required")
	}
	data, err := s.coord.Get(ctx, "/api/v1/citizens/by-username/"+username+"/contributions")
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out ContributionsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode contributions: %w", err)
	}
	return &out, nil
}

// UpdateProfile updates the calling citizen's display name +
// email. Returns the full updated citizen record so the caller
// can re-render with the latest values without a separate GET.
//
// The coord enforces that a citizen can only update their own
// profile; the auth token in the coord client carries identity.
func (s *FatClient) UpdateProfile(ctx context.Context, params UpdateProfileParams) (*CitizenResponse, error) {
	username := s.coord.Username()
	if username == "" {
		return nil, fmt.Errorf("not authenticated")
	}
	body := map[string]interface{}{}
	if params.Name != nil {
		body["name"] = *params.Name
	}
	if params.Email != nil {
		body["email"] = *params.Email
	}
	data, err := s.coord.Put(ctx,
		"/api/v1/citizens/by-username/"+username+"/profile", body)
	if err != nil {
		return nil, err
	}
	if msg := errorMsg(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out CitizenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode profile: %w", err)
	}
	return &out, nil
}
