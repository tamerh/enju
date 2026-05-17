package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/common/wire"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TaskResponse is the canonical wire shape for one task. Used
// by REST (writeJSON), MCP, and future Web UI. Field tags are
// load-bearing — formatters (format.RunStatus,
// format.ReadyTasks, etc.) consume these key names.
//
// This is the BIG response type — ~50 fields covering task
// definition, runtime state, claim metadata, vote/review
// submissions, iteration branches, artifact provenance, and
// task history. Every field has a well-defined reason; see the
// inline comments alongside the api.taskResponse origin if
// uncertain about any of them.
type TaskResponse struct {
	ID                       string                 `json:"id"`
	RunID                    int64                  `json:"run_id"`
	RunSeq                   int                    `json:"run_seq"`
	ProjectID                int64                  `json:"project_id"`
	ProjectRemoteURL         string                 `json:"project_remote_url,omitempty"`
	ProjectName              string                 `json:"project_name,omitempty"`
	Seq                      int                    `json:"seq"`
	TaskDefID                string                 `json:"task_def_id"`
	InstanceKey              string                 `json:"instance_key,omitempty"`
	IterationLabel           string                 `json:"iteration_label,omitempty"`
	ResultDir                string                 `json:"result_dir,omitempty"`
	RunSlug                  string                 `json:"run_slug,omitempty"`
	Ref                      string                 `json:"ref,omitempty"`
	Action                   string                 `json:"action"`
	Prompt                   string                 `json:"prompt,omitempty"`
	UserPrompt               string                 `json:"user_prompt,omitempty"`
	Script                   string                 `json:"script,omitempty"`
	Outputs                  string                 `json:"outputs,omitempty"`
	Requirements             string                 `json:"requirements,omitempty"`
	ResultType               string                 `json:"result_type"`
	State                    string                 `json:"state"`
	ClaimedBy                string                 `json:"claimed_by,omitempty"`
	Model                    string                 `json:"model,omitempty"`
	ResultPath               string                 `json:"result_path,omitempty"`
	CommitSHA                string                 `json:"commit_sha,omitempty"`
	DependsOn                string                 `json:"depends_on,omitempty"`
	ReadsArtifacts           []string               `json:"reads_artifacts,omitempty"`
	WritesArtifacts          enjuYaml.WriteArtifacts `json:"writes_artifacts,omitempty"`
	AssignTo                 []string               `json:"assign_to,omitempty"`
	RequireRole              string                 `json:"require_role,omitempty"`
	ReviewsTarget            string                 `json:"reviews_target,omitempty"`
	ReviewDecision           string                 `json:"review_decision,omitempty"`
	VoteOptions              string                 `json:"vote_options,omitempty"`
	VoteChoice               string                 `json:"vote_choice,omitempty"`
	Citizens                 int                    `json:"citizens,omitempty"`
	MinQuorum                int                    `json:"min_quorum,omitempty"`
	VoteThreshold            string                 `json:"vote_threshold,omitempty"`
	VoteDeadline             string                 `json:"vote_deadline,omitempty"`
	VoteDeadlineAt           string                 `json:"vote_deadline_at,omitempty"`
	Anonymize                bool                   `json:"anonymize,omitempty"`
	Visibility               string                 `json:"visibility,omitempty"`
	FailReason               string                 `json:"fail_reason,omitempty"`
	SkipReason               string                 `json:"skip_reason,omitempty"`
	ParkedFromState          string                 `json:"parked_from_state,omitempty"`
	RunSourcePath            string                 `json:"run_source_path,omitempty"`
	// RunSourceCommitSHA is the run's pinned base commit (project
	// HEAD at create_run). Denormalized here alongside RunBranch so
	// the fat-client can validate that an iteration branch belongs
	// to THIS run — iter-branch names collide across runs that
	// share a slug, and a same-named ref from a prior run does not
	// descend from this run's base. Empty for inline-yaml runs.
	RunSourceCommitSHA       string                 `json:"run_source_commit_sha,omitempty"`
	RunBranch                string                 `json:"run_branch,omitempty"`
	RunParams                map[string]interface{} `json:"run_params,omitempty"`
	InstanceParamsMap        map[string]interface{} `json:"instance_params_map,omitempty"`
	Env                      map[string]string      `json:"env,omitempty"`
	Mode                     string                 `json:"mode,omitempty"`
	Container                string                 `json:"container,omitempty"`
	ContainerRuntime         string                 `json:"container_runtime,omitempty"`
	Volumes                  []string               `json:"volumes,omitempty"`
	Executor                 string                 `json:"executor,omitempty"`
	Resources                *enjuYaml.Resources    `json:"resources,omitempty"`
	VoteSubmissions          []VoteSubmissionRef    `json:"vote_submissions,omitempty"`
	ActiveClaimants          []string               `json:"active_claimants,omitempty"`
	IterationBranches        map[string]string      `json:"iteration_branches,omitempty"`
	IterationSeqs            map[string]int         `json:"iteration_seqs,omitempty"`
	ArtifactProvenance       []ArtifactProvenance   `json:"artifact_provenance,omitempty"`
	TaskHistory              []TaskHistoryEntry     `json:"task_history,omitempty"`
	PreviousIterationCommit  string                 `json:"previous_iteration_commit,omitempty"`
	UpstreamIterationBranch  string                 `json:"upstream_iteration_branch,omitempty"`
	LatestCompletedCommitSHA string                 `json:"latest_completed_commit_sha,omitempty"`
	LatestCompletedBranch    string                 `json:"latest_completed_branch,omitempty"`

	// IterCount is the number of distinct iter_seq values on
	// this task's task_claims rows — i.e. how many accept-
	// cycles it's been through. 1 for a single-attempt task,
	// > 1 for tasks that bounced through request_changes /
	// invalidate / re-claim. Surface readers (enju_run_status
	// Iter column, Mermaid (N×) badge) gate on >1 so the
	// common case stays uncluttered. Phase 8.6.
	IterCount int `json:"iter_count,omitempty"`

	// Iterations is the per-iteration projection embedded on
	// the task response so callers don't need a follow-up
	// /tasks/{id}/iterations roundtrip when rendering history.
	// Same shape as the list endpoint returns. Phase 8.6
	// suppresses this when IterCount <= 1 (single-attempt
	// tasks have nothing interesting to render).
	Iterations []wire.Iteration `json:"iterations,omitempty"`
}

