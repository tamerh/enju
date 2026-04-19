package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/store"
)

// InputsDescriptor is the structured dependency descriptor
// returned at claim time. The fat client's template resolver
// consumes it to resolve {{task.content}}, {{task.responses}},
// {{artifact:path}}, and for_each params without the
// coordinator needing to read any git content.
type InputsDescriptor struct {
	TaskID             string                   `json:"task_id"`
	PromptTemplate     string                   `json:"prompt_template"`
	UserPromptTemplate string                   `json:"user_prompt_template"`
	ForEachParams      map[string]string        `json:"for_each_params"`
	Dependencies       []map[string]interface{} `json:"dependencies"`
	ArtifactReads      []map[string]interface{} `json:"artifact_reads"`
	ProjectID          int64                    `json:"project_id"`
	ProjectRemoteURL   string                   `json:"project_remote_url"`
}

// BuildInputsDescriptor constructs the claim-time dependency
// descriptor for a task. Pure computation — reads upstream
// task state, artifact index, and citizen usernames via
// ReadStore. Never writes.
//
// For each upstream dependency:
//   - Resolves commit_sha + result_path so the client knows
//     which git commit to read from.
//   - For vote upstreams: includes vote_choice so downstream
//     {{task.winning_option}} resolves.
//   - For multi-citizen upstreams (citizens > 1): includes
//     per-citizen responses (username + option + content) so
//     downstream {{task.responses}} resolves. Anonymized
//     upstreams render citizen-N placeholders instead of
//     real usernames.
//
// For each artifact read:
//   - Resolves commit_sha from the artifact index. Missing
//     artifacts (deleted after invalidation) return empty
//     commit_sha — the client surfaces these as warnings.
func (e *Engine) BuildInputsDescriptor(
	task *store.TaskRecord,
	run *store.RunRecord,
) (*InputsDescriptor, error) {
	desc := &InputsDescriptor{
		TaskID:             task.ID,
		PromptTemplate:     task.Prompt,
		UserPromptTemplate: task.UserPrompt,
		ProjectID:          run.ProjectID,
	}

	// for_each params.
	if task.InstanceParams != "" {
		_ = json.Unmarshal([]byte(task.InstanceParams), &desc.ForEachParams)
	}

	// Project remote URL.
	if p, _ := e.store.GetProject(run.ProjectID); p != nil {
		desc.ProjectRemoteURL = p.RemoteURL
	}

	// Dependencies.
	if task.DependsOn != "" {
		for _, depID := range strings.Split(task.DependsOn, ",") {
			depID = strings.TrimSpace(depID)
			depTask, err := e.store.GetTask(depID)
			if err != nil || depTask == nil {
				continue
			}
			var params map[string]string
			if depTask.InstanceParams != "" {
				_ = json.Unmarshal([]byte(depTask.InstanceParams), &params)
			}
			depEntry := map[string]interface{}{
				"task_def_id":     depTask.TaskDefID,
				"instance_key":    depTask.InstanceKey,
				"instance_params": params,
				"commit_sha":      depTask.CommitSHA,
				"result_path":     depTask.ResultPath,
				"vote_choice":     depTask.VoteChoice,
				// State lets the client-side resolver distinguish
				// terminal-with-content (accepted) from
				// terminal-without-content (skipped / failed) so
				// it can render a visible marker instead of
				// trying to read nonexistent result files.
				"state": depTask.State,
			}
			// Multi-citizen upstreams: per-citizen responses
			// for {{task.responses}} resolution.
			if depTask.Citizens > 1 {
				if submissions, err := e.store.ListVoteSubmissions(depTask.ID); err == nil && len(submissions) > 0 {
					perCitizen := make([]map[string]interface{}, 0, len(submissions))
					for idx, sub := range submissions {
						username := e.citizenUsername(sub.CitizenID)
						if depTask.Anonymize {
							username = fmt.Sprintf("citizen-%d", idx+1)
						}
						perCitizen = append(perCitizen, map[string]interface{}{
							"username": username,
							"option":   sub.Option,
							"content":  sub.Content,
						})
					}
					depEntry["responses"] = perCitizen
				}
			}
			desc.Dependencies = append(desc.Dependencies, depEntry)
		}
	}

	// Artifact reads.
	var artifactPaths []string
	if task.ReadsArtifacts != "" {
		_ = json.Unmarshal([]byte(task.ReadsArtifacts), &artifactPaths)
	}
	for _, p := range artifactPaths {
		art, err := e.store.GetArtifact(run.ProjectID, run.Branch, p)
		if err != nil || art == nil {
			desc.ArtifactReads = append(desc.ArtifactReads, map[string]interface{}{
				"path":       p,
				"commit_sha": "",
			})
			continue
		}
		desc.ArtifactReads = append(desc.ArtifactReads, map[string]interface{}{
			"path":       p,
			"commit_sha": art.CommitSHA,
		})
	}

	return desc, nil
}

// citizenUsername resolves a citizen ID to their username.
func (e *Engine) citizenUsername(id int64) string {
	c, err := e.store.GetCitizen(id)
	if err != nil || c == nil {
		return fmt.Sprintf("citizen-%d", id)
	}
	return c.Username
}
