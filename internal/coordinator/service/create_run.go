package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// CreateRunParams is the input shape for CreateRun. Mirrors the
// REST createRunRequest body so the api translator is a flat
// field copy.
type CreateRunParams struct {
	YAML      string
	RepoURL     string
	Params     map[string]interface{}
	SourcePath   string
	SourceCommitSHA string
	Username    string // citizen who created this run, for contribution tracking
	Branch     string // empty | "auto" | explicit branch name
}

// CreateRun parses + validates the run YAML, resolves the
// branch (project default / "auto" / explicit), enforces the
// serial-runs-per-branch invariant, persists the run record +
// tasks atomically, primes the DAG cache, and records the
// run_created contribution event.
//
// Caller is responsible for project membership gating before
// calling this. The Username param is best-effort attribution
// for contribution tracking — empty is allowed (no event
// recorded).
//
// Errors:
//   - ErrInvalidArgument: bad YAML, bad branch shape,
//   ValidateRunCreation failure
//   - ErrNotFound: project missing
//   - ErrConflict: serial-per-branch violation
//   (ActiveRunOnBranch race or partial-unique-index hit)
func (c *Coordinator) CreateRun(projectID int64, params CreateRunParams) (*RunResponse, error) {
	if projectID == 0 {
		return nil, fmt.Errorf("%w: invalid project ID", ErrInvalidArgument)
	}
	if params.YAML == "" {
		return nil, fmt.Errorf("%w: yaml is required", ErrInvalidArgument)
	}

	proj, err := c.Store.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if proj == nil {
		return nil, fmt.Errorf("%w: project %d not found", ErrNotFound, projectID)
	}

	// Always route through ParseWithParams so declared defaults
	// fire even when the caller supplied no params. nil params
	// is equivalent to "caller supplied nothing" — defaults
	// still apply and a required-no-default param raises the
	// natural-language error.
	parsed, err := enjuYaml.ParseWithParams([]byte(params.YAML), params.Params)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid run definition: %s", ErrInvalidArgument, err.Error())
	}

	// Branch resolution — three paths: empty → project
	// default; "auto" → pick an unused "<slug>-N"; explicit →
	// use verbatim, just validate shape.
	branch, err := c.resolveRunBranch(projectID, proj.DefaultBranch, params.Branch, params.SourcePath, parsed.Run.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}

	// Serial-runs-per-branch invariant: refuse a second active
	// run on the same branch. "auto" skips this check —
	// resolveRunBranch already guarantees the result is unused.
	if params.Branch != "auto" {
		if existing, _ := c.Store.ActiveRunOnBranch(projectID, branch); existing != nil {
			return nil, fmt.Errorf("%w: branch %q already has an active run (#%d %q) — wait for it to finish, use branch=\"auto\" for an auto-named branch, or pass branch=\"<name>\" to isolate this run",
				ErrConflict, branch, existing.Seq, existing.Name)
		}
	}

	// Pre-flight validation via engine (artifact paths +
	// citizen usernames). Runs BEFORE CreateRun so a failed
	// validation never leaves a ghost run behind.
	if err := engine.New(c.Store, c.Logger).ValidateRunCreation(parsed); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}

	now := time.Now()
	// Persist the MERGED params (declared defaults + caller-
	// supplied values, supplied wins) so the compute executor
	// rehydrates them into ENJU_PARAM_* env vars on task
	// execution. Persisting only req.Params would drop defaults
	// — `{{param}}` would substitute correctly at parse time
	// but ENJU_PARAM_<name> would come up empty for any param
	// the caller didn't type.
	var paramsJSON string
	if len(parsed.MergedParams) > 0 {
		if b, merr := json.Marshal(parsed.MergedParams); merr == nil {
			paramsJSON = string(b)
		}
	}
	runSlug := engine.ComputeRunSlug(params.SourcePath, parsed.Run.Name)
	runRes, err := c.Store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateRun{Run: store.RunRecord{
				ProjectID:       projectID,
				Name:            parsed.Run.Name,
				Ref:             parsed.Run.Ref,
				YAMLData:        params.YAML,
				RepoURL:         params.RepoURL,
				State:           store.RunActive,
				SourcePath:      params.SourcePath,
				SourceCommitSHA: params.SourceCommitSHA,
				Params:          paramsJSON,
				Branch:          branch,
				Slug:            runSlug,
				CreatedAt:       now,
				UpdatedAt:       now,
			}},
		},
	})
	runID, runSeq := runRes.RunID, runRes.RunSeq
	if err != nil {
		// Partial unique index on (project_id, branch) WHERE
		// state='active' fires when a concurrent request wins
		// the race past ActiveRunOnBranch but before our
		// INSERT. Translate to the same 409 + helpful message
		// the application-level refusal produces, so both
		// paths surface an identical UX.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "idx_runs_active_branch") {
			msg := fmt.Sprintf("branch %q already has an active run — wait for it to finish, use branch=\"auto\" for an auto-named branch, or pass branch=\"<name>\" to isolate this run", branch)
			if existing, _ := c.Store.ActiveRunOnBranch(projectID, branch); existing != nil {
				msg = fmt.Sprintf("branch %q already has an active run (#%d %q) — wait for it to finish, use branch=\"auto\" for an auto-named branch, or pass branch=\"<name>\" to isolate this run", branch, existing.Seq, existing.Name)
			}
			return nil, fmt.Errorf("%w: %s", ErrConflict, msg)
		}
		c.Logger.Error("creating run", "error", err)
		return nil, fmt.Errorf("failed to create run: %w", err)
	}

	// Living-workflow phase 4c — persist the run-level
	// auto_triage rule (if any) before tasks are inserted, so
	// the auto-triage hook can read it the moment the run lands
	// on idle. Empty when not declared OR when declared empty
	// (`auto_triage: {}`) so static workflows and "rule
	// present but empty" are both treated uniformly. Without
	// the empty check, an empty block would land as `{}` in
	// the column and the maybeAutoTriage hook would log
	// "missing action" warnings every idle tick.
	if t := parsed.Run.AutoTriage; t != nil &&
		(t.Action != "" || t.Prompt != "" || len(t.AssignTo) > 0 || t.RequireRole != "") {
		if data, jerr := json.Marshal(t); jerr == nil {
			if _, serr := c.Store.ApplyPlan(store.Plan{
				Version: engine.EngineVersion,
				Mutations: []store.Mutation{
					store.SetAutoTriageTemplate{RunID: runID, TemplateJSON: string(data)},
				},
			}); serr != nil {
				c.Logger.Error("setting auto_triage_template", "run_id", runID, "error", serr)
			}
		}
	}

	// Build task records via engine and apply atomically.
	taskRecords := engine.BuildRunTasks(parsed, runID, projectID, runSeq, runSlug)
	var mutations []store.Mutation
	for i := range taskRecords {
		mutations = append(mutations, store.CreateTask{Task: taskRecords[i]})
	}
	if len(mutations) > 0 {
		plan := store.Plan{
			Version:  engine.EngineVersion,
			Mutations: mutations,
		}
		if _, perr := c.Store.ApplyPlan(plan); perr != nil {
			c.Logger.Error("creating tasks", "error", perr)
			return nil, fmt.Errorf("failed to create tasks: %w", perr)
		}
	}
	taskCount := len(taskRecords)

	// Cache DAG and parsed run in memory for cascade-touching
	// code paths (invalidate, materialize, etc.).
	c.Cache.Put(runID, parsed)

	c.Logger.Info("run created", "id", runID, "project_id", projectID, "seq", runSeq, "name", parsed.Run.Name, "tasks", taskCount)

	if params.Username != "" {
		if citizen, _ := c.Store.GetCitizenByUsername(params.Username); citizen != nil {
			c.Store.RecordContributionEvent(&store.ContributionEvent{
				CitizenID: citizen.ID,
				EventType: "run_created",
				RunID:   runID,
				ProjectID: projectID,
				Metadata: store.MarshalMetadata(map[string]any{
					"tasks": taskCount,
				}),
				CreatedAt: now,
			})
		}
	}

	if len(parsed.Warnings) > 0 {
		c.Logger.Info("run created with warnings",
			"id", runID, "warnings", parsed.Warnings)
	}

	return &RunResponse{
		ID:       runID,
		ProjectID:    projectID,
		Seq:       runSeq,
		Name:      parsed.Run.Name,
		State:      string(store.RunActive),
		TaskCount:    taskCount,
		Branch:     branch,
		Slug:      runSlug,
		CreatedAt:    now.Format(time.RFC3339),
		SourcePath:   params.SourcePath,
		SourceCommitSHA: params.SourceCommitSHA,
		Warnings:    parsed.Warnings,
	}, nil
}

