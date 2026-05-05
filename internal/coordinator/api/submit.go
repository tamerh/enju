package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

// submitResultRequest is the iteration A shape: the client has
// already written the result + artifact files to its local clone,
// committed atomically, and pushed to the project's remote. The
// coordinator only receives metadata — no file content crosses the
// wire.
type submitResultRequest struct {
	// CommitSHA identifies the commit the client pushed to the
	// project's remote. Required.
	CommitSHA string `json:"commit_sha"`
	// ResultPath is the repo-relative directory holding this
	// task's result files. Must match the expected layout for the
	// task (runs/{seq}/{instance_key}/{task_def_id}) — the
	// coordinator validates this.
	ResultPath string `json:"result_path"`
	// ArtifactsWritten lists the user-facing artifact paths the
	// client wrote in the same commit. All share CommitSHA.
	ArtifactsWritten []string `json:"artifacts_written,omitempty"`

	TokensUsed int64 `json:"tokens_used,omitempty"`
	Model   string `json:"model,omitempty"`

	// Username identifies the submitting citizen. Required for
	// multi-citizen tasks so the server can credit the right
	// task_claims slot; optional for single-citizen tasks where
	// tasks.claimed_by is already the implicit claimer.
	Username string `json:"username,omitempty"`

	// Decision is the review verdict for action:review tasks. One
	// of "approve" / "reject". Ignored on non-review tasks. An empty
	// string on a review task is rejected up front — the reviewer
	// has to say something.
	Decision string `json:"decision,omitempty"`

	// Option is the chosen option id for action:vote tasks. Must
	// match one of the declared options' ids. Ignored on
	// non-vote tasks. Session 1 ships single-voter vote so one
	// submit resolves the task; session 2 multi-voter will tally
	// across N submissions.
	Option string `json:"option,omitempty"`

	// Content is the reviewer's prose for the narrow
	// {{review.feedback}} substitution path inside
	// maybeSpawnRemediation — the only place coord still reads
	// submission text. NOT persisted on task_claims (column
	// dropped per ARCHITECTURE.md #3); the rendered remediation
	// prompt does end up in the new task's tasks.prompt row,
	// which is task definition and part of the metadata-on-
	// coord surface tracked in TODO under "metadata privacy
	// gap". For multi-citizen vote/review fan-in, downstream
	// resolvers read each citizen's result.md from git via
	// commit_sha — coord never sees the prose.
	Content string `json:"content,omitempty"`

	// OutputLists carries the *values* of named outputs that
	// are declared as format: list<string> on the submitting
	// task. Populated by the fat client at submit time so the
	// coordinator can use them for dynamic for_each
	// materialization (Phase J.1) without having to read git.
	// Keyed by output field name.
	//
	// Other named-output values (plain strings) stay in the
	// task's git-committed result files — only list<string>
	// outputs need to round-trip through the coordinator.
	OutputLists map[string][]string `json:"output_lists,omitempty"`
}

