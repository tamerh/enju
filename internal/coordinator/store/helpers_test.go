package store

// Test helpers that wrap ApplyPlan for the kinds of state
// transitions that test setup needs. These exist so the legacy
// direct-write methods (Store.CreateTask, Store.ClaimTask, etc.)
// can be deleted while keeping test sites compact.
//
// Production code goes through ApplyPlan; tests do too. The
// helpers just spare each test site from rebuilding the same
// 6-line Plan boilerplate.

import (
	"fmt"
	"testing"
	"time"
)

// testEngineVersion is the package-local alias used by the
// helpers below. Same value as the exported store.TestEngineVersion
// (defined in test_engine_version.go so external test packages can
// compare it against engine.EngineVersion).
const testEngineVersion = TestEngineVersion

// helperCreateTask creates a task via ApplyPlan. Returns an
// error rather than t.Fatal so tests can validate failures
// (e.g. duplicate task IDs).
func helperCreateTask(s *Store, t *TaskRecord) error {
	plan := Plan{
		Version:  testEngineVersion,
		Mutations: []Mutation{CreateTask{Task: *t}},
	}
	_, err := s.ApplyPlan(plan)
	return err
}

// helperClaimTask claims a task for a citizen via ApplyPlan.
// Returns the error so tests can assert on claim refusals
// (already claimed, wrong state, etc.).
func helperClaimTask(s *Store, taskID string, citizenID int64, deadline time.Time) error {
	plan := Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			SetClaim{TaskID: taskID, CitizenID: citizenID, Deadline: deadline},
		},
	}
	_, err := s.ApplyPlan(plan)
	return err
}

// helperReleaseTask releases a citizen's claim via ApplyPlan.
// Matches the legacy Store.ReleaseTask shape: task → READY,
// claim row marked outcome=released.
func helperReleaseTask(s *Store, taskID string, citizenID int64) error {
	plan := Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			ReleaseClaim{TaskID: taskID, CitizenID: citizenID},
		},
	}
	_, err := s.ApplyPlan(plan)
	return err
}

// helperExpireClaimedTask expires a claimed task via ApplyPlan.
// Matches the legacy Store.ExpireClaimedTask shape: task →
// READY, claim row marked outcome=timed_out, citizen score
// penalty applied.
func helperExpireClaimedTask(s *Store, taskID string, citizenID int64) error {
	plan := Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			ExpireClaim{TaskID: taskID, CitizenID: citizenID},
		},
	}
	_, err := s.ApplyPlan(plan)
	return err
}

// helperUpdateReadyTasks runs the readiness cascade via
// ApplyPlan. Returns the readied slice (matching the legacy
// Store.UpdateReadyTasks return shape) so tests that count
// or inspect newly-ready tasks keep working.
func helperUpdateReadyTasks(t *testing.T, s *Store, runID int64) []ReadiedTask {
	t.Helper()
	res, err := s.ApplyPlan(Plan{
		Version:   testEngineVersion,
		Mutations: []Mutation{UpdateReadyTasks{RunID: runID}},
	})
	if err != nil {
		t.Fatalf("helperUpdateReadyTasks: %v", err)
	}
	return res.ReadiedTasks
}

// helperCreateProject creates a project via ApplyPlan, returning
// the new project ID. Replaces the legacy Store.CreateProject
// for test setup.
func helperCreateProject(s *Store, p *ProjectRecord) (int64, error) {
	res, err := s.ApplyPlan(Plan{
		Version:   testEngineVersion,
		Mutations: []Mutation{CreateProject{Project: *p}},
	})
	if err != nil {
		return 0, err
	}
	return res.ProjectID, nil
}

// helperAddProjectMember adds a project member via ApplyPlan.
func helperAddProjectMember(s *Store, projectID, citizenID int64, role ProjectRole, addedBy int64) error {
	_, err := s.ApplyPlan(Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			AddProjectMember{
				ProjectID: projectID,
				CitizenID: citizenID,
				Role:      role,
				AddedBy:   addedBy,
			},
		},
	})
	return err
}

// helperRemoveProjectMember removes a project member via ApplyPlan.
func helperRemoveProjectMember(s *Store, projectID, citizenID int64) error {
	_, err := s.ApplyPlan(Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			RemoveProjectMember{ProjectID: projectID, CitizenID: citizenID},
		},
	})
	return err
}

