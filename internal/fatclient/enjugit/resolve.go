package enjugit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/common/template"
)

// resolve.go — template resolver. Takes a coord-supplied
// dependency descriptor + a prompt template with {{task.content}},
// {{param}}, and {{artifact:path}} references and returns a
// fully-resolved prompt by reading content from the local clone
// at the exact commit SHAs the coordinator's index points at.
//
// Reads at specific SHAs — not HEAD — so a stale clone fails
// loudly rather than silently returning an old version. The
// caller is responsible for ensuring the clone has the commits
// the descriptor references (typically by calling Pull or Fetch
// first).
//
// All gogit operations go through git.Ops (ReadFileAtCommit
// covers the SHA case); the empty-commit fallback reads through
// os.ReadFile against workdir.

// ResolveInput is the structured dependency descriptor a client
// receives when claiming a task. Describes what to read from
// the local clone (and at which commit SHAs) to resolve the
// task's prompt template.
type ResolveInput struct {
	TaskID             string
	PromptTemplate     string
	UserPromptTemplate string
	ForEachParams      map[string]string
	Dependencies       []DependencyRef
	ArtifactReads      []ArtifactRef
}

// DependencyRef is one upstream task instance whose result feeds
// into this task's prompt.
type DependencyRef struct {
	TaskDefID      string
	InstanceKey    string
	InstanceParams map[string]string
	CommitSHA      string
	ResultPath     string
	State          string
	VoteChoice     string
	Responses      []CitizenResponseRef
}

// CitizenResponseRef is one citizen's submission on a multi-
// citizen upstream. Content lives in git only; the resolver
// reads it from `{ResultPath}/citizen-{PathUsername}/result.md`
// at CommitSHA.
type CitizenResponseRef struct {
	Username     string
	PathUsername string
	Option       string
	CommitSHA    string
}

// ArtifactRef is one artifact the task declares reading.
type ArtifactRef struct {
	Path      string
	CommitSHA string
}

// ResolvedPrompt is the output of Resolve. ResolvedArtifacts
// holds present artifact contents; MissingArtifacts lists paths
// that couldn't be read.
type ResolvedPrompt struct {
	Prompt            string
	UserPrompt        string
	ResolvedArtifacts map[string]string
	MissingArtifacts  []string
}

