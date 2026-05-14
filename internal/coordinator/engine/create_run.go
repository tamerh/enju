package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// ValidateRunCreation checks pre-flight constraints that
// require store reads: artifact path validity and
// assign_to citizen existence. Called BEFORE CreateRun so
// a failed validation never leaves a ghost run behind.
func (e *Engine) ValidateRunCreation(parsed *enjuYaml.ParsedRun) error {
	for _, tasks := range parsed.ExpandedTasks {
		for _, ti := range tasks {
			for _, p := range ti.ReadsArtifacts {
				if err := ValidateArtifactPath(p); err != nil {
					return fmt.Errorf("task %q: invalid reads_artifacts path %q: %v", ti.ID, p, err)
				}
			}
			for _, entry := range ti.WritesArtifacts {
				if err := ValidateArtifactDeclaration(entry.Path); err != nil {
					return fmt.Errorf("task %q: invalid writes_artifacts path %q: %v", ti.ID, entry.Path, err)
				}
			}
			for _, uname := range ti.AssignTo {
				if err := store.ValidateUsername(uname); err != nil {
					return fmt.Errorf("task %q: invalid assign_to username %q: %v", ti.ID, uname, err)
				}
				c, _ := e.store.GetCitizenByUsername(uname)
				if c == nil {
					return fmt.Errorf("task %q: assign_to citizen %q is not registered", ti.ID, uname)
				}
			}
		}
	}
	return nil
}

// ValidateArtifactPath checks that a CONCRETE artifact path is
// well-formed (non-empty, no leading slash, no path traversal,
// no reserved-dir collisions). Used for:
//
//   - reads_artifacts entries (must be literal — declared deps
//     reference concrete commit-resolvable paths).
//   - artifacts_written entries on submit (the post-expansion
//     literal paths the citizen actually wrote).
//
// For the broader writes_artifacts declaration shape (literal
// + glob + directory + optional), use ValidateArtifactDeclaration
// instead — it enforces the same safety rules but allows the
// pattern-syntax markers (`*`, `?`, `[`, trailing `/`).
//
// Exported so both engine and router can call it.
func ValidateArtifactPath(p string) error {
	return validateArtifactPathCore(p, false)
}

// ValidateArtifactDeclaration is the writes_artifacts variant
// of ValidateArtifactPath: same safety guarantees (no leading
// `/`, no `..`, no `.git/`, no `enju/`), but tolerates the
// declaration-only syntax markers — globs (`*`, `?`, `[`) and
// trailing-slash directory form. The expansion step at submit
// time is responsible for ensuring the concrete matches all
// pass the strict ValidateArtifactPath check, so this gate
// just rejects malformed declarations.
//
// Implementation note: declarations are stripped of trailing
// `/` before re-running the core checks because path-traversal
// rules don't change based on whether the user meant directory
// or file. Glob characters are passed through — they neither
// trigger nor bypass the safety rules; what matters is the
// prefix path (no `..`, no `.git/`, no `enju/`).
func ValidateArtifactDeclaration(p string) error {
	return validateArtifactPathCore(p, true)
}

