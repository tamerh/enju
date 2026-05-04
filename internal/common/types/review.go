// Package types holds shared domain enums that cross the
// coordinator ↔ fat-client boundary. Both sides need to agree
// on the wire-format strings for review decisions, claim
// outcomes, citizen kinds, etc., so the canonical typed form
// lives in common where either side can import it without
// violating the layering rule (tools/check-imports.sh).
//
// Keep this package small and dependency-free. It must not
// import anything from internal/coordinator/ or
// internal/fatclient/ — only stdlib.
package types

// ReviewDecision is the verdict a reviewer submits on an
// action:review task. Stored on TaskRecord.ReviewDecision
// (coordinator-side) and on TaskClaimRecord.Option for review
// submissions; written to git trailers as the verdict; sent
// over the wire by the fat-client submit handler.
//
// Empty string is the "no decision yet" sentinel — used while
// the task is in flight or after invalidation clears the prior
// verdict.
//
// Tally semantics (see engine.EvaluateReviewTally):
//
//   - Approve: counts toward the resolve threshold.
//   - Reject: hard-kills the review immediately under
//     any-reject-kills (the default).
//   - RequestChanges: counts as negative but reopens the
//     iteration for revision rather than killing it.
//   - Comment: non-blocking; recorded for context but
//     doesn't affect the tally.
type ReviewDecision string

const (
	ReviewDecisionApprove        ReviewDecision = "approve"
	ReviewDecisionReject         ReviewDecision = "reject"
	ReviewDecisionRequestChanges ReviewDecision = "request_changes"
	ReviewDecisionComment        ReviewDecision = "comment"
)

// IsValidReviewDecision reports whether s is one of the four
// declared verdicts. Empty string is rejected — callers that
// want to allow "no decision yet" check that explicitly.
func IsValidReviewDecision(s string) bool {
	switch ReviewDecision(s) {
	case ReviewDecisionApprove, ReviewDecisionReject,
		ReviewDecisionRequestChanges, ReviewDecisionComment:
		return true
	}
	return false
}
