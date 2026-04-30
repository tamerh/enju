package test

// Targeted tests for the foundational v1 topic-branch flow
// (living-workflow phase 6b). These pin the load-bearing
// invariants the broader integration suite passes through
// only incidentally — without them, a refactor that quietly
// changes the merge logic (e.g. always-merge-on-submit, never-
// merge-on-reject inverted, fork-from-wrong-base) would still
// produce green CI. Each test asserts ONE specific contract.

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPApprovedReviewMergesUpstreamAndVerdict pins the happy
// path: a reviewer approves a single-citizen draft. The run
// branch must advance to a tip that contains BOTH the upstream's
// commit AND the reviewer's verdict prose, in one fast-forward
// step. Pre-fix, the FF push would refuse because the review
// topic was forked from main (missing upstream content) or
// from a stale upstream topic (missing other accepted work).
func TestMCPApprovedReviewMergesUpstreamAndVerdict(t *testing.T) {
	eachRemoteMode(t, "TopicApprove", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
		reviewer := h.newMCPClientAs(t, "Approver")
		projectID := h.createTestProject()

		yaml := `name: "approve merges upstream"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: gate
    action: review
    reviews: draft
    prompt: "Approve or reject."
`
		h.mcpCreateRunInline(t, projectID, yaml)

		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "DRAFT-CONTENT-marker")

		// Draft is single-citizen with a downstream review, so
		// it transitions to ACCEPTED on submit and its topic
		// FF-merges to main immediately. Snapshot main's tip
		// after the draft merge so we can assert the review
		// merge advances it further.
		remoteURL := h.remoteFor(projectID)
		mainAfterDraft := bareRefSHA(t, remoteURL, "main")
		if mainAfterDraft == "" {
			t.Fatalf("main missing on bare after draft submit")
		}
		// Sanity: draft's content must be on main (the FF
		// merge happened).
		draftPath := filepath.Join(h.runDir(1), "draft/result.md")
		if body, ok := readRepoFileOnBranch(t, remoteURL, "main", draftPath); !ok || !strings.Contains(string(body), "DRAFT-CONTENT-marker") {
			t.Fatalf("draft content missing on main after auto-accept; ok=%v body=%q", ok, body)
		}

		// Reviewer approves. The review's topic should fork
		// from upstream's topic (since draft is now accepted
		// and on main, fork-from-current-main is fine too —
		// either way the review topic includes the draft
		// commit). Approve triggers FF merge of review topic
		// to main.
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "VERDICT-PROSE-marker", "approve")

		mainAfterApprove := bareRefSHA(t, remoteURL, "main")
		if mainAfterApprove == mainAfterDraft {
			t.Errorf("approve should have advanced main beyond draft tip; main stayed at %s", mainAfterDraft)
		}

		// The reviewer's verdict prose lives in the review
		// task's result.md and must now be visible on main —
		// that's the whole point of the merge.
		reviewPath := filepath.Join(h.runDir(1), "gate/result.md")
		body, ok := readRepoFileOnBranch(t, remoteURL, "main", reviewPath)
		if !ok {
			t.Errorf("review result.md missing from main after approve")
		}
		if !strings.Contains(string(body), "VERDICT-PROSE-marker") {
			t.Errorf("review content on main missing VERDICT-PROSE-marker, got: %q", body)
		}
	})
}

// TestMCPRejectedReviewLeavesMainUntouched pins the negative
// path: a reviewer rejects (or request_changes) a single-
// citizen draft. Per design, the topic ref is retained as
// audit but the run branch does NOT advance. Pre-fix, the
// reject would either also merge (polluting main) or the
// topic wouldn't be pushed (losing the audit trail).
func TestMCPRejectedReviewLeavesMainUntouched(t *testing.T) {
	eachRemoteMode(t, "TopicReject", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
		reviewer := h.newMCPClientAs(t, "Rejecter")
		projectID := h.createTestProject()

		yaml := `name: "reject leaves main alone"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: gate
    action: review
    reviews: draft
    prompt: "Approve or reject."
`
		h.mcpCreateRunInline(t, projectID, yaml)

		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "DRAFT-FOR-REJECTION")

		remoteURL := h.remoteFor(projectID)
		mainBeforeReview := bareRefSHA(t, remoteURL, "main")
		if mainBeforeReview == "" {
			t.Fatalf("main missing on bare before review")
		}

		// Reviewer rejects the draft. Topic gets pushed (audit)
		// but no merge to main happens.
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "needs work", "request_changes")

		mainAfterReview := bareRefSHA(t, remoteURL, "main")
		if mainAfterReview != mainBeforeReview {
			t.Errorf("main advanced during rejected review:\n  before=%s\n  after=%s\nrejected reviews must not merge to main",
				mainBeforeReview, mainAfterReview)
		}

		// The review topic ref MUST exist on the bare —
		// rejected work stays as audit, not deleted.
		gateTask := h.taskGet("gate")
		topic, _ := gateTask["latest_completed_branch"].(string)
		if topic == "" {
			t.Fatalf("gate task missing latest_completed_branch; cannot verify topic retention")
		}
		if sha := bareRefSHA(t, remoteURL, topic); sha == "" {
			t.Errorf("rejected review's topic %q missing from bare — audit ref was not retained", topic)
		}
	})
}

