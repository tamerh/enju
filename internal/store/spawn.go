package store

import (
	"fmt"
	"strings"
	"time"
)

// SpawnSpec describes a new task to add to an in-flight run.
// Living-workflow phase 4a — manual spawn primitive. The
// caller owns naming (TaskDefID) and content (Action / Prompt
// / etc.); the engine owns the lineage bookkeeping
// (cycle budget, parent linkage, event emission).
type SpawnSpec struct {
	RunID          int64    // run to spawn into
	ParentTaskID   string   // optional — empty for top-level human/operator spawns
	TaskDefID      string   // unique within the run; the new task's id is "<projectID>:<runSeq>:<TaskDefID>"
	Action         string   // "answer" | "compute" | "contribute" | "review" | "vote"
	Prompt         string   // optional
	UserPrompt     string   // optional
	// Citizens is the multi-citizen count for the spawned
	// task. Defaults to 1.
	//
	// Caveat (foundational v1 / phase 6b): if the run already
	// has any topic-stamped claims and a spawn lands a task
	// with Citizens > 1, the run-level multi-citizen gate in
	// generateIterationBranch only takes effect for FUTURE
	// claims — earlier topics keep their refs and FF-merge as
	// expected, but the new multi-citizen task commits
	// directly to the run branch and can advance main between
	// an earlier topic's fork and its own FF-merge, producing
	// a non-FF refusal at merge time. In practice, today's
	// spawn paths (manual, on_review_reject remediation, auto-
	// triage) all default to Citizens=1 so the failure mode
	// is vacuous; v2 (rebase-on-non-FF + multi-citizen topic
	// flow) lifts this. See docs/living-workflow.md § v2
	// follow-ups.
	Citizens       int
	DependsOn      []string // optional — fully-qualified task ids the spawned task must wait on
	AssignTo       []string // optional — restrict claim to specific usernames
	RequireRole    string   // optional
	ResultType     string   // "text" | "json"; defaults to "text"
	Trigger        string   // "human" | "bot" | "template_rule" | "auto_triage"
	SpawnedBy      int64    // citizen ID for attribution; 0 for system
	ClosesIssueSeq int      // > 0 when this is an auto-triage fix task; on accept, the named issue auto-closes (phase 4c)
}

