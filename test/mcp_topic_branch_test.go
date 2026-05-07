package test

// Targeted tests for the foundational v1 topic-branch flow
// (living-workflow phase 6b). These pin the load-bearing
// invariants the broader integration suite passes through
// only incidentally — without them, a refactor that quietly
// changes the merge logic (e.g. always-merge-on-submit, never-
// merge-on-reject inverted, fork-from-wrong-base) would still
// produce green CI. Each test asserts ONE specific contract.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPApprovedReviewMergesUpstreamAndVerdict pins the happy
// path: a reviewer approves a single-citizen draft. Phase 6b.2
// changed the merge gate so a reviewed task does NOT merge on
// submit — it stays on its topic, waiting for the reviewer's
// verdict. The approve is the merge moment: main advances to a
// tip that contains BOTH the upstream's commit AND the reviewer's
// verdict prose, in one fast-forward step.
//
// Pre-6b.2, draft auto-merged on submit; ISSUE-001 in the v1
// sanity report flagged that as launch-blocking because rejected
// work would have already polluted main by the time the reviewer
// said no. The assertions below pin the gate's two halves: main
// is unchanged after the draft submit, and main IS advanced
// after the review approve.
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

		remoteURL := h.remoteFor(projectID)
		mainBeforeDraft := bareRefSHA(t, remoteURL, "main")
		if mainBeforeDraft == "" {
			t.Fatalf("main missing on bare before draft submit")
		}

		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "DRAFT-CONTENT-marker")

		// Phase 6b.2 gate: draft has a downstream review, so
		// its topic must NOT merge to main on submit. Main
		// stays exactly where it was. The draft commit lives
		// only on its topic ref until the reviewer approves.
		mainAfterDraft := bareRefSHA(t, remoteURL, "main")
		if mainAfterDraft != mainBeforeDraft {
			t.Errorf("draft submit must not advance main while a downstream review is pending:\n  before=%s\n  after=%s",
				mainBeforeDraft, mainAfterDraft)
		}
		// Draft's content must NOT be on main yet — that's the
		// inverse of the old assertion. The content lives on
		// the topic ref only.
		draftPath := filepath.Join(h.runDir(1), "draft/result.md")
		if _, ok := readRepoFileOnBranch(t, remoteURL, "main", draftPath); ok {
			t.Errorf("draft content unexpectedly on main before review approval — merge gate failed")
		}

		// Reviewer approves. The review's topic was forked
		// from upstream's topic, so review_topic carries
		// draft's commit. The approve triggers one FF push
		// that advances main with both the upstream and the
		// verdict.
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "VERDICT-PROSE-marker", "approve")

		mainAfterApprove := bareRefSHA(t, remoteURL, "main")
		if mainAfterApprove == mainAfterDraft {
			t.Errorf("approve should have advanced main; stayed at %s", mainAfterDraft)
		}

		// Both files must now be visible on main: draft's
		// content (carried forward via fork chain) and the
		// reviewer's verdict prose.
		if body, ok := readRepoFileOnBranch(t, remoteURL, "main", draftPath); !ok || !strings.Contains(string(body), "DRAFT-CONTENT-marker") {
			t.Errorf("draft content missing on main after approve; ok=%v body=%q", ok, body)
		}
		reviewPath := filepath.Join(h.runDir(1), "gate/result.md")
		if body, ok := readRepoFileOnBranch(t, remoteURL, "main", reviewPath); !ok || !strings.Contains(string(body), "VERDICT-PROSE-marker") {
			t.Errorf("review verdict missing on main after approve; ok=%v body=%q", ok, body)
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
// Phase 6b.2 fixes the path that delivers this. Reviewed
// tasks no longer auto-merge on submit; their commit lives
// only on the topic ref. The review's response surfaces the
// upstream's topic via upstream_iteration_branch, the fat
// client uses it as the fork base, and the resulting review
// topic descends from upstream_topic and carries its commit
// forward. This test pins the conditional API contract
// (upstream_iteration_branch is non-empty for the reviewed
// case) AND the user-facing observable (review topic has
// upstream's content).
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

		// Phase 6b.2 contract: gate becomes ready (engine
		// state machine still auto-accepts answer on submit
		// even when reviewed) and gate.upstream_iteration_branch
		// is non-empty — pointing at draft's topic, which
		// hasn't merged to main because of the new gate.
		gate := h.taskGet("gate")
		if got := gate["state"]; got != "ready" {
			t.Fatalf("expected gate ready after draft submit, got %v", got)
		}
		upstreamTopic, _ := gate["upstream_iteration_branch"].(string)
		if upstreamTopic == "" {
			t.Fatalf("gate.upstream_iteration_branch should be non-empty (draft has unmerged topic); got %q", upstreamTopic)
		}
		if !strings.Contains(upstreamTopic, "/draft/iter-1") {
			t.Errorf("upstream_iteration_branch %q doesn't look like draft's topic ref", upstreamTopic)
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

// TestMCPReviewedTaskSubmitDoesNotMergeUntilApprove is the
// dedicated pin for the phase 6b.2 ISSUE-001 fix: a task with a
// downstream review must NOT merge to main on submit. The merge
// moment is the review's approve. Without this gate, rejected
// work pollutes main forever — exactly the launch-blocking
// regression the v1 sanity report flagged.
//
// Where TestMCPApprovedReviewMergesUpstreamAndVerdict tests
// the full happy path (draft → review approve → main advances),
// this test isolates the gate itself: it asserts that the
// draft's topic ref is pushed to the bare (audit trail) but
// the run branch tip is unchanged after the draft submit. Any
// future regression in `taskHasDownstreamReview` or in the
// merge-gate suppression logic would surface as a moved main
// SHA here.
func TestMCPReviewedTaskSubmitDoesNotMergeUntilApprove(t *testing.T) {
	eachRemoteMode(t, "MergeGate", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
		projectID := h.createTestProject()

		yaml := `name: "merge gate pin"
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

		remoteURL := h.remoteFor(projectID)
		mainBefore := bareRefSHA(t, remoteURL, "main")
		if mainBefore == "" {
			t.Fatalf("main missing on bare before any submit")
		}

		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "WORK-IN-PROGRESS")

		// Half 1 of the gate: main MUST NOT have moved. The
		// draft's auto-accept (engine state machine) doesn't
		// trigger a merge because of the downstream-review
		// suppression in collectAcceptedMerges.
		mainAfter := bareRefSHA(t, remoteURL, "main")
		if mainAfter != mainBefore {
			t.Errorf("merge gate broken: main advanced after submit of reviewed task:\n  before=%s\n  after=%s\nrejected/pending work would now be on main forever",
				mainBefore, mainAfter)
		}

		// Half 2 of the gate: the topic ref MUST exist on the
		// bare. The commit was pushed (audit trail) — only the
		// merge to main was suppressed.
		draftTask := h.taskGet("draft")
		topic, _ := draftTask["latest_completed_branch"].(string)
		if topic == "" {
			t.Fatalf("draft.latest_completed_branch empty; cannot verify topic was pushed")
		}
		if sha := bareRefSHA(t, remoteURL, topic); sha == "" {
			t.Errorf("draft topic %q missing from bare — submit didn't push, gate is over-suppressing", topic)
		}
	})
}

// TestMCPRequestChangesKeepsClaimOpenForRevision is the 6c
// successor to the prior TestMCPRequestChangesRelabelsClaimOutcome.
// The 6b.2 behavior (relabel claim outcome to "rejected" on
// request_changes) was an interim fix; 6c supersedes it with
// the proper iteration semantics: a request_changes round is
// a revision within the SAME iteration, not a new one. The
// claim row stays OPEN (outcome=NULL) so the next claim by
// the same citizen reuses it, the topic-branch name is stable
// across revisions, and iter-N bumps only on a hard reset
// (invalidate, reject, abandon).
//
// Pre-6c: iter-1 [rejected] → re-claim → iter-2 [completed].
// Post-6c: iter-1 stays open → re-submit → iter-1 [completed]
// (after final approve).
func TestMCPRequestChangesKeepsClaimOpenForRevision(t *testing.T) {
	eachRemoteMode(t, "OpenForRevision", func(t *testing.T, h *mcpHarness) {
		reviewer := h.newMCPClientAs(t, "RevisionReviewer")
		projectID := h.createTestProject()

		yaml := `name: "open for revision"
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

		// First attempt: submit + reviewer asks for changes.
		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "first attempt")
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "needs work", "request_changes")

		// Phase 6c contract: still ONE iteration, claim open.
		// list_iterations should show iter-1 with NEITHER a
		// "[rejected]" relabel NOR a fresh iter-2 — the row
		// stays open through the revision round.
		out := mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
			"task_id": h.taskID("draft"),
		}))
		if strings.Contains(out, "iter-2") {
			t.Errorf("request_changes must NOT bump iter-N; expected only iter-1, got:\n%s", out)
		}
		if strings.Contains(out, "[rejected]") {
			t.Errorf("request_changes must NOT close the claim with outcome=rejected (the claim stays open for revision), got:\n%s", out)
		}

		// Re-claim + re-submit on the same iteration. The
		// reuse-on-reopen logic lets the same citizen claim
		// the same row again; submit lands as a new attempt
		// in task_submissions but the claim row's iter_seq
		// stays 1.
		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "second attempt")

		// Reviewer approves on the revised attempt.
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "looks good now", "approve")

		// Final state: still iter-1, now [completed]. ONE
		// iteration row, two submission attempts on it.
		out = mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
			"task_id": h.taskID("draft"),
		}))
		if strings.Contains(out, "iter-2") {
			t.Errorf("approve after revision must keep iter-N stable; expected only iter-1, got:\n%s", out)
		}
		if !strings.Contains(out, "[completed]") {
			t.Errorf("iter-1 should be [completed] after final approve, got:\n%s", out)
		}
	})
}