// resolveRunBranch picks the run's git branch from the three
// supported forms: empty (use project default, fallback "main"),
// "auto" (allocate <slug>-N), or explicit (use verbatim, just
// validate shape).
//
// The auto-allocation slug comes from engine.ComputeRunSlug
// (the same slugger that stamps `enju/runs/{seq}-{slug}/`) so
// the user never sees `git checkout quick-inline-1` pointing at
// a run whose dir is `2-Quick_Inline/` (style drift that
// surfaced on early testing).
func (c *Coordinator) resolveRunBranch(projectID int64, defaultBranch, requested, sourcePath, runName string) (string, error) {
	if requested == "" {
		if defaultBranch == "" {
			return "main", nil
		}
		return defaultBranch, nil
	}
	if requested == "auto" {
		// Walk <slug>-1, <slug>-2, ... picking the first
		// unused. Bounded to 10_000 so a misbehaving caller
		// can't stall the endpoint forever.
		used := map[string]bool{}
		branches, err := c.Store.ListRunBranches(projectID)
		if err != nil {
			return "", fmt.Errorf("allocating auto branch name: %w", err)
		}
		for _, b := range branches {
			used[b] = true
		}
		slug := engine.ComputeRunSlug(sourcePath, runName)
		// Defense in depth: a slug that slips past the kebab
		// slugger into something git would reject (shouldn't
		// happen — outputs are [a-z0-9-]+) falls back to "run".
		if validateBranchName(slug) != nil {
			slug = "run"
		}
		for n := 1; n <= 10000; n++ {
			name := fmt.Sprintf("%s-%d", slug, n)
			if !used[name] {
				return name, nil
			}
		}
		return "", fmt.Errorf("unable to allocate an auto branch name after 10000 tries — pass branch=\"<name>\" explicitly")
	}
	if err := validateBranchName(requested); err != nil {
		return "", err
	}
	return requested, nil
}