// SpawnTask creates a new task in an existing run, enforcing the
// per-run cycle budget. Returns the new fully-qualified task id.
//
// Atomicity: budget check, INSERT, counter increment, and
// task_spawned event all run in one transaction. Budget exhaustion
// is also transactional — pause + cycle_budget_exhausted event
// land together so the run state and event log can't drift.
//
// Refuses on terminal/paused runs. The cycle_budget_exhausted
// path returns an error AND pauses the run; the caller can resume
// via ResumeRun after the operator extends the budget (deferred
// in phase 4a; for now the run sits paused until manually
// resumed).
func (s *Store) SpawnTask(spec SpawnSpec) (string, error) {
	if spec.RunID == 0 {
		return "", fmt.Errorf("run_id is required")
	}
	if spec.TaskDefID == "" {
		return "", fmt.Errorf("task_def_id is required")
	}
	if spec.Action == "" {
		return "", fmt.Errorf("action is required")
	}
	if spec.Trigger == "" {
		spec.Trigger = "human"
	}
	if spec.Citizens <= 0 {
		spec.Citizens = 1
	}
	if spec.ResultType == "" {
		spec.ResultType = "text"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// 1. Load run state + budget. Lock the run row by selecting
	//    inside the tx so concurrent spawns serialize through
	//    SQLite's WAL writer (busy_timeout handles contention).
	var (
		runState   string
		projectID  int64
		runSeq     int
		runSlug    string
		budgetUsed int
		budgetMax  int
	)
	err = tx.QueryRow(
		`SELECT state, project_id, seq, slug, cycle_budget_used, cycle_budget_max
		 FROM runs WHERE id = ?`,
		spec.RunID,
	).Scan(&runState, &projectID, &runSeq, &runSlug, &budgetUsed, &budgetMax)
	if err != nil {
		return "", fmt.Errorf("loading run: %w", err)
	}

	// 2. Refuse on terminal/paused runs. Paused runs need an
	//    explicit resume before they accept new spawns —
	//    operators pause precisely to stop the runaway, so
	//    auto-resuming on spawn would defeat the purpose.
	switch RunState(runState) {
	case RunCompleted, RunFailed:
		return "", fmt.Errorf("run %d is %s — cannot spawn into a terminal run", spec.RunID, runState)
	case RunPaused:
		return "", fmt.Errorf("run %d is paused — resume it first with enju_resume_run", spec.RunID)
	}

	// 3. Cycle budget check. If exhausted, pause the run +
	//    emit cycle_budget_exhausted event in this same tx so
	//    the audit log is consistent with the state flip.
	if budgetUsed >= budgetMax {
		now := time.Now()
		if _, err := tx.Exec(
			`UPDATE runs SET state = 'paused', updated_at = ? WHERE id = ? AND state IN ('active', 'idle')`,
			now, spec.RunID,
		); err != nil {
			return "", fmt.Errorf("auto-pausing run on budget exhaustion: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
			 VALUES (0, 'cycle_budget_exhausted', '', '', ?, ?, ?, ?)`,
			spec.RunID, projectID,
			fmt.Sprintf(`{"used":%d,"max":%d,"attempted_task":%q,"attempted_by":%d}`,
				budgetUsed, budgetMax, spec.TaskDefID, spec.SpawnedBy),
			now,
		); err != nil {
			return "", fmt.Errorf("recording cycle_budget_exhausted: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("cycle budget exhausted for run %d (%d/%d) — run paused; extend budget and resume to allow further spawns", spec.RunID, budgetUsed, budgetMax)
	}

	// 4. Compute next per-run task seq + the fully-qualified id.
	var maxSeq int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM tasks WHERE run_id = ?`, spec.RunID).Scan(&maxSeq); err != nil {
		return "", err
	}
	nextSeq := maxSeq + 1
	taskID := fmt.Sprintf("%d:%d:%s", projectID, runSeq, spec.TaskDefID)

	// 5. INSERT the task row. Spawned tasks start ready unless
	//    they declare upstream deps; UpdateReadyTasks would
	//    promote them on the next sweep but starting them ready
	//    when there are no deps avoids one wakeup-cycle of
	//    latency. depends_on is a comma-separated list (existing
	//    convention).
	state := TaskReady
	if len(spec.DependsOn) > 0 {
		state = TaskPending
	}
	dependsOn := strings.Join(spec.DependsOn, ",")
	assignTo := ""
	if len(spec.AssignTo) > 0 {
		// Assignment is stored as a JSON-encoded string array per
		// existing convention. Manual JSON to avoid pulling
		// encoding/json just for this one path; the shape is
		// trivial.
		quoted := make([]string, len(spec.AssignTo))
		for i, u := range spec.AssignTo {
			quoted[i] = fmt.Sprintf("%q", u)
		}
		assignTo = "[" + strings.Join(quoted, ",") + "]"
	}

	now := time.Now()
	if _, err := tx.Exec(
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref,
		                    action, prompt, user_prompt, script, outputs, requirements, result_type,
		                    timeout, state, depends_on, reads_artifacts, writes_artifacts,
		                    assign_to, require_role, citizens, run_slug,
		                    spawned_from, spawn_trigger, closes_issue_seq, created_at)
		 VALUES (?, ?, ?, ?, '', '', '',
		         ?, ?, ?, '', '', '', ?,
		         '', ?, ?, '[]', '[]',
		         ?, ?, ?, ?,
		         ?, ?, ?, ?)`,
		taskID, spec.RunID, nextSeq, spec.TaskDefID,
		spec.Action, spec.Prompt, spec.UserPrompt, spec.ResultType,
		state, dependsOn,
		assignTo, spec.RequireRole, spec.Citizens, runSlug,
		spec.ParentTaskID, spec.Trigger, spec.ClosesIssueSeq, now,
	); err != nil {
		// Translate the SQLite UNIQUE constraint into a
		// domain message — the raw error leaks table/column
		// names and an SQLite errno that's actionable to no
		// caller. The most likely cause is a colliding
		// task_def_id (template rule + same-issue retry
		// without bumping the suffix), so name the field the
		// caller can fix.
		if strings.Contains(err.Error(), "UNIQUE constraint failed: tasks.id") {
			return "", fmt.Errorf("task_def_id %q already exists in run %d — pick a different id (or bump the suffix on a re-spawn)", spec.TaskDefID, spec.RunID)
		}
		return "", fmt.Errorf("inserting spawned task: %w", err)
	}

	// 6. Increment cycle counter.
	if _, err := tx.Exec(`UPDATE runs SET cycle_budget_used = cycle_budget_used + 1, updated_at = ? WHERE id = ?`, now, spec.RunID); err != nil {
		return "", err
	}

	// 7. If the run was idle, the new ready task lifts it back
	//    to active — within the same tx so the state is correct
	//    by the time SpawnTask returns.
	if state == TaskReady && runState == string(RunIdle) {
		if _, err := tx.Exec(`UPDATE runs SET state = 'active', updated_at = ? WHERE id = ? AND state = 'idle'`, now, spec.RunID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(
			`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
			 VALUES (0, 'run_active', 'idle', '', ?, ?, ?, ?)`,
			spec.RunID, projectID,
			fmt.Sprintf(`{"from":"idle","to":"active","trigger":"spawn"}`),
			now,
		); err != nil {
			return "", err
		}
	}

	// 8. Emit task_spawned event with attribution + lineage.
	if _, err := tx.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, 'task_spawned', ?, ?, ?, ?, ?, ?)`,
		spec.SpawnedBy, spec.Trigger, taskID, spec.RunID, projectID,
		fmt.Sprintf(`{"task_def_id":%q,"action":%q,"parent_task_id":%q,"trigger":%q,"depends_on":%q}`,
			spec.TaskDefID, spec.Action, spec.ParentTaskID, spec.Trigger, dependsOn),
		now,
	); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return taskID, nil
}

// GetCycleBudget returns the current (used, max) for a run.
// Cheap read used by run_status renders and by the cycle-budget
// regression test.
func (s *Store) GetCycleBudget(runID int64) (used, max int, err error) {
	err = s.db.QueryRow(`SELECT cycle_budget_used, cycle_budget_max FROM runs WHERE id = ?`, runID).Scan(&used, &max)
	return
}

// ListRunsWithAutoTriage returns IDs of every run in the project
// that has a non-empty auto_triage_template, regardless of state.
// Used by handleFileIssue to drive re-evaluation across any run
// that opted into open-ended semantics — including runs that
// already completed (in which case the new issue should demote
// them back to idle, where the auto-triage hook can spawn a fix).
//
// Skips paused and failed — those are explicit operator states
// and shouldn't be auto-resumed by issue activity.
func (s *Store) ListRunsWithAutoTriage(projectID int64) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT id FROM runs
		 WHERE project_id = ? AND auto_triage_template != '' AND state NOT IN ('paused', 'failed')`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// generateIterationBranch is the single source of truth for the
// per-iteration branch identifier (living-workflow phase 6a).
// Both the plan-driven applySetClaim and the standalone
// Store.ClaimTask call this so the format never drifts between
// the two write paths. Format:
// "<run-slug>/<task_def_id>/iter-<N>".
//
// Vote tasks aggregate per-citizen submits into a single tally
// rather than producing one canonical commit, so the topic-
// branch flow doesn't model them well — the helper returns ""
// for vote action and the fat-client falls back to committing
// directly on the run branch (existing behavior).
//
// Review action DOES get a topic branch in the foundational v1
// design: a review's commit (metadata.json + result.md) holds
// the verdict prose and must travel through the same accept-
// then-merge gate as the upstream content it judges. Without a
// review topic branch, an approved review's commit lands on
// the run branch BEFORE the upstream's topic-→-run merge,
// producing a divergence that the FF gate refuses (the run
// branch and the upstream's topic branch then have unrelated
// tips).
//
// taskAction is the task's action ("answer" / "compute" / etc.),
// taskDefID is the YAML id, instanceKey is the per-instance
// for_each segment ("alpha" for alpha:expand, empty for
// singleton tasks), runSlug is the per-run slug for the
// enju/runs/{seq}-{slug}/ layout (defaults to "run" when empty).
// priorClaims should be the result of
// `SELECT COUNT(*) FROM task_claims WHERE task_id = ?` evaluated
// inside the same transaction so iter-N is monotonic under
// concurrent claims.
//
// The instance key is required: without it, for_each siblings
// (e.g. Python:pros and Go:pros) would share the same topic-
// branch name and the second submit would silently land on top
// of the first, producing a topic that diverges from main when
// the first sibling has already merged. Encoding the instance
// key gives each iteration its own unique ref.
func generateIterationBranch(taskAction, taskDefID, instanceKey, runSlug string, runSeq, priorClaims, citizens int, runHasMultiCitizen bool) string {
	if taskAction == "vote" {
		return ""
	}
	// Multi-citizen tasks (parallel reviewers, parallel
	// answerers) keep the legacy "commit directly to the run
	// branch" behavior. Each citizen writes their own per-
	// citizen subdir, so per-claim topic branches would
	// produce N divergent topics that can't FF-merge cleanly
	// onto the run branch — exactly the parallel-iteration
	// case the v1 wedge defers. Single-citizen tasks (the
	// vast majority) get the topic-branch flow.
	if citizens > 1 {
		return ""
	}
	// Run-level gate: if ANY task in the run is multi-
	// citizen, disable topics for every task in that run.
	// Otherwise a single-citizen draft's topic could be
	// forked from main_old, then a multi-citizen reviewer
	// commits directly to main, then the draft's topic ↔
	// main FF would fail. Conservative v1 behavior; the
	// rebase-on-non-FF work (v2) lets us re-enable per-task
	// topics independently.
	if runHasMultiCitizen {
		return ""
	}
	slug := runSlug
	if slug == "" {
		slug = "run"
	}
	// Encode run seq alongside the slug so two runs with the
	// same name (or two runs created from the same template)
	// don't collide on a single topic-branch ref. The runs
	// dir on disk uses the same `<seq>-<slug>` segment.
	runSegment := fmt.Sprintf("%d-%s", runSeq, slug)
	// Branch names can't contain ":" — use "/" as the
	// separator so the instance key sits as its own path
	// segment in the ref namespace (refs/heads/<run-segment>/
	// <instance-key>/<task-def>/iter-N). For singleton tasks
	// (no instance key) the segment collapses naturally.
	defSegment := taskDefID
	if instanceKey != "" {
		defSegment = instanceKey + "/" + taskDefID
	}
	return fmt.Sprintf("%s/%s/iter-%d", runSegment, defSegment, priorClaims+1)
}

// SetAutoTriageTemplate persists the run-level auto-triage rule
// (JSON-encoded RemediationTemplate). Called by the create-run
// path after the RunRecord is inserted; runs without a rule
// keep the schema default ('').
func (s *Store) SetAutoTriageTemplate(runID int64, templateJSON string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET auto_triage_template = ?, updated_at = ? WHERE id = ?`,
		templateJSON, time.Now(), runID,
	)
	return err
}

