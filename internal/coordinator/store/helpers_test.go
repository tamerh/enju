package store

// Test fixtures (Creation Methods, per Meszaros, *xUnit Test
// Patterns*) for the kinds of state transitions test setup
// needs.
//
// These are NOT historical scaffolding for deleted methods —
// they're test fixtures that abstract the standard Plan +
// Mutation + ApplyPlan boilerplate so a test can read at the
// level of "claim this task" rather than "construct a Plan
// containing a SetClaim mutation, apply it, check the
// error." Both forms exercise the same chokepoint code path;
// only the test's signal-to-noise ratio changes.
//
// Two principles:
//
//   - The chokepoint discipline ("every state write goes
//     through ApplyPlan") is enforced architecturally by the
//     CoordinatorStore interface (compile-time, external
//     callers) and by the apply-package layout (in-package
//     review, plus the chokepoint lint). Tests do not need
//     to visually re-prove it via 7-line Plan literals at
//     every call site.
//
//   - Each fixture below faithfully wraps ApplyPlan +
//     Mutation; none introduce shadow semantics. If a fixture
//     ever drifts from production behavior, it stops being a
//     Creation Method and becomes a Mystery Guest — delete it.
//
// Singletons (1-2 call sites) get inlined; helpers below are
// all 3+ call sites and earn their place per DAMP > DRY.

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

// helperUpdateReadyTasks runs the readiness cascade via
// ApplyPlan. Returns the readied slice so tests that count
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
// the new project ID.
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

// helperPauseRun pauses a run via ApplyPlan. Returns
// (changed, error) where `changed` is true iff the run's
// state actually transitioned. Reads the prior state to
// compute the changed flag.
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

// helperCreateIssue creates an issue via ApplyPlan, returning
// (id, seq, error). Stamps the IDs back onto the input record
// so callers that retain it see the assigned values.
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
func helperTriageIssue(s *Store, issueID, citizenID int64, severity IssueSeverity) error {
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

// helperSpawnTask runs the SpawnTask mutation via ApplyPlan.
// Cycle exhaustion is converted from ApplyResult.BudgetExhausted
// to a stringy error so existing test assertions can keep
// using error-message matching. Production callers
// (service.SpawnTask) check the BudgetExhausted field directly
// — tests that want the typed flag should call ApplyPlan
// inline instead of using this fixture.
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
func helperCloseIssue(s *Store, issueID, citizenID int64, status IssueStatus, closedByTaskID string) error {
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
// CompleteRun mutation. Returns the resulting RunState.
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

// helperCreateCitizen wraps the CreateCitizen mutation: takes a
// *CitizenRecord plus the bearer token to seed into the tokens
// table (a citizen row carries no token), returns (id, error).
func helperCreateCitizen(s *Store, c *CitizenRecord, token string) (int64, error) {
	res, err := s.ApplyPlan(Plan{
		Version:   testEngineVersion,
		Mutations: []Mutation{CreateCitizen{Citizen: *c, Token: token}},
	})
	if err != nil {
		return 0, err
	}
	return res.CitizenID, nil
}

// helperRevokeTokenByValue revokes a token by string value
// via ApplyPlan.
func helperRevokeTokenByValue(s *Store, token string) error {
	_, err := s.ApplyPlan(Plan{
		Version:  testEngineVersion,
		Mutations: []Mutation{RevokeTokenByValue{Token: token}},
	})
	return err
}

// helperCreateRun creates a run via ApplyPlan, returning
// (id, seq, error).
func helperCreateRun(s *Store, r *RunRecord) (int64, int, error) {
	res, err := s.ApplyPlan(Plan{
		Version:  testEngineVersion,
		Mutations: []Mutation{CreateRun{Run: *r}},
	})
	if err != nil {
		return 0, 0, err
	}
	return res.RunID, res.RunSeq, nil
}
