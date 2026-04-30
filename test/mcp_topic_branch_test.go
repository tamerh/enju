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

// TestMCPRequestChangesRelabelsClaimOutcome pins ISSUE-003 from
// the v1 sanity report: when a review request_changes / reject
// fires, the rejected iteration's claim row outcome must be
// relabeled from "completed" (its row-level state when the
// citizen submitted) to "rejected" (the verdict it actually
// received). Without this relabel, every iteration in
// list_iterations reads "completed" regardless of what really
// happened, destroying the audit trail.
func TestMCPRequestChangesRelabelsClaimOutcome(t *testing.T) {
	eachRemoteMode(t, "OutcomeRelabel", func(t *testing.T, h *mcpHarness) {
		reviewer := h.newMCPClientAs(t, "OutcomeReviewer")
		projectID := h.createTestProject()

		yaml := `name: "outcome relabel"
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

		// Iter-1: submit draft → request_changes.
		h.mcpClaimOK(t, "draft")
		h.mcpSubmitText(t, "draft", "first attempt")
		h.mcpClaimAs(t, reviewer, "gate")
		h.mcpSubmitReviewAs(t, reviewer, "gate", "needs work", "request_changes")

		// list_iterations should now show iter-1 with
		// outcome=rejected, NOT outcome=completed. Read the
		// rendered output (the table format is
		// "iter-N  @user  [<outcome>]").
		out := mcpText(h.callOK(t, "enju_list_iterations", map[string]any{
			"task_id": h.taskID("draft"),
		}))
		if !strings.Contains(out, "[rejected]") {
			t.Errorf("iter-1 outcome should be relabeled to 'rejected' after request_changes, got:\n%s", out)
		}
		if strings.Contains(out, "[completed]") {
			t.Errorf("iter-1 outcome should NOT read 'completed' after request_changes (audit drift), got:\n%s", out)
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