// GetAutoTriageTemplate returns the run's auto-triage rule, or
// "" when none is configured. Empty string short-circuits the
// auto-triage hook — every static workflow's runs return "".
func (s *Store) GetAutoTriageTemplate(runID int64) (string, error) {
	var t string
	err := s.db.QueryRow(`SELECT auto_triage_template FROM runs WHERE id = ?`, runID).Scan(&t)
	return t, err
}

// CountTasksWithDefIDPrefix returns how many tasks in the run
// have a task_def_id starting with prefix. Used by the
// remediation-spawn path to pick the next "<target>_remediation_N"
// index without scanning the event log. Indexed by run_id.
//
// Caller-supplied prefix is escaped before going into the LIKE
// pattern so any %, _, or \ characters that appear in a task
// def id (today disallowed by the parser, but if the grammar
// ever loosens) are matched literally instead of as wildcards.
// The ESCAPE '\' clause registers backslash as the escape
// character; SQLite's default LIKE has none.
func (s *Store) CountTasksWithDefIDPrefix(runID int64, prefix string) (int, error) {
	escaped := likeEscape(prefix)
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE run_id = ? AND task_def_id LIKE ? ESCAPE '\'`,
		runID, escaped+"%",
	).Scan(&n)
	return n, err
}

// likeEscape escapes the three SQL LIKE meta-characters (%, _,
// and the escape character itself) so a literal-prefix match
// stays literal. Replaces \ first to avoid double-escaping the
// other replacements' new backslashes.
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// SetCycleBudgetMax bumps the budget cap on a run. Use to give
// a paused-by-exhaustion run room to keep going. Refuses if the
// new max is below current used (would be immediately
// exhausted). Emits `cycle_budget_changed` in the same tx as
// the UPDATE so the audit log captures who raised the safety
// cap and when — set_cycle_budget is a privileged operator
// action and the event log is its only after-the-fact record.
func (s *Store) SetCycleBudgetMax(runID int64, citizenID int64, newMax int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var used, oldMax int
	var projectID int64
	if err := tx.QueryRow(
		`SELECT cycle_budget_used, cycle_budget_max, project_id FROM runs WHERE id = ?`, runID,
	).Scan(&used, &oldMax, &projectID); err != nil {
		return err
	}
	if newMax < used {
		return fmt.Errorf("new max %d is below current used %d — would be immediately exhausted", newMax, used)
	}
	if newMax == oldMax {
		// No-op set; don't emit a noise event.
		return tx.Commit()
	}

	now := time.Now()
	if _, err := tx.Exec(
		`UPDATE runs SET cycle_budget_max = ?, updated_at = ? WHERE id = ?`,
		newMax, now, runID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO contribution_events (citizen_id, event_type, event_subtype, task_id, run_id, project_id, metadata, created_at)
		 VALUES (?, 'cycle_budget_changed', '', '', ?, ?, ?, ?)`,
		citizenID, runID, projectID,
		fmt.Sprintf(`{"old_max":%d,"new_max":%d,"used":%d}`, oldMax, newMax, used),
		now,
	); err != nil {
		return err
	}
	return tx.Commit()
}
