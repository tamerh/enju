package mcpgit

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enju-ai/enju/internal/template"
)

// ResolveInput is the structured dependency descriptor a client
// receives from the coordinator when it claims a task. It describes
// what the client needs to read from its local clone (and at which
// commit SHAs) in order to resolve the task's prompt template.
//
// In the Phase 1 architecture the coordinator read these files
// itself and returned a fully-resolved prompt. In the orchestrator
// model the coordinator only returns the descriptor; the client
// reads the files and substitutes them locally.
type ResolveInput struct {
	// TaskID identifies the task being resolved. Used only to
	// annotate error messages so a citizen seeing a missing-upstream
	// or missing-artifact error knows which task triggered it.
	// Optional — resolver still works with an empty TaskID.
	TaskID string
	// PromptTemplate is the raw prompt with `{{task.content}}`,
	// `{{param}}`, and `{{artifact:path}}` references still in
	// place.
	PromptTemplate string
	// UserPromptTemplate is the assisted-task user prompt, if the
	// task declares one. Same template syntax as PromptTemplate.
	UserPromptTemplate string
	// ForEachParams is the map of `for_each` variable names to the
	// instance's values. Empty for singleton tasks.
	ForEachParams map[string]string
	// Dependencies lists the upstream task instances this task
	// reads content from. Multiple entries with the same TaskDefID
	// indicate a fan-in aggregation (iteration 5 task-level
	// for_each).
	Dependencies []DependencyRef
	// ArtifactReads lists the artifact paths this task declares
	// reading, each with the commit SHA the coordinator's artifact
	// index currently points at. The client reads the file's
	// content as of that commit.
	ArtifactReads []ArtifactRef
}

// DependencyRef is one upstream task instance whose result feeds
// into this task's prompt.
type DependencyRef struct {
	// TaskDefID is the short task ID as written in the YAML.
	// Multiple DependencyRefs with the same TaskDefID form a
	// fan-in group.
	TaskDefID string
	// InstanceKey is the for_each iteration key (e.g. "BRCA1"),
	// or empty for singleton upstreams.
	InstanceKey string
	// InstanceParams is the upstream's for_each params, used to
	// render the fan-in label in the Option 4 block. Optional —
	// if empty, the raw InstanceKey is used as the label.
	InstanceParams map[string]string
	// CommitSHA identifies the exact commit where this upstream
	// task's result was written. The client reads files at this
	// commit to get the exact version the coordinator's DB points
	// at (avoiding races with newer commits on the local clone).
	CommitSHA string
	// ResultPath is the repo-relative directory holding the
	// upstream's result files (e.g. "runs/2/gather" or
	// "runs/2/BRCA1/gather").
	ResultPath string
	// State is the upstream's lifecycle state ("accepted",
	// "skipped", "failed", etc.). Used to distinguish
	// terminal-with-content (accepted) from terminal-without-
	// content (skipped / failed). For skipped / failed, the
	// resolver returns a visible marker instead of trying to
	// read nonexistent result files.
	State string
	// VoteChoice is the upstream vote task's winning option id.
	// Populated only when the upstream is an action:vote task
	// that has resolved; empty otherwise. When non-empty, the
	// resolver attaches it as a `winning_option` field on the
	// result map so downstream prompts can reference it via
	// `{{task.winning_option}}`.
	VoteChoice string
	// Responses is the per-citizen submission list for
	// multi-citizen upstreams. Each entry has a username + the
	// citizen's choice (option id for vote tasks,
	// "approve"/"reject" for review tasks). The resolver reads
	// the content from the per-citizen result.md subdirectory
	// and attaches the array to the result map so
	// `{{task.responses}}` renders a markdown block with each
	// voter's verdict + commentary.
	Responses []CitizenResponseRef
}

// CitizenResponseRef is one citizen's submission on a multi-
// citizen upstream task. Content comes from the coordinator
// descriptor (sourced from task_claims.content) — the
// authoritative storage for multi-citizen responses. The
// resolver uses it directly instead of reading per-citizen
// result.md from git.
type CitizenResponseRef struct {
	Username string
	Option   string
	Content  string
}

