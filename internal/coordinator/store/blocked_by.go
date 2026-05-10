package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BlockedByKind enumerates the reasons a run can be stuck in
// WAITING. Order encodes the priority used by computeBlockedBy:
// the first kind that applies wins. Phase 8.5 design call —
// surface only the top blocker so enju_run_status can render
// one actionable line instead of a list of overlapping causes.
type BlockedByKind string

const (
	// BlockedByReview — a review task is CLAIMED or RUNNING
	// with no recent activity. Reviewer is the bottleneck.
	BlockedByReview BlockedByKind = "review"

	// BlockedByHumanClaim — a non-review task assigned to a
	// human is READY but unclaimed. Waiting on a human to pick
	// it up.
	BlockedByHumanClaim BlockedByKind = "human_claim"

	// BlockedByArtifact — a pending task can't satisfy its
	// reads_artifacts because every candidate writer is in
	// SUBMITTED (Phase 8.3 deferred-accept gate). The artifact
	// row exists but its writer hasn't merged yet.
	BlockedByArtifact BlockedByKind = "artifact"

	// BlockedByStuck — fall-through. None of the above apply
	// but the run is in WAITING. System bug, unreachable
	// dependency, or a state we haven't taught the evaluator
	// about. The detail field carries a short diagnostic.
	BlockedByStuck BlockedByKind = "stuck"
)

// BlockedBy is the structured form of the runs.blocked_by JSON
// column. Surface readers (enju_run_status) and tests
// unmarshal into this; computeBlockedBy / applyCompleteRun
// marshal from it. Field set varies by Kind — empty fields
// drop out of the JSON via omitempty.
type BlockedBy struct {
	Kind         BlockedByKind `json:"kind"`
	Task         string        `json:"task,omitempty"`
	Assignee     string        `json:"assignee,omitempty"`
	Since        string        `json:"since,omitempty"`         // RFC3339; review kind only
	AwaitingPath string        `json:"awaiting_path,omitempty"` // artifact kind only
	Detail       string        `json:"detail,omitempty"`        // stuck kind only
}

// computeBlockedBy walks the run's task graph and returns the
// top-priority blocker as JSON, ready to UPSERT into the
// runs.blocked_by column. Caller invokes this only when the
// run state evaluator decides to land on RunWaiting; for any
// other terminal state the column is cleared (NULL) and this
// function isn't called.
//
// Priority order (first match wins):
//
//  1. review — a CLAIMED/RUNNING review task. The reviewer is
//     holding the run open; surface them so the operator can
//     ping or reassign.
//  2. human_claim — a READY non-review task whose assign_to
//     names someone but has no claim. Human hasn't started
//     yet.
//  3. artifact — a PENDING task whose reads_artifacts can't
//     find any writer in {accepted, skipped} (the Phase 8.3
//     gate). Surface the path + the task waiting on it; an
//     operator looking at this will go check the writer's
//     SUBMITTED state.
//  4. stuck — fall-through. Detail captures whatever signal
//     the evaluator could glean.
//
// Caller holds the apply transaction; this fn shares it via
// the *sql.Tx (avoiding visibility races against an in-tx
// state flip that triggered the WAITING transition).
//
// Returns the JSON blob and a non-nil error only for
// underlying SQL failures. A blocker can always be computed
// (worst case: stuck).
func computeBlockedBy(tx *sql.Tx, runID int64) (string, error) {
	if blocker, err := findReviewBlocker(tx, runID); err != nil {
		return "", err
	} else if blocker != nil {
		return marshalBlockedBy(blocker)
	}
	if blocker, err := findHumanClaimBlocker(tx, runID); err != nil {
		return "", err
	} else if blocker != nil {
		return marshalBlockedBy(blocker)
	}
	if blocker, err := findArtifactBlocker(tx, runID); err != nil {
		return "", err
	} else if blocker != nil {
		return marshalBlockedBy(blocker)
	}
	return marshalBlockedBy(&BlockedBy{
		Kind:   BlockedByStuck,
		Detail: "no actionable blocker identified — check run topology",
	})
}

// findReviewBlocker looks for a review task currently
// CLAIMED or RUNNING. Picks the oldest claim_at so the most-
// stuck reviewer surfaces (multiple claimed reviews mean
// progress is happening; stale ones are the real bottleneck).
func findReviewBlocker(tx *sql.Tx, runID int64) (*BlockedBy, error) {
	row := tx.QueryRow(
		`SELECT t.id, t.claimed_at, c.username
		 FROM tasks t
		 LEFT JOIN citizens c ON t.claimed_by = c.id
		 WHERE t.run_id = ? AND t.action = 'review'
		   AND t.state IN ('claimed', 'running')
		 ORDER BY t.claimed_at ASC
		 LIMIT 1`, runID)
	var taskID, username sql.NullString
	var claimedAt sql.NullTime
	if err := row.Scan(&taskID, &claimedAt, &username); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !taskID.Valid {
		return nil, nil
	}
	b := &BlockedBy{
		Kind:     BlockedByReview,
		Task:     taskID.String,
		Assignee: username.String,
	}
	if claimedAt.Valid {
		b.Since = claimedAt.Time.UTC().Format(time.RFC3339)
	}
	return b, nil
}