// TaskHistoryEntry is one row of a task's claim/submission
// history, surfaced when the task has been re-claimed after
// invalidation.
type TaskHistoryEntry struct {
	Citizen     string `json:"citizen"`
	ClaimedAt   string `json:"claimed_at"`
	SubmittedAt string `json:"submitted_at,omitempty"`
	Outcome     string `json:"outcome"`
	Decision    string `json:"decision,omitempty"`
}

// ArtifactProvenance is one entry per artifact this task
// reads, with the last writer info pulled from the artifact
// index. Surfaced so reviewers can see who produced an upstream
// without a separate fetch.
type ArtifactProvenance struct {
	Path       string `json:"path"`
	LastWriter string `json:"last_writer,omitempty"`
	LastTaskID string `json:"last_task_id,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"`
}

// VoteSubmissionRef is one citizen's vote on a multi-citizen
// task, included in TaskResponse so the formatter can render
// the tally without a separate fetch.
type VoteSubmissionRef struct {
	Username    string `json:"username"`
	Option      string `json:"option"`
	SubmittedAt string `json:"submitted_at,omitempty"`
	Model       string `json:"model,omitempty"`
}

// UnmarshalStringSlice parses the storage form (JSON-encoded
// []string) back to a Go slice. An empty string yields nil
// (no entries). Defense in depth — a corrupted row yields nil
// rather than crashing the caller.
func UnmarshalStringSlice(s string) []string {
	if s == "" {
		return nil
	}
	var xs []string
	if err := json.Unmarshal([]byte(s), &xs); err != nil {
		return nil
	}
	return xs
}

// UnmarshalWriteArtifacts parses the storage form of the
// writes_artifacts column (legacy []string OR current
// [{path,track}] form — yaml.WriteArtifacts.UnmarshalJSON
// handles both).
func UnmarshalWriteArtifacts(s string) enjuYaml.WriteArtifacts {
	if s == "" {
		return nil
	}
	var w enjuYaml.WriteArtifacts
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return nil
	}
	return w
}

// UnmarshalResources decodes the JSON-encoded SLURM ask stored
// in tasks.resources (marshalResources in engine/materialize.go
// is the inverse). Returns nil for the empty/clean-default case
// and for a corrupt row (a bad resources blob shouldn't fail a
// task lookup) — callers treat nil as "no ask, SLURM defaults".
// A zero-but-present struct also collapses to nil so the wire
// stays terse.
func UnmarshalResources(s string) *enjuYaml.Resources {
	if s == "" {
		return nil
	}
	var r enjuYaml.Resources
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil
	}
	if r.IsZero() {
		return nil
	}
	return &r
}

