package engine

import (
	"time"

	"github.com/enju-ai/enju/internal/store"
)

// mockStore is a configurable ReadStore for unit-testing
// engine functions without touching SQLite. Each field
// holds the data that the corresponding ReadStore method
// returns. Unconfigured fields return nil/zero.
type mockStore struct {
	tasks        map[string]*store.TaskRecord
	runs         map[int64]*store.RunRecord
	projects     map[int64]*store.ProjectRecord
	citizens     map[int64]*store.CitizenRecord
	citizensByUN map[string]*store.CitizenRecord
	submissions  map[string][]store.TaskClaimRecord
	claimTimes   map[string]time.Time
	activeClaims map[string]map[int64]bool // taskID → citizenID → true
	claimCounts  map[string]int
	artifacts    map[string]*store.ArtifactRecord // "projectID:path" → record
	tasksByRun   map[int64][]store.TaskRecord
}

func (m *mockStore) GetTask(id string) (*store.TaskRecord, error) {
	if m.tasks == nil {
		return nil, nil
	}
	return m.tasks[id], nil
}

func (m *mockStore) ListTasksByRun(runID int64) ([]store.TaskRecord, error) {
	if m.tasksByRun == nil {
		return nil, nil
	}
	return m.tasksByRun[runID], nil
}

func (m *mockStore) GetRun(id int64) (*store.RunRecord, error) {
	if m.runs == nil {
		return nil, nil
	}
	return m.runs[id], nil
}

func (m *mockStore) GetRunByProjectSeq(projectID int64, seq int) (*store.RunRecord, error) {
	return nil, nil
}

func (m *mockStore) GetProject(id int64) (*store.ProjectRecord, error) {
	if m.projects == nil {
		return nil, nil
	}
	return m.projects[id], nil
}

func (m *mockStore) ListVoteSubmissions(taskID string) ([]store.TaskClaimRecord, error) {
	if m.submissions == nil {
		return nil, nil
	}
	return m.submissions[taskID], nil
}

func (m *mockStore) ListActiveClaims(taskID string) ([]store.TaskClaimRecord, error) {
	return nil, nil
}

func (m *mockStore) EarliestClaimTime(taskID string) (time.Time, error) {
	if m.claimTimes == nil {
		return time.Time{}, nil
	}
	return m.claimTimes[taskID], nil
}

func (m *mockStore) HasActiveClaim(taskID string, citizenID int64) (bool, error) {
	if m.activeClaims == nil {
		return false, nil
	}
	citizens := m.activeClaims[taskID]
	return citizens[citizenID], nil
}

func (m *mockStore) CountActiveClaims(taskID string) (int, error) {
	if m.claimCounts == nil {
		return 0, nil
	}
	return m.claimCounts[taskID], nil
}

func (m *mockStore) GetCitizen(id int64) (*store.CitizenRecord, error) {
	if m.citizens == nil {
		return nil, nil
	}
	return m.citizens[id], nil
}

func (m *mockStore) GetCitizenByUsername(username string) (*store.CitizenRecord, error) {
	if m.citizensByUN == nil {
		return nil, nil
	}
	return m.citizensByUN[username], nil
}

func (m *mockStore) GetArtifact(projectID int64, path string) (*store.ArtifactRecord, error) {
	if m.artifacts == nil {
		return nil, nil
	}
	return m.artifacts[artKey(projectID, path)], nil
}

func (m *mockStore) ListTasksWritingArtifact(projectID int64, path string, acceptedOnly bool) ([]store.TaskRecord, error) {
	return nil, nil
}

func (m *mockStore) ListTasksReadingArtifact(projectID int64, path string, acceptedOnly bool) ([]store.TaskRecord, error) {
	return nil, nil
}

func artKey(projectID int64, path string) string {
	return string(rune(projectID)) + ":" + path
}