// Resolve renders a prompt template against upstream content
// and artifacts at the dependency-supplied SHAs. The worktree
// path used for the rare empty-CommitSHA fallback comes from
// workflow state (Workflow.WorkDir).
//
// Template resolution order matches the coord-side flow:
//   1. for_each param substitution (`{{gene}}` → "BRCA1")
//   2. upstream task content (`{{analyze.content}}`) — fan-in
//      aggregation when multiple upstreams share a TaskDefID
//   3. artifact content (`{{artifact:path}}`) — inlined from
//      the version at the index's commit SHA
func (w *Workflow) Resolve(input ResolveInput) (*ResolvedPrompt, error) {
	workdir := w.WorkDir()
	// Group dependencies by task def id so fan-in cases get one
	// aggregated entry. Deterministic ordering (sorted by
	// instance key) so the rendered block is stable.
	grouped := make(map[string][]DependencyRef)
	for _, d := range input.Dependencies {
		grouped[d.TaskDefID] = append(grouped[d.TaskDefID], d)
	}
	for key := range grouped {
		deps := grouped[key]
		sort.SliceStable(deps, func(i, j int) bool {
			return deps[i].InstanceKey < deps[j].InstanceKey
		})
		grouped[key] = deps
	}

	taskCtx := ""
	if input.TaskID != "" {
		taskCtx = fmt.Sprintf(" (while resolving task %s)", input.TaskID)
	}
	inputs := make(map[string]interface{})
	for taskDefID, deps := range grouped {
		if len(deps) == 1 {
			result, err := w.readResultForTemplate(deps[0], workdir)
			if err != nil {
				return nil, fmt.Errorf("reading upstream %q%s: %w", taskDefID, taskCtx, err)
			}
			inputs[taskDefID] = result
			continue
		}
		// Fan-in. Build per-iteration Option 4 blocks for each
		// field downstreams can reference: content,
		// winning_option (for_each vote), responses (for_each
		// multi-citizen). Singleton handling above exposes
		// these via readResultForTemplate; the fan-in path
		// aggregates them in the same shape so
		// {{pick.winning_option}} / {{pick.responses}} on
		// fan-in upstreams resolve identically.
		var contentB, winningB, responsesB strings.Builder
		hasWinning, hasResponses := false, false
		for i, d := range deps {
			label := formatIterationLabel(d.InstanceParams, d.InstanceKey)
			header := fmt.Sprintf("### iteration: %s\n", label)

			result, err := w.readResultForTemplate(d, workdir)
			if err != nil {
				return nil, fmt.Errorf("reading upstream %q iteration %q%s: %w", taskDefID, d.InstanceKey, taskCtx, err)
			}
			if i > 0 {
				contentB.WriteString("\n---\n")
			}
			contentB.WriteString(header)
			contentB.WriteString(extractContentForAggregation(result))
			contentB.WriteString("\n")

			if wo, ok := result["winning_option"].(string); ok && wo != "" {
				if hasWinning {
					winningB.WriteString("\n---\n")
				}
				winningB.WriteString(header)
				winningB.WriteString(wo)
				winningB.WriteString("\n")
				hasWinning = true
			}
			if rr, ok := result["responses"]; ok && rr != nil {
				rendered := template.RenderResponsesMarkdown(rr)
				if strings.TrimSpace(rendered) != "" {
					if hasResponses {
						responsesB.WriteString("\n---\n")
					}
					responsesB.WriteString(header)
					responsesB.WriteString(rendered)
					responsesB.WriteString("\n")
					hasResponses = true
				}
			}
		}
		aggregated := map[string]interface{}{
			"task_id": taskDefID,
			"content": contentB.String(),
		}
		if hasWinning {
			aggregated["winning_option"] = winningB.String()
		}
		if hasResponses {
			aggregated["responses"] = responsesB.String()
		}
		inputs[taskDefID] = aggregated
	}

	resolvedPrompt := template.ResolveUpstream(input.PromptTemplate, inputs)
	resolvedPrompt = template.ResolveParams(resolvedPrompt, input.ForEachParams)

	resolvedUserPrompt := ""
	if input.UserPromptTemplate != "" {
		resolvedUserPrompt = template.ResolveUpstream(input.UserPromptTemplate, inputs)
		resolvedUserPrompt = template.ResolveParams(resolvedUserPrompt, input.ForEachParams)
	}

	// Artifacts at the index-supplied commit SHA.
	artifacts := make(map[string]string, len(input.ArtifactReads))
	var missing []string
	for _, ref := range input.ArtifactReads {
		repoPath := ArtifactPath(ref.Path)
		content, ok, err := w.readArtifactVersion(ref.CommitSHA, repoPath, workdir)
		if err != nil || !ok {
			missing = append(missing, ref.Path)
			continue
		}
		artifacts[ref.Path] = content
	}
	resolvedPrompt = template.ResolveArtifacts(resolvedPrompt, artifacts)
	if resolvedUserPrompt != "" {
		resolvedUserPrompt = template.ResolveArtifacts(resolvedUserPrompt, artifacts)
	}

	return &ResolvedPrompt{
		Prompt:            resolvedPrompt,
		UserPrompt:        resolvedUserPrompt,
		ResolvedArtifacts: artifacts,
		MissingArtifacts:  missing,
	}, nil
}