// TestMCPReviewTopicCarriesUpstreamForward pins the user-
// facing fork-base invariant: when the reviewer's topic
// merges to main on approve, the merged tip contains the
// upstream's commit too — no separate upstream merge step,
// no missing draft content on main after approve.
//
// In v1 the path that delivers this is "review forks from
// current main" because every action:answer auto-accepts on
// submit, which FF-merges its topic onto main BEFORE the
// review claims (a review only becomes ready when its target
// is in state=accepted, per Store.UpdateReadyTasks). So at
// gate's claim time, draft.State == accepted and main
// already carries draft's commit. The router's
// upstream_iteration_branch field is gated on `state !=
// accepted` and is therefore empty in this scenario — the
// conditional is asserted explicitly below.
//
// The "upstream still review_pending → fork from
// upstream_topic" branch in router.go's toTaskResponse is
// future-proofing for v2 (chained reviews, races between
// invalidate and claim) and is unreachable under v1's
// normal flow. Tested implicitly by the integration suite,
// which would surface a non-FF refusal if the wrong fork
// base were ever chosen.
func TestMCPReviewTopicCarriesUpstreamForward(t *testing.T) {
	eachRemoteMode(t, "TopicForkBase", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
		reviewer := h.newMCPClientAs(t, "ForkChecker")
		projectID := h.createTestProject()

		yaml := `name: "review carries upstream"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: gate
    action: review
    reviews: draft
    prompt: "Approve."
`
		h.mcpCreateRunInline(t, projectID, yaml)

		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "UPSTREAM-CONTENT-DRAFT")

		// Draft auto-accepted on submit (single-citizen
		// answer). Pin the conditional contract: because
		// draft.State == accepted, gate's response must NOT
		// surface upstream_iteration_branch — fork is from
		// current main, not from upstream's topic.
		gate := h.taskGet("gate")
		if got := gate["state"]; got != "ready" {
			t.Fatalf("expected gate ready after draft accept, got %v", got)
		}
		if up, _ := gate["upstream_iteration_branch"].(string); up != "" {
			t.Errorf("gate.upstream_iteration_branch should be empty when upstream accepted (router gates on state); got %q", up)
		}

		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "REVIEW-VERDICT", "approve")

		// The review's topic ref must contain BOTH the
		// upstream's commit (inherited via fork from main,
		// which had the auto-accepted draft) AND the
		// reviewer's own verdict commit. This is the
		// invariant that makes the approve merge work in one
		// FF step — without it, the FF would either lose
		// upstream content (if the topic forked from somewhere
		// without it) or refuse non-FF (if main had moved
		// past the topic's base).
		gate = h.taskGet("gate")
		reviewTopic, _ := gate["latest_completed_branch"].(string)
		if reviewTopic == "" {
			t.Fatalf("gate missing latest_completed_branch")
		}
		remoteURL := h.remoteFor(projectID)
		draftPath := filepath.Join(h.runDir(1), "draft/result.md")
		body, ok := readRepoFileOnBranch(t, remoteURL, reviewTopic, draftPath)
		if !ok {
			t.Errorf("upstream draft's result.md not visible on review topic %q — review topic didn't carry upstream commit forward", reviewTopic)
		}
		if !strings.Contains(string(body), "UPSTREAM-CONTENT-DRAFT") {
			t.Errorf("review topic has wrong draft content: %q", body)
		}
		gatePath := filepath.Join(h.runDir(1), "gate/result.md")
		if body, ok := readRepoFileOnBranch(t, remoteURL, reviewTopic, gatePath); !ok || !strings.Contains(string(body), "REVIEW-VERDICT") {
			t.Errorf("review topic missing its own result.md: ok=%v body=%q", ok, body)
		}
	})
}