// findHumanClaimBlocker looks for a non-review READY task with
// a non-empty assign_to and no active claim. Picks the lowest
// seq so the oldest "waiting for human" task surfaces first —
// matches the natural reading order of run_status.
func findHumanClaimBlocker(tx *sql.Tx, runID int64) (*BlockedBy, error) {
	rows, err := tx.Query(
		`SELECT id, assign_to FROM tasks
		 WHERE run_id = ? AND state = 'ready'
		   AND action != 'review'
		   AND assign_to != '' AND assign_to != '[]'
		   AND claimed_by IS NULL
		 ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, assignToJSON string
		if err := rows.Scan(&taskID, &assignToJSON); err != nil {
			return nil, err
		}
		var assignees []string
		if err := json.Unmarshal([]byte(assignToJSON), &assignees); err != nil {
			// Malformed JSON in the column shouldn't crash
			// the evaluator — skip and try the next.
			continue
		}
		if len(assignees) == 0 {
			continue
		}
		// First-listed assignee is the surfaced one. For
		// multi-assigned tasks the operator can pull the full
		// list from enju_get_task; the blocker line is
		// deliberately one-name to keep run_status scannable.
		return &BlockedBy{
			Kind:     BlockedByHumanClaim,
			Task:     taskID,
			Assignee: assignees[0],
		}, nil
	}
	return nil, rows.Err()
}

// findArtifactBlocker looks for a PENDING task whose
// reads_artifacts can't be satisfied because every declared
// path's writer is in a non-terminal state (SUBMITTED-in-
// flight or earlier). Surfaces the first such path —
// operator goes from "blocked on path X" to "find writer of
// X" via enju_get_artifact's provenance.
//
// The artifact-state gate applyUpdateReadyTasks runs is what
// kept the task in PENDING; this query mirrors its logic to
// answer "which path is missing." Implementation walks
// pending tasks in seq order; for each, walks its
// reads_artifacts paths and checks whether any has no
// terminal-good writer (artifact row exists but its writer
// is not in {accepted, skipped} OR no row exists at all).
func findArtifactBlocker(tx *sql.Tx, runID int64) (*BlockedBy, error) {
	var projectID int64
	var runBranch string
	if err := tx.QueryRow(`SELECT project_id, branch FROM runs WHERE id = ?`, runID).Scan(&projectID, &runBranch); err != nil {
		return nil, err
	}
	if runBranch == "" {
		runBranch = "main"
	}

	rows, err := tx.Query(
		`SELECT id, reads_artifacts FROM tasks
		 WHERE run_id = ? AND state = 'pending'
		   AND reads_artifacts != '' AND reads_artifacts != '[]'
		 ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var taskID, readsJSON string
		if err := rows.Scan(&taskID, &readsJSON); err != nil {
			return nil, err
		}
		var paths []string
		if err := json.Unmarshal([]byte(readsJSON), &paths); err != nil {
			continue
		}
		for _, p := range paths {
			if p == "" {
				continue
			}
			// Mirrors the gate in applyUpdateReadyTasks: a
			// path is satisfied iff its row exists AND no
			// non-terminal writer is in flight. We surface a
			// path as the blocker when it FAILS that gate.
			var visible int
			err := tx.QueryRow(
				`SELECT 1 FROM artifacts a
				 WHERE a.project_id = ? AND a.branch = ? AND a.path = ?
				   AND NOT EXISTS (
				     SELECT 1 FROM tasks
				     WHERE tasks.id = a.last_task_id
				       AND tasks.state NOT IN ('accepted', 'skipped')
				   )
				 LIMIT 1`,
				projectID, runBranch, p,
			).Scan(&visible)
			if err == nil {
				continue // path is satisfied; check next
			}
			if err != sql.ErrNoRows {
				return nil, err
			}
			return &BlockedBy{
				Kind:         BlockedByArtifact,
				Task:         taskID,
				AwaitingPath: p,
			}, nil
		}
	}
	return nil, rows.Err()
}

// marshalBlockedBy renders the BlockedBy as a compact JSON
// blob. Returns an error only on the impossible case of
// json.Marshal failing on a struct with no recursive types —
// we still propagate it rather than panicking, mirroring the
// rest of the apply.go error-handling shape.
func marshalBlockedBy(b *BlockedBy) (string, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("marshal blocked_by: %w", err)
	}
	return string(body), nil
}

// ParseBlockedBy is the surface-side helper. Returns nil when
// the column is empty (no blocker recorded) or the JSON is
// malformed (forward-compat: a future field added to this
// struct shouldn't crash older readers, just render nothing).
// Exported so enju_run_status renderers and tests can both
// use it without re-implementing the parse.
func ParseBlockedBy(raw string) *BlockedBy {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var b BlockedBy
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil
	}
	if b.Kind == "" {
		return nil
	}
	return &b
}