// helperPauseRun pauses a run via ApplyPlan, matching the
// legacy Store.PauseRun shape: returns (changed, error). Reads
// the prior state to compute the changed flag.
func helperPauseRun(s *Store, runID, citizenID int64) (bool, error) {
	r, err := s.GetRun(runID)
	if err != nil {
		return false, err
	}
	priorState := ""
	if r != nil {
		priorState = string(r.State)
	}
	if _, err := s.ApplyPlan(Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			PauseRun{RunID: runID, CitizenID: citizenID},
		},
	}); err != nil {
		return false, err
	}
	r2, _ := s.GetRun(runID)
	if r2 == nil {
		return false, nil
	}
	return priorState != string(r2.State), nil
}

// helperResumeRun resumes a paused run + re-evaluates state via
// ApplyPlan. Returns the resulting RunState.
func helperResumeRun(s *Store, runID, citizenID int64) (RunState, error) {
	if _, err := s.ApplyPlan(Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			ResumeRun{RunID: runID, CitizenID: citizenID},
			CompleteRun{RunID: runID},
		},
	}); err != nil {
		return "", err
	}
	r, err := s.GetRun(runID)
	if err != nil || r == nil {
		return "", err
	}
	return r.State, nil
}

// helperCreateIssue creates an issue via ApplyPlan, matching
// the legacy Store.CreateIssue shape: returns (id, seq, error).
func helperCreateIssue(s *Store, rec *IssueRecord) (int64, int, error) {
	res, err := s.ApplyPlan(Plan{
		Version:   testEngineVersion,
		Mutations: []Mutation{CreateIssue{Issue: *rec}},
	})
	if err != nil {
		return 0, 0, err
	}
	rec.ID = res.IssueID
	rec.Seq = res.IssueSeq
	return res.IssueID, res.IssueSeq, nil
}

// helperTriageIssue runs the TriageIssue mutation via ApplyPlan.
func helperTriageIssue(s *Store, issueID, citizenID int64, severity string) error {
	_, err := s.ApplyPlan(Plan{
		Version:   testEngineVersion,
		Mutations: []Mutation{TriageIssue{IssueID: issueID, CitizenID: citizenID, Severity: severity}},
	})
	return err
}

// helperMarkIssueInProgress runs the MarkIssueInProgress mutation via ApplyPlan.
func helperMarkIssueInProgress(s *Store, issueID, citizenID int64, fixTaskID string) error {
	_, err := s.ApplyPlan(Plan{
		Version:   testEngineVersion,
		Mutations: []Mutation{MarkIssueInProgress{IssueID: issueID, CitizenID: citizenID, FixTaskID: fixTaskID}},
	})
	return err
}

// helperSpawnTask runs the SpawnTask mutation via ApplyPlan,
// matching the legacy Store.SpawnTask shape: returns
// (taskID, error). Cycle exhaustion is converted from
// ApplyResult.BudgetExhausted to a stringy error to keep
// existing test assertions working — production callers
// (service.SpawnTask) check the field directly.
func helperSpawnTask(s *Store, spec SpawnSpec) (string, error) {
	res, err := s.ApplyPlan(Plan{
		Version:   testEngineVersion,
		Mutations: []Mutation{SpawnTask{Spec: spec}},
	})
	if err != nil {
		return "", err
	}
	if res.BudgetExhausted {
		return "", fmt.Errorf("cycle budget exhausted")
	}
	return res.SpawnedTaskID, nil
}

// helperSetCycleBudgetMax runs the SetCycleBudgetMax mutation via ApplyPlan.
func helperSetCycleBudgetMax(s *Store, runID, citizenID int64, newMax int) error {
	_, err := s.ApplyPlan(Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			SetCycleBudgetMax{RunID: runID, CitizenID: citizenID, NewMax: newMax},
		},
	})
	return err
}

// helperCloseIssue runs the CloseIssue mutation via ApplyPlan.
func helperCloseIssue(s *Store, issueID, citizenID int64, status, closedByTaskID string) error {
	_, err := s.ApplyPlan(Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{CloseIssue{
			IssueID:        issueID,
			CitizenID:      citizenID,
			Status:         status,
			ClosedByTaskID: closedByTaskID,
		}},
	})
	return err
}

// helperEvaluateRunState re-evaluates run state via the
// CompleteRun mutation (which now subsumes the legacy
// EvaluateRunState path). Returns the resulting RunState.
func helperEvaluateRunState(s *Store, runID int64) (RunState, error) {
	if _, err := s.ApplyPlan(Plan{
		Version: testEngineVersion,
		Mutations: []Mutation{
			CompleteRun{RunID: runID},
		},
	}); err != nil {
		return "", err
	}
	r, err := s.GetRun(runID)
	if err != nil || r == nil {
		return "", err
	}
	return r.State, nil
}