// TestMCPSingleCitizenReclaimGetsNewTopic pins the iteration-
// rotation contract: after request_changes invalidates iter-1,
// re-claiming the same task produces iter-2 on a DISTINCT
// topic ref. The iter-1 ref is retained (audit); iter-2 is
// brand new. This guards against a regression where the
// branch-name generator forgets to bump iter-N or where the
// re-claim quietly reuses iter-1's ref (which would silently
// destroy the iter-1 audit).
func TestMCPSingleCitizenReclaimGetsNewTopic(t *testing.T) {
	eachRemoteMode(t, "TopicReclaimRotates", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
		reviewer := h.newMCPClientAs(t, "RotateReviewer")
		projectID := h.createTestProject()

		yaml := `name: "iter rotates on reclaim"
version: 1
tasks:
  - id: draft
    action: answer
    prompt: "Write."
  - id: gate
    action: review
    reviews: draft
    prompt: "Approve."
`
		h.mcpCreateRunInline(t, projectID, yaml)

		// iter-1: submit draft + reject it. After the cascade,
		// draft bounces back to READY for revision.
		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "ITER-1-content")
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "needs work", "request_changes")

		// Capture iter-1's topic ref + SHA from the bare BEFORE
		// the re-claim (so iter-2's stamp doesn't overwrite it
		// in our snapshot).
		remoteURL := h.remoteFor(projectID)
		iters := h.draftIterationBranches(t, "draft")
		if len(iters) != 1 {
			t.Fatalf("expected 1 iteration before reclaim, got %d: %v", len(iters), iters)
		}
		iter1Topic := iters[0]
		iter1SHA := bareRefSHA(t, remoteURL, iter1Topic)
		if iter1SHA == "" {
			t.Fatalf("iter-1 topic %q missing on bare before reclaim", iter1Topic)
		}

		// iter-2: re-claim draft and submit a new revision.
		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "ITER-2-content")

		iters = h.draftIterationBranches(t, "draft")
		if len(iters) != 2 {
			t.Fatalf("expected 2 iterations after reclaim, got %d: %v", len(iters), iters)
		}
		iter2Topic := iters[1]
		if iter2Topic == iter1Topic {
			t.Fatalf("iter-2 reused iter-1's topic %q — branch generator forgot to bump iter-N", iter1Topic)
		}
		if !strings.HasSuffix(iter2Topic, "/iter-2") {
			t.Errorf("iter-2 topic %q doesn't end in /iter-2 (encoding contract)", iter2Topic)
		}

		// iter-1's ref must still exist + still point at the
		// same SHA — re-claim must not delete or rewrite it.
		if got := bareRefSHA(t, remoteURL, iter1Topic); got != iter1SHA {
			t.Errorf("iter-1 topic %q changed during reclaim:\n  before=%s\n  after=%s",
				iter1Topic, iter1SHA, got)
		}

		// iter-2's commit must exist as a brand-new ref.
		if sha := bareRefSHA(t, remoteURL, iter2Topic); sha == "" {
			t.Errorf("iter-2 topic %q missing from bare after reclaim", iter2Topic)
		}
	})
}

// draftIterationBranches returns the branch column for every
// claim row on the named task, ordered oldest-first. Used by
// the reclaim test to assert iter-N rotation. Reads through
// enju_list_iterations rather than the task response so each
// historical claim is enumerated, not just the active one.
//
// The list_iterations formatter emits a "branch:       <name>"
// line per iteration when the branch is non-empty (see
// handleListIterations in mcpserver/task.go). Scan for that
// prefix rather than full-parsing the rendered table.
func (h *mcpHarness) draftIterationBranches(t *testing.T, shortID string) []string {
	t.Helper()
	out := mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
		"task_id": h.taskID(shortID),
	}))
	const prefix = "branch:"
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		branches = append(branches, strings.TrimSpace(trimmed[len(prefix):]))
	}
	return branches
}

// bareRefSHA returns the tip commit SHA of `branch` on a bare
// remote, or "" if the ref doesn't exist. Sibling of
// readRepoFileOnBranch — that one reads file content; this
// one just resolves a ref. Used by the topic-branch tests to
// assert ref existence (audit retention) and ref movement
// (merge happened / didn't happen).
func bareRefSHA(t *testing.T, remoteURL, branch string) string {
	t.Helper()
	cmd := execCommand("git", "--git-dir", remoteURL, "rev-parse", "--verify", "refs/heads/"+branch)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