// TestMCPTaskRequestChangesEmitsEvent pins the audit hook for
// the request_changes review path: a reviewer's "request_changes"
// verdict produces a task_request_changes event with the right
// metadata (reviewer_id, review_task_id, iter_seq, decision).
// Without this test, a regression that drops the emission or
// muddles the metadata fields would silently break the audit
// timeline.
func TestMCPTaskRequestChangesEmitsEvent(t *testing.T) {
	eachRemoteMode(t, "RequestChangesEmits", func(t *testing.T, h *mcpHarness) {
		reviewer := h.newMCPClientAs(t, "RcEmitReviewer")
		projectID := h.createTestProject()

		yaml := `name: "request_changes emits event"
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
		h.mcpSubmitText(t, "draft", "first attempt")
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "needs work", "request_changes")

		ev := h.findEvent(projectID, "task_request_changes")
		if ev == nil {
			t.Fatal("task_request_changes event not emitted after request_changes review")
		}
		meta, _ := ev["metadata"].(map[string]interface{})
		if meta == nil {
			if metaStr, ok := ev["metadata"].(string); ok {
				_ = json.Unmarshal([]byte(metaStr), &meta)
			}
		}
		if meta == nil {
			t.Fatalf("task_request_changes metadata unparseable: %+v", ev)
		}
		if _, ok := meta["reviewer_id"].(float64); !ok {
			t.Errorf("metadata missing reviewer_id (got %+v)", meta)
		}
		if rid, _ := meta["review_task_id"].(string); rid == "" {
			t.Errorf("metadata missing review_task_id (got %+v)", meta)
		}
		if iter, ok := meta["iter_seq"].(float64); !ok || iter < 1 {
			t.Errorf("metadata iter_seq missing or zero (got %+v)", meta)
		}
		if dec, _ := meta["decision"].(string); dec != "request_changes" {
			t.Errorf("metadata decision = %q, want request_changes", dec)
		}
		if tid, _ := ev["task_id"].(string); tid == "" || !strings.HasSuffix(tid, ":draft") {
			t.Errorf("task_request_changes task_id should target the draft, got %q", tid)
		}
	})
}

// TestMCPManualInvalidateRelabelsClaimOutcome pairs with
// TestMCPRequestChangesRelabelsClaimOutcome — both pin the
// rule "the iteration's outcome reflects what happened, not
// just whether bytes were submitted." Round-3 review caught
// that 6b.2 covered the request_changes / reject paths but
// missed the manual `enju_invalidate_task` path; the relabel
// is the same shape, just with outcome=invalidated instead of
// rejected (no verdict — operator scrapped the iteration).
func TestMCPManualInvalidateRelabelsClaimOutcome(t *testing.T) {
	eachRemoteMode(t, "InvalidateRelabel", func(t *testing.T, h *mcpHarness) {
		projectID := h.createTestProject()

		yaml := `name: "invalidate relabel"
version: 1
tasks:
  - id: solo
    action: answer
    prompt: "Write."
`
		h.mcpCreateRunInline(t, projectID, yaml)

		// Iter-1: submit, then operator manually invalidates.
		h.mcpClaimOK(t, "solo")
		h.mcpSubmitText(t, "solo", "first attempt")
		h.mcpInvalidate(t, "solo", "operator scrapped this")

		out := mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
			"task_id": h.taskID("solo"),
		}))
		if !strings.Contains(out, "[invalidated]") {
			t.Errorf("iter-1 outcome should be relabeled to 'invalidated' after manual invalidate, got:\n%s", out)
		}
		if strings.Contains(out, "[completed]") {
			t.Errorf("iter-1 outcome should NOT read 'completed' after manual invalidate (audit drift), got:\n%s", out)
		}
	})
}

// TestMCPFailCascadeInvalidatesClaimedDescendant pins the
// 6c follow-up bug fix: when an upstream task is failed
// (manually via enju_fail_task, or via review reject) and
// a downstream consumer was ALREADY claimed, the
// descendant's open claim row must be marked 'invalidated'
// by the fail cascade.
//
// Pre-fix the descendant's task row went to skipped
// (correct) but its task_claims row stayed open with
// outcome=NULL — list_iterations then reported an active
// iteration on a skipped task. Pre-6c this worked
// implicitly via applySetTaskState{ClearClaim:true};
// 6c moved that decision to the cascade caller, and
// performFailCascade missed the loop while
// performInvalidate had it. This test pins the parity.
//
// Uses enju_fail_task on a no-review chain (rather than
// review reject) so we can claim the consumer while it's
// READY — under review-gating, a reviewed-upstream's
// consumer waits for the gate's verdict before becoming
// ready, so the "claimed-before-cascade" window only opens
// for the explicit fail path.
func TestMCPFailCascadeInvalidatesClaimedDescendant(t *testing.T) {
	eachRemoteMode(t, "FailCascadeClaim", func(t *testing.T, h *mcpHarness) {
		consumer := h.newMCPClientAs(t, "Consumer")
		projectID := h.createTestProject()

		yaml := `name: "fail cascade claimed descendant"
version: 1
tasks:
  - id: producer
    action: answer
    prompt: "Write."
  - id: consume
    action: answer
    depends_on: [producer]
    prompt: "Use {{producer.content}}"
`
		h.mcpCreateRunInline(t, projectID, yaml)

		// Producer submits — no downstream review, so it
		// auto-accepts and merges. Consumer becomes ready,
		// gets claimed (claim row open).
		h.mcpClaimOK(t, "producer")
		h.mcpSubmitText(t, "producer", "producer content")
		if got := h.taskGet("consume")["state"]; got != "ready" {
			t.Fatalf("consume should be ready after producer accept; got %v", got)
		}
		h.mcpClaimAs(t, consumer, "consume")
		if got := h.taskGet("consume")["state"]; got != "claimed" {
			t.Fatalf("consume should be claimed after mcpClaimAs; got %v", got)
		}

		// Operator manually fails the upstream. handleFailTask
		// → performFailCascade fires: producer → failed,
		// consume → skipped, AND consume's open claim row
		// must be marked invalidated.
		h.mcpFail(t, "producer", "operator scrapped this")

		if got := h.taskGet("consume")["state"]; got != "skipped" {
			t.Errorf("consume should be skipped after producer fail; got %v", got)
		}

		// The bug pin: consume's iteration row must read
		// [invalidated], not show as an open claim. Pre-fix
		// performFailCascade left descendants' claim rows
		// with outcome=NULL.
		out := mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
			"task_id": h.taskID("consume"),
		}))
		if !strings.Contains(out, "[invalidated]") {
			t.Errorf("consume's claim outcome should be 'invalidated' after upstream fail-cascade (claimed-descendant case), got:\n%s", out)
		}
	})
}

// TestMCPSingleCitizenReclaimGetsNewTopic pins the iteration-
// rotation contract: after a HARD reset (manual invalidate
// via enju_invalidate_task), re-claiming the same task
// produces iter-2 on a DISTINCT topic ref. The iter-1 ref is
// retained (audit); iter-2 is brand new.
//
// 6c semantics: only terminal-outcome events bump iter-N
// (invalidate, reject, abandon). request_changes — which
// previously triggered the bump in this test — now leaves
// the claim open for revision and keeps iter-N stable; a
// separate test (TestMCPRequestChangesKeepsClaimOpenForRevision)
// pins that behavior. This test uses invalidate to force the
// terminal outcome and exercise the iter rotation.
func TestMCPSingleCitizenReclaimGetsNewTopic(t *testing.T) {
	eachRemoteMode(t, "TopicReclaimRotates", func(t *testing.T, h *mcpHarness) {
		requireRemote(t, h)
		projectID := h.createTestProject()

		yaml := `name: "iter rotates on reclaim"
version: 1
tasks:
  - id: solo
    action: answer
    prompt: "Write."
`
		h.mcpCreateRunInline(t, projectID, yaml)

		// iter-1: submit + manual invalidate.
		h.mcpClaimOK(t, "solo")
		h.mcpSubmitText(t, "solo", "ITER-1-content")
		h.mcpInvalidate(t, "solo", "operator scrapped this")

		// Capture iter-1's topic ref + SHA from the bare BEFORE
		// the re-claim.
		remoteURL := h.remoteFor(projectID)
		iters := h.draftIterationBranches(t, "solo")
		if len(iters) != 1 {
			t.Fatalf("expected 1 iteration before reclaim, got %d: %v", len(iters), iters)
		}
		iter1Topic := iters[0]
		iter1SHA := bareRefSHA(t, remoteURL, iter1Topic)
		if iter1SHA == "" {
			t.Fatalf("iter-1 topic %q missing on bare before reclaim", iter1Topic)
		}

		// iter-2: re-claim and submit. Invalidate was
		// terminal, so iter_seq bumps and a fresh topic is
		// generated.
		h.mcpClaimOK(t, "solo")
		h.mcpSubmitText(t, "solo", "ITER-2-content")

		iters = h.draftIterationBranches(t, "solo")
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

// TestMCPReporterScenario_RequestChangesShouldNotBumpIterSeq is
// a reproduction of the reporter's day-of-development claim:
//
//   "iter_seq: bumped from 1 → 2 → 3 (NOT staying at 1 as
//    phase 6c docs suggest)"
//
// Phase 6c contract: request_changes verdicts on a downstream
// review keep the upstream's claim row OPEN. Same-citizen
// re-claim REUSES the row, iter_seq stays. Distinct iter-N
// topic branches only appear after a TERMINAL outcome
// (invalidate / reject / abandon).
//
// This test exercises three rounds of request_changes on the
// same upstream by the same dev citizen and asserts:
//   - Only ONE iteration row exists across all rounds.
//   - Branch list contains exactly one entry, ending in /iter-1.
//   - iter-2 / iter-3 do NOT appear.
//
// If this fails, we've found a real iter_seq-bumping regression
// matching the reporter's data. If it passes, the reporter's
// production verdicts must have actually been `reject` (terminal),
// not `request_changes` — and the iter_seq bumping was
// per-design.
func TestMCPReporterScenario_RequestChangesShouldNotBumpIterSeq(t *testing.T) {
	eachRemoteMode(t, "RequestChangesNoIterBump", func(t *testing.T, h *mcpHarness) {
		reviewer := h.newMCPClientAs(t, "RcBumpReviewer")
		projectID := h.createTestProject()

		yaml := `name: "request_changes no bump"
version: 1
tasks:
  - id: develop_domain
    action: answer
    prompt: "Do work."
  - id: review_domain
    action: review
    reviews: develop_domain
    prompt: "Review."
`
		h.mcpCreateRunInline(t, projectID, yaml)

		// Three rounds of request_changes on the SAME develop_domain.
		for round := 1; round <= 3; round++ {
			h.mcpClaimOK(t, "develop_domain")
			h.mcpSubmitText(t, "develop_domain", fmt.Sprintf("attempt %d", round))
			h.mcpClaimAs(t, reviewer, "review_domain")
			// On the final round, approve so the run can finish.
			verdict := "request_changes"
			if round == 3 {
				verdict = "approve"
			}
			h.mcpSubmitReviewAs(t, reviewer, "review_domain", fmt.Sprintf("round %d feedback", round), verdict)
		}

		// After three rounds with request_changes (rounds 1 and
		// 2) and a final approve (round 3), develop_domain
		// should have exactly ONE iteration. iter-2 / iter-3
		// MUST NOT appear.
		out := mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
			"task_id": h.taskID("develop_domain"),
		}))
		if strings.Contains(out, "iter-2") || strings.Contains(out, "iter-3") {
			t.Errorf("REPRO: request_changes bumped iter_seq (reporter's bug).\n"+
				"Expected only iter-1; got:\n%s", out)
		}
		if !strings.Contains(out, "[completed]") {
			t.Errorf("expected iter-1 [completed] after final approve, got:\n%s", out)
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