func validateArtifactPathCore(p string, allowPatterns bool) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("must be relative (no leading /)")
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("must not contain '..'")
	}
	// Strip the directory marker before running reserved-dir
	// checks. `enju/foo/` is rejected by the same rule that
	// rejects `enju/foo`; the trailing slash is purely a
	// classification hint at the syntactic level.
	check := p
	if allowPatterns && strings.HasSuffix(check, "/") {
		check = strings.TrimSuffix(check, "/")
		if check == "" {
			return fmt.Errorf("path is just a slash")
		}
	}
	if !allowPatterns && strings.ContainsAny(check, "*?[") {
		return fmt.Errorf("must not contain glob metacharacters (this path must be literal)")
	}
	// Block reserved directories — artifacts live at natural repo
	// paths so we must prevent writing into Enju's own state dirs
	// or git internals.
	if check == ResultDirRoot || strings.HasPrefix(check, ResultDirRoot+"/") || strings.HasPrefix(check, ResultDirRoot+`\`) {
		return fmt.Errorf("must not write into %s/ (reserved for Enju state)", ResultDirRoot)
	}
	if strings.HasPrefix(check, ".git/") || strings.HasPrefix(check, ".git\\") || check == ".git" {
		return fmt.Errorf("must not write into .git/")
	}
	// enju/templates/ is covered by the ResultDirRoot check above;
	// no separate guard needed now that templates live under enju/.
	return nil
}

// BuildRunTasks converts a ParsedRun's ExpandedTasks into
// store.TaskRecords ready for CreateTask. Pure mapping —
// no store reads, no side effects. Deterministic ordering
// by instance key so seq numbers are reproducible.
//
// Called AFTER CreateRun (when runID + runSeq are known).
func BuildRunTasks(parsed *enjuYaml.ParsedRun, runID int64, projectID int64, runSeq int, runSlug string) []store.TaskRecord {
	runPrefix := fmt.Sprintf("%d:%d:", projectID, runSeq)
	now := time.Now()

	instanceKeys := make([]string, 0, len(parsed.ExpandedTasks))
	for k := range parsed.ExpandedTasks {
		instanceKeys = append(instanceKeys, k)
	}
	sort.Strings(instanceKeys)

	var records []store.TaskRecord
	taskSeq := 0
	for _, instanceKey := range instanceKeys {
		tasks := parsed.ExpandedTasks[instanceKey]
		for _, ti := range tasks {
			taskSeq++
			resultType := ti.ResultType
			if resultType == "" {
				resultType = "text"
			}
			timeout := ti.Timeout
			if timeout == "" {
				timeout = parsed.Run.Defaults.Timeout
			}

			var deps []string
			for _, dep := range ti.DependsOn {
				deps = append(deps, runPrefix+dep)
			}

			state := store.TaskPending
			if len(ti.DependsOn) == 0 {
				state = store.TaskReady
			}

			paramsJSON := ""
			if len(ti.Params) > 0 {
				if b, err := json.Marshal(ti.Params); err == nil {
					paramsJSON = string(b)
				}
			}

			records = append(records, store.TaskRecord{
				ID:              runPrefix + ti.FullID,
				RunID:           runID,
				Seq:             taskSeq,
				TaskDefID:       ti.ID,
				InstanceKey:     instanceKey,
				InstanceParams:  paramsJSON,
				RunSlug:         runSlug,
				Ref:             ti.Ref,
				Action:          ti.Action,
				Prompt:          ti.Prompt,
				UserPrompt:      ti.UserPrompt,
				Script:          ti.Script,
				Outputs:         marshalOutputs(ti.Outputs),
				Requirements:    marshalRequirements(ti.Requirements),
				ResultType:      resultType,
				Timeout:         timeout,
				State:           state,
				DependsOn:       strings.Join(deps, ","),
				ReadsArtifacts:  marshalStringSlice(ti.ReadsArtifacts),
				WritesArtifacts: marshalWriteArtifacts(ti.WritesArtifacts),
				AssignTo:        marshalStringSlice([]string(ti.AssignTo)),
				RequireRole:     ti.RequireRole,
				ReviewsTarget:   ti.Reviews,
				OnReviewReject:         ti.OnReviewReject,
				OnReviewRequestChanges: ti.OnReviewRequestChanges,
				RemediationTemplate:    marshalRemediationTemplate(ti.RemediationTemplate),
				VoteOptions:     marshalVoteOptions(ti.Options),
				Citizens:        ti.Citizens,
				MinQuorum:       ti.MinQuorum,
				VoteThreshold:   ti.Threshold,
				VoteDeadline:    ti.Deadline,
				Anonymize:       ti.Anonymize,
				Visibility:      ti.Visibility,
				Env:              marshalStringMap(ti.Env),
				Mode:             ti.Mode,
				Container:        ti.Container,
				ContainerRuntime: ti.ContainerRuntime,
				CreatedAt:        now,
			})
		}
	}
	return records
}