// handleSubmitResult is the metadata-only submit path. The client
// has already done the git work; the coordinator just validates the
// report, updates the state machine, updates the artifact index,
// runs the scheduler, and checks run completion.
//
// This is coordinator protocol, not a bot SDK — the wire format
// fat clients use to report a completed submission. There is no
// coordinator-side git worker (trust-the-client); bots calling
// this directly take on the fat client's git responsibilities.
//
// Per-action contract:
//
//  - compute / answer / contribute → commit_sha REQUIRED. The
//   400 fires below if missing. Submission lives in git
//   (metadata.json + result.md + writes_artifacts paths) and
//   the DB stores commit_sha + result_path as the pointer.
//
//  - vote / review → commit_sha OPTIONAL today (TODO: tighten
//   to required, see Phase 3.2 followups). The state machine,
//   scheduler, and tally engine read from
//   tasks.vote_choice / review_decision and the per-citizen
//   task_claims rows (decision, option, commit_sha). Multi-
//   citizen fan-in for `{{task.responses}}` reads each
//   citizen's result.md from git via commit_sha — coord no
//   longer stores prose content for any submission.
//
// See docs/coordinator.md § REST API § Tasks for the full
// per-action table and the two-tier (DB-mutable, git-immutable)
// rationale.
func (s *Server) handleSubmitResult(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req submitResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Task existence check FIRST — otherwise a submit to a
	// deleted/wiped task falls through the commit_sha validator
	// and surfaces "commit_sha is required" as if that were the
	// root cause.
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("task %q not found", taskID))
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}
	// Refuse late-arriving submits on terminated runs. The
	// fat-client compute that ran when terminate fired may
	// reach this endpoint after the cascade has already
	// skipped the task. Topic branch may exist in git, but
	// no commit lands on main and no coord state changes —
	// the work is honestly lost. Friendly error so the
	// fat-client can log and stop retrying.
	if reason, ok := s.runTerminatedRefusal(task); ok {
		writeError(w, http.StatusConflict, reason)
		return
	}

	// Claim validity check comes next — a submit from someone
	// who never claimed the task has no legitimate path, and
	// reporting "commit_sha is required" would hide the actual
	// "you don't own a slot on this task" error. For
	// single-citizen tasks the claimant is tasks.claimed_by; for
	// multi-citizen tasks it's any row in task_claims for the
	// submitting username.
	if task.Citizens > 1 {
		if req.Username == "" {
			writeError(w, http.StatusBadRequest, "username is required on multi-citizen task submissions")
			return
		}
		citizen, cerr := s.store.GetCitizenByUsername(req.Username)
		if cerr != nil || citizen == nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown citizen %q", req.Username))
			return
		}
		hasClaim, _ := s.store.HasActiveClaim(taskID, citizen.ID)
		if !hasClaim {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no open claim on task %q for user %q — claim it with enju_claim_task first", taskID, req.Username))
			return
		}
	} else if task.ClaimedBy == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("task %q has no active claimant — claim it with enju_claim_task first", taskID))
		return
	}

	// commit_sha is required for action types that actually
	// ship prose/code/data to git (answer / contribute /
	// compute). Vote and review are decisions — the
	// authoritative storage for their submissions is the
	// task_claims row (decision/option/commit_sha pointer), not a git file's content.
	// Making commit_sha optional for vote/review removes a
	// class of ordering bugs and matches the tools' real
	// contracts: "git is how tasks ship their work; votes and
	// reviews have nothing to ship."
	if req.CommitSHA == "" && task.Action != "vote" && task.Action != "review" {
		writeError(w, http.StatusBadRequest, "commit_sha is required — the coordinator no longer writes result files, clients must write + push + report")
		return
	}
	// Shape-level validation on commit_sha. Trust-the-client
	// architecture says we don't fetch the commit to verify it
	// exists on the remote (that's an optional future mode —
	// see ARCHITECTURE.md principle 7 + Open Question #4). But
	// even under trust-the-client, a commit_sha of the wrong
	// SHAPE is always a client bug — a buggy client sending
	// "not-a-sha" corrupts the artifact index for its own
	// project and makes downstream template resolution fail
	// mysteriously. Shape-check catches that cheaply. Accept
	// both SHA-1 (40 hex) and SHA-256 (64 hex) lengths so the
	// check doesn't block git's future hash transition.
	if req.CommitSHA != "" && !service.IsValidCommitSHAShape(req.CommitSHA) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("commit_sha %q is not a valid git SHA (expected 40 or 64 hex characters)", req.CommitSHA))
		return
	}

	if task.Action == "vote" || task.Action == "review" {
		passed, derr := s.engine().DeadlinePassed(task)
		if derr == nil && passed {
			writeError(w, http.StatusConflict, fmt.Sprintf("task %q voting deadline has expired — submission rejected, run enju_tally_task to resolve", taskID))
			return
		}
	}

	s.handleSubmitResultReport(w, r, task, &req)
}

// handleSubmitResultReport is the client-writes submit path
// (iteration A.2). The client has already written result files +
// artifacts and pushed them to the project's remote. We just update
// metadata: result_path, commit_sha, state machine, artifact index,
// scheduler re-evaluation, run completion. No git operations here.
func (s *Server) handleSubmitResultReport(w http.ResponseWriter, r *http.Request, task *store.TaskRecord, req *submitResultRequest) {
	resp, err := s.coord.SubmitTaskResult(task, service.SubmitResultParams{
		CommitSHA:    req.CommitSHA,
		ResultPath:    req.ResultPath,
		ArtifactsWritten: req.ArtifactsWritten,
		TokensUsed:    req.TokensUsed,
		Model:      req.Model,
		Username:     req.Username,
		Decision:     req.Decision,
		Option:      req.Option,
		Content:     req.Content,
		OutputLists:   req.OutputLists,
	})
	if err != nil {
		// Validation errors land as 400; anything else (apply
		// plan failures) as 500. The core preserves the same
		// split the old monolithic handler enforced —
		// "applying submit plan" is the only error form that
		// wraps its underlying cause.
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "applying submit plan:") {
			status = http.StatusInternalServerError
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