// readResultForTemplate reads one upstream task's result at a
// specific commit and normalizes it into the map shape the
// template resolver consumes.
func (w *Workflow) readResultForTemplate(dep DependencyRef, workdir string) (map[string]interface{}, error) {
	result := map[string]interface{}{
		"task_id": dep.TaskDefID,
	}

	// Terminal-without-content states: skipped (losing vote
	// branch) and failed (hard reject / compute script error).
	// Visible markers in place of content so downstream prompts
	// referencing {{task.content}} see something explicit, not
	// an empty string that could be confused with missing data.
	switch dep.State {
	case "skipped":
		result["content"] = "(skipped)"
		return result, nil
	case "failed":
		result["content"] = "(failed)"
		return result, nil
	}

	if dep.VoteChoice != "" {
		result["winning_option"] = dep.VoteChoice
		result["vote_choice"] = dep.VoteChoice
	}

	// Multi-citizen upstreams: read each citizen's result.md
	// from `{ResultPath}/citizen-{PathUsername}/result.md` at
	// the citizen's own commit SHA. Best-effort: missing
	// commit/file leaves content empty rather than failing.
	if len(dep.Responses) > 0 {
		responses := make([]map[string]interface{}, 0, len(dep.Responses))
		for _, r := range dep.Responses {
			var content string
			if r.CommitSHA != "" && r.PathUsername != "" {
				citizenPath := filepath.Join(dep.ResultPath, "citizen-"+r.PathUsername, "result.md")
				if data, ok, _ := w.readAt(r.CommitSHA, citizenPath, workdir); ok {
					content = string(data)
				}
			}
			responses = append(responses, map[string]interface{}{
				"username": r.Username,
				"option":   r.Option,
				"content":  content,
			})
		}
		result["responses"] = responses
		// Multi-citizen upstreams have no task-level result.md
		// — only per-citizen subdirs. Fall out early; downstreams
		// that want per-citizen content reference {{task.responses}}.
		result["content"] = ""
		return result, nil
	}

	// Read metadata first to distinguish single-file from
	// multi-file (named outputs) results.
	metaPath := filepath.Join(dep.ResultPath, "metadata.json")
	metaBytes, metaOK, err := w.readAt(dep.CommitSHA, metaPath, workdir)
	if err != nil {
		return nil, err
	}
	if metaOK {
		var meta map[string]interface{}
		if json.Unmarshal(metaBytes, &meta) == nil {
			result["metadata"] = meta
			if outputFiles, ok := meta["output_files"].(map[string]interface{}); ok && len(outputFiles) > 0 {
				contentMap := make(map[string]interface{})
				for name, fileName := range outputFiles {
					fname, _ := fileName.(string)
					if fname == "" {
						continue
					}
					data, ok, err := w.readAt(dep.CommitSHA, filepath.Join(dep.ResultPath, fname), workdir)
					if err != nil || !ok {
						continue
					}
					contentMap[name] = string(data)
				}
				result["content"] = contentMap
				return result, nil
			}
		}
	}

	// Fall back to single-file reading (result.md or result.json).
	for _, ext := range []string{".md", ".json"} {
		contentPath := filepath.Join(dep.ResultPath, "result"+ext)
		data, ok, err := w.readAt(dep.CommitSHA, contentPath, workdir)
		if err != nil {
			return nil, err
		}
		if ok {
			s := string(data)
			result["content"] = s
			if ext == ".json" || isJSON(s) {
				var parsed interface{}
				if json.Unmarshal(data, &parsed) == nil {
					result["content"] = parsed
				}
			}
			return result, nil
		}
	}
	// metadata.json present but no result.md / result.json /
	// named outputs — upstream submitted artifacts only.
	// {{<task>.content}} can't resolve; surface the real cause.
	if metaOK {
		return nil, fmt.Errorf(
			"upstream task %q submitted without content (artifacts-only submit); downstream references to {{%s.content}} have nothing to resolve. Pass content= (or outputs=) on submit, or drop the {{.content}} reference and use {{artifact:<path>}} instead",
			dep.TaskDefID, dep.TaskDefID)
	}
	return nil, fmt.Errorf("no result file found under %s at commit %s", dep.ResultPath, dep.CommitSHA)
}

// readAt is the resolver's "read this file at this commit"
// primitive. Empty commitSHA falls back to reading from the
// working tree at workdir — useful for tests and the rare path
// where the caller doesn't care which version it gets.
func (w *Workflow) readAt(commitSHA, repoRelPath, workdir string) ([]byte, bool, error) {
	if commitSHA == "" {
		if workdir == "" {
			return nil, false, nil
		}
		data, err := os.ReadFile(filepath.Join(workdir, repoRelPath))
		if err != nil {
			return nil, false, nil
		}
		return data, true, nil
	}
	body, found, err := w.git.ReadFileAtCommit(commitSHA, repoRelPath)
	if err != nil {
		return nil, false, translateGitError("read at commit", err)
	}
	return body, found, nil
}

// readArtifactVersion reads an artifact file at a specific
// commit (or the worktree if commitSHA is empty) and returns
// its content as a string.
func (w *Workflow) readArtifactVersion(commitSHA, repoRelPath, workdir string) (string, bool, error) {
	data, ok, err := w.readAt(commitSHA, repoRelPath, workdir)
	if err != nil || !ok {
		return "", false, err
	}
	return string(data), true, nil
}

// formatIterationLabel renders a task's iteration context as
// "key1=val1, key2=val2" using the supplied params, or the raw
// instance key as a fallback. Keys sorted for determinism.
func formatIterationLabel(params map[string]string, instanceKey string) string {
	if len(params) == 0 {
		return instanceKey
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, ", ")
}

// extractContentForAggregation pulls a printable "content" out
// of a result map.
func extractContentForAggregation(result map[string]interface{}) string {
	if result == nil {
		return ""
	}
	if errMsg, ok := result["error"].(string); ok {
		return "(upstream error: " + errMsg + ")"
	}
	content, ok := result["content"]
	if !ok {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case map[string]interface{}:
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func isJSON(s string) bool {
	s = strings.TrimSpace(s)
	return (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"))
}