// UnmarshalStringMapField decodes the JSON-encoded env: map
// stored on tasks.env back into a map[string]string. Empty
// string yields nil. Malformed JSON yields nil.
func UnmarshalStringMapField(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// FormatIterationLabel renders a task's iteration context as
// "key1=val1, key2=val2" using the persisted instance_params
// JSON when available, falling back to the raw instance key
// slug for rows that predate the instance_params column. Keys
// are sorted so output is deterministic.
func FormatIterationLabel(instanceParams, instanceKey string) string {
	if instanceParams == "" {
		return instanceKey
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(instanceParams), &m); err != nil || len(m) == 0 {
		return instanceKey
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		// `<var>__<field>` keys are the list<record> env-var
		// expansion (ENJU_PARAM_<var>__<field>), not iteration
		// identity — the bare `<var>` key already names the
		// instance. Hide them so the label stays "<var>=<key>"
		// instead of dumping every record field.
		if strings.Contains(k, "__") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return instanceKey
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

// ParseReviewsTargetForMerge splits a reviews_target string
// (e.g. "instance:task_def_id" or just "task_def_id") into
// (taskDefID, instanceKey). Used by ToTaskResponse and the
// review merge gate.
func ParseReviewsTargetForMerge(target string) (taskDefID, instanceKey string) {
	if idx := strings.Index(target, ":"); idx > 0 {
		return target[idx+1:], target[:idx]
	}
	return target, ""
}

// ToTaskResponse builds the canonical TaskResponse for one
// task. Performs N+1 store reads as needed (run, project,
// citizen lookups, vote submissions, claim history, artifact
// provenance) — gated to keep the cost manageable on
// run-listing endpoints (e.g. model lookup only fires on
// state=accepted single-citizen tasks).
//
// Wire shape is preserved from api.toTaskResponse exactly so
// no caller observes a change.
func ToTaskResponse(s store.CoordinatorStore, t store.TaskRecord) TaskResponse {
	var (
		projectID     int64
		runSeq        int
		remoteURL     string
		projectName   string
		runSourcePath      string
		runSourceCommitSHA string
		runBranch          string
		runParams          map[string]interface{}
	)
	if run, _ := s.GetRun(t.RunID); run != nil {
		projectID = run.ProjectID
		runSeq = run.Seq
		runSourcePath = run.SourcePath
		runSourceCommitSHA = run.SourceCommitSHA
		runBranch = run.Branch
		if run.Params != "" {
			_ = json.Unmarshal([]byte(run.Params), &runParams)
		}
		if p, _ := s.GetProject(projectID); p != nil {
			remoteURL = p.RemoteURL
			projectName = p.Name
		}
	}
	var instanceParams map[string]interface{}
	if t.InstanceParams != "" {
		_ = json.Unmarshal([]byte(t.InstanceParams), &instanceParams)
	}
	iterationLabel := ""
	if t.InstanceKey != "" {
		iterationLabel = FormatIterationLabel(t.InstanceParams, t.InstanceKey)
	}
	resp := TaskResponse{
		ID:                t.ID,
		RunID:             t.RunID,
		RunSeq:            runSeq,
		ProjectID:         projectID,
		ProjectRemoteURL:  remoteURL,
		ProjectName:       projectName,
		Seq:               t.Seq,
		TaskDefID:         t.TaskDefID,
		InstanceKey:       t.InstanceKey,
		ResultDir:         engine.ComputeResultDir(&t),
		RunSlug:           t.RunSlug,
		IterationLabel:    iterationLabel,
		Ref:               t.Ref,
		Action:            t.Action,
		Prompt:            t.Prompt,
		UserPrompt:        t.UserPrompt,
		Script:            t.Script,
		Outputs:           t.Outputs,
		Requirements:      t.Requirements,
		ResultType:        t.ResultType,
		State:             string(t.State),
		ClaimedBy:         CitizenUsername(s, t.ClaimedBy),
		ResultPath:        t.ResultPath,
		CommitSHA:         t.CommitSHA,
		DependsOn:         t.DependsOn,
		ReadsArtifacts:    UnmarshalStringSlice(t.ReadsArtifacts),
		WritesArtifacts:   UnmarshalWriteArtifacts(t.WritesArtifacts),
		AssignTo:          UnmarshalStringSlice(t.AssignTo),
		RequireRole:       t.RequireRole,
		ReviewsTarget:     t.ReviewsTarget,
		ReviewDecision:    string(t.ReviewDecision),
		VoteOptions:       t.VoteOptions,
		VoteChoice:        t.VoteChoice,
		Citizens:          t.Citizens,
		MinQuorum:         t.MinQuorum,
		VoteThreshold:     t.VoteThreshold,
		VoteDeadline:      t.VoteDeadline,
		Anonymize:         t.Anonymize,
		Visibility:        t.Visibility,
		FailReason:        t.FailReason,
		SkipReason:        t.SkipReason,
		ParkedFromState:   t.ParkedFromState,
		RunSourcePath:      runSourcePath,
		RunSourceCommitSHA: runSourceCommitSHA,
		RunBranch:          runBranch,
		RunParams:         runParams,
		InstanceParamsMap: instanceParams,
		Env:               UnmarshalStringMapField(t.Env),
		Mode:              t.Mode,
		Container:         t.Container,
		ContainerRuntime:  t.ContainerRuntime,
		Volumes:           UnmarshalStringSlice(t.Volumes),
		Executor:          t.Executor,
		Resources:         UnmarshalResources(t.Resources),
	}

	// Single-citizen task model attribution. Gate on state ==
	// accepted to avoid an N+1 task_claims query on every
	// 100-task run-listing endpoint. Take the LAST element of
	// ListVoteSubmissions: invalidation leaves stale 'completed'
	// rows at index 0; the freshly-submitted model lives at
	// subs[len-1].
	if t.Citizens <= 1 && store.TaskState(t.State) == store.TaskAccepted {
		if subs, err := s.ListVoteSubmissions(t.ID); err == nil && len(subs) > 0 {
			latest := subs[len(subs)-1]
			resp.Model = latest.Model
		}
	}

	// Per-citizen claim/submission state for multi-citizen
	// vote AND review tasks.
	if (t.Action == "vote" || t.Action == "review") && t.Citizens > 1 {
		if t.VoteDeadline != "" {
			if d, derr := time.ParseDuration(t.VoteDeadline); derr == nil {
				if first, ferr := s.EarliestClaimTime(t.ID); ferr == nil && !first.IsZero() {
					resp.VoteDeadlineAt = first.Add(d).Format(time.RFC3339)
				}
			}
		}
		if submissions, err := s.ListVoteSubmissions(t.ID); err == nil {
			for idx, sub := range submissions {
				uname := CitizenUsername(s, sub.CitizenID)
				if t.Anonymize {
					uname = fmt.Sprintf("citizen-%d", idx+1)
				}
				ref := VoteSubmissionRef{
					Username: uname,
					Option:   sub.Option,
				}
				if sub.SubmittedAt != nil {
					ref.SubmittedAt = sub.SubmittedAt.Format(time.RFC3339)
				}
				ref.Model = sub.Model
				resp.VoteSubmissions = append(resp.VoteSubmissions, ref)
			}
		}
		if claims, err := s.ListActiveClaims(t.ID); err == nil {
			for idx, c := range claims {
				uname := CitizenUsername(s, c.CitizenID)
				if uname == "" {
					continue
				}
				if t.Anonymize {
					uname = fmt.Sprintf("active-citizen-%d", idx+1)
				}
				resp.ActiveClaimants = append(resp.ActiveClaimants, uname)
				if c.Branch != "" {
					if resp.IterationBranches == nil {
						resp.IterationBranches = map[string]string{}
					}
					resp.IterationBranches[uname] = c.Branch
				}
				if c.IterSeq > 0 {
					if resp.IterationSeqs == nil {
						resp.IterationSeqs = map[string]int{}
					}
					resp.IterationSeqs[uname] = c.IterSeq
				}
			}
		}
	}

	// upstream_iteration_branch for action:review tasks.
	if t.Action == "review" && t.ReviewsTarget != "" {
		targetDef, targetInstance := ParseReviewsTargetForMerge(t.ReviewsTarget)
		runTasks, _ := s.ListTasksByRun(t.RunID)
		for _, ut := range runTasks {
			if ut.TaskDefID != targetDef || ut.InstanceKey != targetInstance {
				continue
			}
			hist, _ := s.ListTaskHistory(ut.ID)
			for i := len(hist) - 1; i >= 0; i-- {
				if hist[i].Branch != "" {
					resp.UpstreamIterationBranch = hist[i].Branch
					break
				}
			}
			break
		}
	}

	// previous_iteration_commit + latest_completed_commit_sha
	// scan: walk the task's claim history newest-first, keep
	// the most recent commit-bearing claim.
	if hist, err := s.ListTaskHistory(t.ID); err == nil {
		for i := len(hist) - 1; i >= 0; i-- {
			c := hist[i]
			if c.CommitSHA == "" {
				continue
			}
			if resp.LatestCompletedCommitSHA == "" {
				resp.LatestCompletedCommitSHA = c.CommitSHA
				resp.LatestCompletedBranch = c.Branch
			}
			if resp.PreviousIterationCommit == "" {
				resp.PreviousIterationCommit = c.CommitSHA
			}
			if resp.LatestCompletedCommitSHA != "" && resp.PreviousIterationCommit != "" {
				break
			}
		}
	}

	// Per-citizen iteration_branches for tasks with active
	// claims that DON'T match the multi-citizen path above
	// (single-citizen action:answer).
	if resp.IterationBranches == nil && !t.Anonymize {
		if claims, err := s.ListActiveClaims(t.ID); err == nil {
			for _, c := range claims {
				if c.Branch == "" {
					continue
				}
				uname := CitizenUsername(s, c.CitizenID)
				if uname == "" {
					continue
				}
				if resp.IterationBranches == nil {
					resp.IterationBranches = map[string]string{}
				}
				resp.IterationBranches[uname] = c.Branch
				if c.IterSeq > 0 {
					if resp.IterationSeqs == nil {
						resp.IterationSeqs = map[string]int{}
					}
					resp.IterationSeqs[uname] = c.IterSeq
				}
			}
		}
	}

	// Task history (only when there are multiple claims —
	// single-claim tasks skip to avoid noise).
	if history, err := s.ListTaskHistory(t.ID); err == nil && len(history) > 1 {
		for _, h := range history {
			entry := TaskHistoryEntry{
				Citizen:   CitizenUsername(s, h.CitizenID),
				ClaimedAt: h.ClaimedAt.Format(time.RFC3339),
				Outcome:   string(h.Outcome),
				Decision:  h.Option,
			}
			if h.SubmittedAt != nil {
				entry.SubmittedAt = h.SubmittedAt.Format(time.RFC3339)
			}
			resp.TaskHistory = append(resp.TaskHistory, entry)
		}
	}

	// Phase 8.6 — iteration projection. iter_count = the
	// task's accept-cycle count (max iter_seq from
	// task_claims); the embedded iterations[] gives a
	// per-cycle history so callers don't need a follow-up
	// /tasks/{id}/iterations roundtrip. Both suppress on
	// single-attempt tasks (iter_count == 1) so the common
	// case stays uncluttered.
	if iters, err := s.ListTaskIterations(t.ID); err == nil && len(iters) > 0 {
		distinct := map[int]bool{}
		for _, it := range iters {
			distinct[it.IterSeq] = true
		}
		// Fall back to the row count when iter_seq is
		// uniformly zero (legacy rows pre-Phase-6c migration);
		// matches the post-migration semantics of "one
		// task_claims row per iteration."
		count := len(distinct)
		if len(distinct) == 1 {
			for k := range distinct {
				if k == 0 {
					count = len(iters)
				}
			}
		}
		if count > 1 {
			resp.IterCount = count
			for _, it := range iters {
				row := wire.Iteration{
					Seq:            it.Seq,
					Citizen:        it.Username,
					ClaimedAt:      it.ClaimedAt.UTC(),
					CommitSHA:      it.CommitSHA,
					Branch:         it.Branch,
					ReviewDecision: string(it.ReviewDecision),
					Option:         it.Option,
				}
				if it.Outcome == "" {
					row.Outcome = "active"
				} else {
					row.Outcome = string(it.Outcome)
				}
				if it.SubmittedAt != nil {
					ts := it.SubmittedAt.UTC()
					row.SubmittedAt = &ts
					if d := ts.Sub(it.ClaimedAt.UTC()).Milliseconds(); d > 0 {
						row.DurationMS = d
					}
				}
				row.Model = it.Model
				resp.Iterations = append(resp.Iterations, row)
			}
		}
	}

	// Artifact provenance: scoped to the task's run branch so
	// parallel runs on other branches don't bleed in.
	for _, path := range UnmarshalStringSlice(t.ReadsArtifacts) {
		prov := ArtifactProvenance{Path: path}
		if art, err := s.GetArtifact(projectID, runBranch, path); err == nil && art != nil {
			prov.LastWriter = CitizenUsername(s, art.LastWriter)
			prov.LastTaskID = art.LastTaskID
			prov.CommitSHA = art.CommitSHA
		}
		resp.ArtifactProvenance = append(resp.ArtifactProvenance, prov)
	}

	return resp
}

// ReadyTasksParams bundles the optional filter knobs.
// project_id=0 means "across all projects the caller can see"
// (legacy zero-member projects + memberships); run_seq is
// resolved against project_id (both required to narrow to a
// single run).
type ReadyTasksParams struct {
	ProjectID int64
	RunSeq    int
}

// ListReadyTasks returns the READY tasks visible to the
// caller, filtered by project + run when set. Cross-project
// queries (project_id=0) get the same membership filter as
// list_runs: legacy zero-member projects stay open, member-
// gated projects require the caller on the list.
func ListReadyTasks(s store.CoordinatorStore, caller *store.CitizenRecord, p ReadyTasksParams) ([]TaskResponse, error) {
	if p.ProjectID > 0 {
		if !CanReadProject(s, p.ProjectID, caller.ID) {
			return nil, ErrNotMember
		}
	}
	var runGlobalID int64
	if p.ProjectID > 0 && p.RunSeq > 0 {
		run, _ := s.GetRunByProjectSeq(p.ProjectID, p.RunSeq)
		if run != nil {
			runGlobalID = run.ID
		}
	}
	tasks, err := s.ListReadyTasks(runGlobalID)
	if err != nil {
		return nil, err
	}
	// Project-scoped: when the caller passes ProjectID > 0 we
	// MUST keep the result inside that project, even if the
	// run lookup missed (RunSeq omitted, RunSeq invalid, run
	// doesn't exist yet) and runGlobalID stayed 0. Without
	// this filter the s.ListReadyTasks(0) above returns every
	// ready task across the coord, and a buggy client passing
	// the wrong run_id (e.g. global int64 instead of per-
	// project seq) silently escapes its scope and sees tasks
	// from projects it isn't a member of. This was the bot
	// daemon's symptom: scoped to project 3, it received
	// task 1:1:draft from project 1 because the daemon
	// passed wire.Run.ID instead of wire.Run.Seq. Belt-and-
	// braces: fix on both sides.
	if p.ProjectID > 0 && runGlobalID == 0 {
		filtered := tasks[:0]
		for _, t := range tasks {
			run, _ := s.GetRun(t.RunID)
			if run == nil {
				continue
			}
			if run.ProjectID == p.ProjectID {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}
	// Cross-project: filter to tasks whose run's project the
	// caller can see. Single-project case was already gated by
	// CanReadProject above.
	if p.ProjectID == 0 {
		allowed := map[int64]bool{}
		member, _ := s.ListProjectsForCitizen(caller.ID)
		for _, mp := range member {
			allowed[mp.ID] = true
		}
		filtered := tasks[:0]
		for _, t := range tasks {
			run, _ := s.GetRun(t.RunID)
			if run == nil {
				continue
			}
			total, _ := s.CountProjectMembers(run.ProjectID)
			if total == 0 || allowed[run.ProjectID] {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}
	return ToTaskResponses(s, tasks), nil
}

// GetTask returns one task by id, gated on caller membership
// of the parent project. Returns ErrNotFound when the task
// doesn't exist; ErrNotMember when the caller can't read the
// project.
//
// Note: side-effect-free. The legacy api.handleGetTask path
// also runs maybeResolveDeadlineVote(task) for lazy vote
// resolution; that's an api-side concern (the HTTP path's lazy
// sweep) and stays in the api handler. Native MCP callers
// don't currently trigger it.
func GetTask(s store.CoordinatorStore, caller *store.CitizenRecord, taskID string) (*TaskResponse, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrNotFound
	}
	run, err := s.GetRun(task.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	if !CanReadProject(s, run.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	resp := ToTaskResponse(s, *task)
	return &resp, nil
}

// ToTaskResponses bulks ToTaskResponse over a task slice.
// Note: each call performs its own store reads — for a
// 100-task run, expect O(N) GetRun lookups all returning the
// same row. Fine for now; if it ever shows up in profiles, an
// optional pre-fetched-run hint makes a clean optimization.
func ToTaskResponses(s store.CoordinatorStore, tasks []store.TaskRecord) []TaskResponse {
	out := make([]TaskResponse, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, ToTaskResponse(s, t))
	}
	return out
}