// ArtifactRef is one artifact the task declares reading.
type ArtifactRef struct {
	// Path is the user-facing artifact path (no `artifacts/`
	// prefix).
	Path string
	// CommitSHA is the commit where the artifact's current
	// version was written, according to the coordinator's index.
	CommitSHA string
}

// ResolvedPrompt is the output of Resolve. Fields mirror what the
// old coordinator-side `handleGetTaskInputs` returned, minus the
// coordinator-internal bookkeeping.
type ResolvedPrompt struct {
	// Prompt is the fully-resolved prompt ready to hand to Claude.
	Prompt string
	// UserPrompt is the fully-resolved user prompt (assisted
	// tasks), empty if none.
	UserPrompt string
	// ResolvedArtifacts maps each artifact path the task read to
	// its content. Present artifacts are included; missing ones
	// surface via MissingArtifacts.
	ResolvedArtifacts map[string]string
	// MissingArtifacts lists artifact paths that were declared
	// but couldn't be read (missing file, bad SHA, etc.).
	// Formatters use this to render a warning block.
	MissingArtifacts []string
}

// Resolve takes the dependency descriptor the coordinator returned
// and renders a fully-resolved prompt by reading upstream results
// and artifacts from the project's local clone.
//
// The caller should have already called Pull() on the project to
// ensure the local clone has all the commits the descriptor
// references. Reads happen at the specific commit SHAs in the
// descriptor, so a stale clone will fail the reads loudly rather
// than silently returning stale content.
//
// Template resolution order, matching the iteration 5 flow:
//
//  1. for_each param substitution (`{{gene}}` → "BRCA1")
//  2. upstream task content (`{{analyze.content}}`) — with
//     fan-in aggregation when multiple upstreams share a TaskDefID
//  3. artifact content (`{{artifact:path}}`) — inlined from the
//     version at the index's commit SHA
func (p *Project) Resolve(input ResolveInput) (*ResolvedPrompt, error) {
	// 1. Group dependencies by task def id so fan-in cases get one
	// aggregated entry. Deterministic ordering (sorted by instance
	// key) so the rendered block is stable.
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

	// 2. Build the inputs map for the template resolver. For
	// singleton upstreams the value is a straight result map; for
	// fan-in upstreams the value is a synthetic aggregated result
	// whose "content" is the Option 4 block. The resolver doesn't
	// need to know the difference.
	taskCtx := ""
	if input.TaskID != "" {
		taskCtx = fmt.Sprintf(" (while resolving task %s)", input.TaskID)
	}
	inputs := make(map[string]interface{})
	for taskDefID, deps := range grouped {
		if len(deps) == 1 {
			result, err := p.readResultForTemplate(deps[0])
			if err != nil {
				return nil, fmt.Errorf("reading upstream %q%s: %w", taskDefID, taskCtx, err)
			}
			inputs[taskDefID] = result
			continue
		}
		// Fan-in. Build per-iteration Option 4 blocks for each
		// field downstreams can reference: content, winning_option
		// (for_each vote upstreams), responses (for_each
		// multi-citizen upstreams). Singleton handling above
		// already exposes these fields via readResultForTemplate;
		// the fan-in path needs to aggregate them in the same
		// shape or {{pick.winning_option}} / {{pick.responses}}
		// leak through as literal placeholders.
		//
		// Each field is its own independent block — a downstream
		// may reference any subset. Only populated when at least
		// one iteration had a non-empty value for that field, so
		// {{task.responses}} on a non-multi-citizen for_each
		// upstream stays literal-unresolved (same behavior as
		// the singleton case).
		var contentB, winningB, responsesB strings.Builder
		hasWinning, hasResponses := false, false
		for i, d := range deps {
			label := formatIterationLabel(d.InstanceParams, d.InstanceKey)
			header := fmt.Sprintf("### iteration: %s\n", label)

			result, err := p.readResultForTemplate(d)
			if err != nil {
				return nil, fmt.Errorf("reading upstream %q iteration %q%s: %w", taskDefID, d.InstanceKey, taskCtx, err)
			}

			// Content (always present — even an empty-content
			// iteration produces an empty block under its
			// label for layout consistency).
			if i > 0 {
				contentB.WriteString("\n---\n")
			}
			contentB.WriteString(header)
			contentB.WriteString(extractContentForAggregation(result))
			contentB.WriteString("\n")

			// Winning option (for_each vote).
			if wo, ok := result["winning_option"].(string); ok && wo != "" {
				if hasWinning {
					winningB.WriteString("\n---\n")
				}
				winningB.WriteString(header)
				winningB.WriteString(wo)
				winningB.WriteString("\n")
				hasWinning = true
			}

			// Responses (for_each multi-citizen). Each
			// iteration's per-citizen list is pre-rendered by
			// the shared template helper so downstream sees
			// consistent markdown (### @username — option)
			// whether the upstream was singleton or fan-in.
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
			// Stashed as a string — the template resolver's
			// responses branch accepts strings (from fan-in)
			// AND []{username,option,content} entries (from
			// singleton readResultForTemplate) via the same
			// extractField switch.
			aggregated["responses"] = responsesB.String()
		}
		inputs[taskDefID] = aggregated
	}

	// 3. Resolve {{task.field}} references first, then for_each
	// params, then artifacts. Same order the coordinator used.
	resolvedPrompt := template.ResolveUpstream(input.PromptTemplate, inputs)
	resolvedPrompt = template.ResolveParams(resolvedPrompt, input.ForEachParams)

	resolvedUserPrompt := ""
	if input.UserPromptTemplate != "" {
		resolvedUserPrompt = template.ResolveUpstream(input.UserPromptTemplate, inputs)
		resolvedUserPrompt = template.ResolveParams(resolvedUserPrompt, input.ForEachParams)
	}

	// 4. Read declared artifacts and substitute `{{artifact:path}}`
	// references. Reads happen at the index's commit SHA so the
	// content matches what the coordinator's DB points at — not
	// whatever the local clone's HEAD has right now.
	artifacts := make(map[string]string, len(input.ArtifactReads))
	var missing []string
	for _, ref := range input.ArtifactReads {
		// A.7 backward compat: try the namespaced path first,
		// fall back to the pre-A.5 flat `artifacts/...` layout
		// on miss. Projects created before the namespacing have
		// their files at the legacy location even though the
		// DB index now reports their path in the unprefixed
		// user-facing form.
		repoPath := ArtifactPath(ref.Path)
		content, ok, err := p.readArtifactVersion(ref.CommitSHA, repoPath)
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
// template resolver consumes. Mirrors the coordinator's legacy
// `readResultForTemplate` but reads from the local clone instead of
// the coordinator's working tree.
func (p *Project) readResultForTemplate(dep DependencyRef) (map[string]interface{}, error) {
	result := map[string]interface{}{
		"task_id": dep.TaskDefID,
	}

	// Terminal-without-content states: skipped (losing vote
	// branch) and failed (hard reject / compute script error).
	// These have no result files on disk — reading would 404.
	// Return a visible marker as content so downstream prompts
	// that reference {{task.content}} see something explicit
	// ("(skipped)") rather than an empty string (which could
	// be mistaken for missing data) or a claim-time error
	// (which would break workflows that legitimately aggregate
	// across both taken and skipped branches).
	switch dep.State {
	case "skipped":
		result["content"] = "(skipped)"
		return result, nil
	case "failed":
		result["content"] = "(failed)"
		return result, nil
	}

	// Phase E.2 — vote task upstreams expose their winning
	// option id via `{{task.winning_option}}` in downstream
	// prompts. Set both the Phase-E-facing "winning_option" key
	// and a legacy-friendly "vote_choice" alias so either reference
	// form resolves. Non-vote upstreams leave both blank.
	if dep.VoteChoice != "" {
		result["winning_option"] = dep.VoteChoice
		result["vote_choice"] = dep.VoteChoice
	}

	// Phase E.2 session 2b — multi-citizen upstreams carry a
	// per-citizen response list. Read each voter's commentary
	// from `citizen-<username>/result.md` at the dep's commit
	// and attach the array as a `responses` field so downstream
	// prompts can render `{{task.responses}}` as a markdown
	// block with each voter's verdict + prose.
	if len(dep.Responses) > 0 {
		responses := make([]map[string]interface{}, 0, len(dep.Responses))
		for _, r := range dep.Responses {
			// Content comes from the descriptor (sourced from
			// task_claims.content on the coordinator), not
			// from reading per-citizen result.md via git.
			// The DB column is the authoritative source for
			// multi-citizen submissions since commit_sha is
			// now optional on vote/review actions and not
			// every submit results in a git commit.
			responses = append(responses, map[string]interface{}{
				"username": r.Username,
				"option":   r.Option,
				"content":  r.Content,
			})
		}
		result["responses"] = responses
		// Multi-citizen upstreams don't have a single
		// task-level result.md — the per-citizen subdirs are
		// the only on-disk content. Fall out early with the
		// responses map attached, since the normal read-
		// metadata-then-result-file path would fail on the
		// missing base file. {{task.content}} on a
		// multi-citizen upstream returns an empty string;
		// authors who want per-citizen content reference
		// {{task.responses}} instead.
		result["content"] = ""
		return result, nil
	}

	// Read metadata first so we can distinguish single-file results
	// from multi-file (named outputs) results.
	metaPath := filepath.Join(dep.ResultPath, "metadata.json")
	metaBytes, metaOK, err := p.readAt(dep.CommitSHA, metaPath)
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
					data, ok, err := p.readAt(dep.CommitSHA, filepath.Join(dep.ResultPath, fname))
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
		data, ok, err := p.readAt(dep.CommitSHA, contentPath)
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
	// metadata.json present but neither result.md nor result.json nor
	// named-output files — the upstream submit carried artifacts only
	// (no content=, no outputs=). Downstream references to
	// `{{<task>.content}}` can't resolve. Surface that directly
	// rather than the low-level "no result file" error, which hides
	// the real cause from authors.
	if metaOK {
		return nil, fmt.Errorf(
			"upstream task %q submitted without content (artifacts-only submit); downstream references to {{%s.content}} have nothing to resolve. Pass content= (or outputs=) on submit, or drop the {{.content}} reference and use {{artifact:<path>}} instead",
			dep.TaskDefID, dep.TaskDefID)
	}
	return nil, fmt.Errorf("no result file found under %s at commit %s", dep.ResultPath, dep.CommitSHA)
}

// readAt is the client's "read this file at this commit" primitive.
// If the commit SHA is empty, it falls back to reading from the
// working tree — useful for tests and for cases where the caller
// doesn't care which version it gets.
func (p *Project) readAt(commitSHA, repoRelPath string) ([]byte, bool, error) {
	if commitSHA == "" {
		data, err := p.ReadFile(repoRelPath)
		if err != nil {
			return nil, false, nil
		}
		return data, true, nil
	}
	return p.ReadFileAtCommit(commitSHA, repoRelPath)
}

// readArtifactVersion reads an artifact file at a specific commit
// (or the working tree if commitSHA is empty).
func (p *Project) readArtifactVersion(commitSHA, repoRelPath string) (string, bool, error) {
	data, ok, err := p.readAt(commitSHA, repoRelPath)
	if err != nil || !ok {
		return "", false, err
	}
	return string(data), true, nil
}

// formatIterationLabel renders a task's iteration context as
// "key1=val1, key2=val2" using the supplied params, or the raw
// instance key as a fallback. Keys sorted for determinism. Mirrors
// the coordinator-side helper from iteration 5.
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

// extractContentForAggregation pulls a printable "content" out of a
// result map. Matches the coordinator's iteration 5 helper.
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
