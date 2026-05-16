package types

// ClaimOutcome is the terminal label written to
// task_claims.outcome when a claim row closes. NULL while the
// claim is open; one of the constants below once it's done.
//
// Distinct values (not all redundant — they record HOW the
// claim ended, which the projection layer surfaces in the
// audit feed):
//
//   - Completed: the citizen submitted and (for review tasks)
//     the verdict landed; for non-review tasks this is the
//     terminal-success label.
//   - Rejected: a reviewer's request_changes or reject verdict
//     closed THIS reviewed-task's claim — the task itself
//     re-opens for revision (request_changes) or fails (reject),
//     but the claim row is terminally rejected either way.
//   - Released: the citizen released voluntarily
//     (enju_release_task) — the task goes back to READY.
//   - TimedOut: the reaper hit the claim's deadline.
//   - Invalidated: enju_invalidate_task wiped the task; open
//     claim rows close without a verdict.
//   - Abandoned: a different citizen claimed an open multi-citizen
//     slot; the prior open row closes without verdict.
//
// Empty string is the open-claim sentinel — only meaningful in
// SQL ("WHERE outcome IS NULL") at the DB layer; service code
// handles open vs closed via NULL checks, not by comparing to
// empty string.
type ClaimOutcome string

const (
	ClaimOutcomeCompleted   ClaimOutcome = "completed"
	ClaimOutcomeRejected    ClaimOutcome = "rejected"
	ClaimOutcomeReleased    ClaimOutcome = "released"
	ClaimOutcomeTimedOut    ClaimOutcome = "timed_out"
	ClaimOutcomeInvalidated ClaimOutcome = "invalidated"
	ClaimOutcomeAbandoned   ClaimOutcome = "abandoned"
	// ClaimOutcomeFailed closes the iteration of an attempt whose
	// compute script errored. Distinct from `invalidated` (no
	// verdict, collateral) and `rejected` (a reviewer judged it):
	// the work ran and failed on its own merits. Closing the row
	// with this outcome is what lets a subsequent enju_retry_task
	// re-claim get a fresh iter_seq instead of colliding with the
	// failed attempt's still-open row — each retry is its own
	// auditable iteration.
	ClaimOutcomeFailed ClaimOutcome = "failed"
)

// IsValidClaimOutcome reports whether s is one of the seven
// declared terminal outcomes. Empty string is rejected —
// callers that want to allow "still open" check that explicitly.
func IsValidClaimOutcome(s string) bool {
	switch ClaimOutcome(s) {
	case ClaimOutcomeCompleted, ClaimOutcomeRejected,
		ClaimOutcomeReleased, ClaimOutcomeTimedOut,
		ClaimOutcomeInvalidated, ClaimOutcomeAbandoned,
		ClaimOutcomeFailed:
		return true
	}
	return false
}
