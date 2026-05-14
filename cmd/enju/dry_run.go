package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/bots"
)

// runDryRun is the `enju go --dry-run` entry point. Parses the
// workflow YAML, substitutes --params, renders the resolved
// DAG, and exits. No coord round-trip, no project resolution,
// no git operations.
//
// Useful in CI ("does my workflow parse with these params and
// produce the expected task set?") and for operators who want
// to inspect what `enju go` would create before committing.
//
// Exit semantics match `enju validate`: 0 on success, 4 on
// parse error or missing required params. Note that dry-run
// surfaces parse-time errors a coord-side `create_run` would
// raise verbatim — the same enjuYaml.ParseWithParams entry point
// the MCP handler uses.
func runDryRun(workflowPath string, params map[string]interface{}, asJSON bool) int {
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dry-run: %v\n", err)
		return 4
	}

	// Two-pass when --params is non-empty: parse once without
	// params to learn declared types, coerce string CLI values
	// to those types, then parse for real. This matches the
	// MCP-side type handling without forcing the operator to
	// type-tag CLI args. Single-pass when no params supplied.
	if len(params) > 0 {
		bare, perr := enjuYaml.Parse(data)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "dry-run: %v\n", perr)
			return 4
		}
		coerced, cerr := coerceCLIParams(params, bare.Run.Params)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "dry-run: %v\n", cerr)
			return 4
		}
		params = coerced
	}

	parsed, err := enjuYaml.ParseWithParams(data, params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dry-run: %v\n", err)
		return 4
	}

	rep := buildDryRunReport(workflowPath, parsed)
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	renderDryRunHuman(os.Stdout, rep)
	return 0
}

// dryRunReport is the JSON shape and the source of truth for the
// human renderer too. Keeping one type avoids drift between
// `--json` and the human output and makes the renderer
// testable without re-running the parser.
type dryRunReport struct {
	WorkflowPath string                 `json:"workflow_path"`
	Name         string                 `json:"name,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Params       map[string]interface{} `json:"params,omitempty"`
	Bots         []dryRunBot            `json:"bots,omitempty"`
	Tasks        []dryRunTask           `json:"tasks"`
	Warnings     []string               `json:"warnings,omitempty"`
}

type dryRunBot struct {
	Name    string `json:"name"`
	Model   string `json:"model,omitempty"`
	Handler string `json:"handler,omitempty"`
}

// dryRunTask captures the operator-visible shape of one task
// instance — what it does, what it waits on, what it produces.
// Intentionally narrower than yaml.TaskInstance: we drop fields
// (env, requirements, container) that aren't load-bearing for a
// "what's the shape of this run going to be?" preview.
type dryRunTask struct {
	ID          string   `json:"id"`
	Action      string   `json:"action"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Reviews     string   `json:"reviews,omitempty"`
	AssignTo    []string `json:"assign_to,omitempty"`
	Reads       []string `json:"reads_artifacts,omitempty"`
	Writes      []string `json:"writes_artifacts,omitempty"`
	PromptHead  string   `json:"prompt_head,omitempty"` // first ~80 chars, for orientation
}

func buildDryRunReport(workflowPath string, parsed *enjuYaml.ParsedRun) dryRunReport {
	rep := dryRunReport{
		WorkflowPath: workflowPath,
		Name:         parsed.Run.Name,
		Description:  parsed.Run.Description,
		Params:       parsed.MergedParams,
		Warnings:     parsed.Warnings,
	}

	// Inline bots: only present when the workflow declares a
	// bots: section. Parse failures are non-fatal here — the
	// validator already surfaced them.
	if m, perr := bots.FromInlineNode(parsed.Run.Bots); perr == nil && m != nil {
		for _, b := range m.Bots {
			rep.Bots = append(rep.Bots, dryRunBot{
				Name: b.Name, Model: b.Model, Handler: b.Handler,
			})
		}
	}

	// Walk ExpandedTasks in stable order so two dry-runs on the
	// same YAML produce identical output. Map iteration is
	// random; sort by instance-key then by task id within each
	// instance.
	keys := make([]string, 0, len(parsed.ExpandedTasks))
	for k := range parsed.ExpandedTasks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, ti := range parsed.ExpandedTasks[k] {
			rep.Tasks = append(rep.Tasks, taskInstanceToDryRun(ti))
		}
	}
	return rep
}

func taskInstanceToDryRun(ti enjuYaml.TaskInstance) dryRunTask {
	out := dryRunTask{
		ID:        ti.FullID,
		Action:    ti.Action,
		DependsOn: ti.DependsOn,
		Reviews:   ti.Reviews,
		AssignTo:  []string(ti.AssignTo),
		Reads:     ti.ReadsArtifacts,
		Writes:    ti.WritesArtifacts.Paths(),
	}
	out.PromptHead = promptHead(ti.Prompt)
	return out
}

// promptHead returns the first non-empty line of the prompt,
// truncated to 80 chars with an ellipsis if longer. Gives the
// operator enough to identify the task without dumping a
// multi-paragraph prompt into the listing.
func promptHead(p string) string {
	for _, line := range strings.Split(p, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if len(t) > 80 {
			return t[:80] + "…"
		}
		return t
	}
	return ""
}

// renderDryRunHuman prints the report in a terminal-friendly
// shape. Sections:
//   1. Header (path, name, description)
//   2. Params (resolved values after default merge)
//   3. Bots (if any)
//   4. Tasks (id, action, deps, prompt-head)
//   5. Warnings (parse-time advisories from the validator)
func renderDryRunHuman(w io.Writer, r dryRunReport) {
	fmt.Fprintf(w, "Workflow: %s\n", r.WorkflowPath)
	if r.Name != "" {
		fmt.Fprintf(w, "Name:     %s\n", r.Name)
	}
	if r.Description != "" {
		fmt.Fprintf(w, "Description:\n  %s\n", strings.ReplaceAll(strings.TrimSpace(r.Description), "\n", "\n  "))
	}

	if len(r.Params) > 0 {
		fmt.Fprintln(w, "\nParameters (after default merge):")
		paramKeys := make([]string, 0, len(r.Params))
		for k := range r.Params {
			paramKeys = append(paramKeys, k)
		}
		sort.Strings(paramKeys)
		for _, k := range paramKeys {
			fmt.Fprintf(w, "  %s = %v\n", k, r.Params[k])
		}
	}

	if len(r.Bots) > 0 {
		fmt.Fprintln(w, "\nBots:")
		for _, b := range r.Bots {
			handler := b.Handler
			if handler == "" {
				handler = "claude"
			}
			fmt.Fprintf(w, "  %-20s  model=%s  handler=%s\n", b.Name, b.Model, handler)
		}
	}

	fmt.Fprintf(w, "\nTasks (%d):\n", len(r.Tasks))
	for _, t := range r.Tasks {
		deps := ""
		if len(t.DependsOn) > 0 {
			deps = " ← " + strings.Join(t.DependsOn, ", ")
		}
		fmt.Fprintf(w, "  %-30s [%-10s]%s\n", t.ID, t.Action, deps)
		if t.Reviews != "" {
			fmt.Fprintf(w, "      reviews: %s\n", t.Reviews)
		}
		if len(t.AssignTo) > 0 {
			fmt.Fprintf(w, "      assign:  %s\n", strings.Join(t.AssignTo, ", "))
		}
		if len(t.Reads) > 0 {
			fmt.Fprintf(w, "      reads:   %s\n", strings.Join(t.Reads, ", "))
		}
		if len(t.Writes) > 0 {
			fmt.Fprintf(w, "      writes:  %s\n", strings.Join(t.Writes, ", "))
		}
		if t.PromptHead != "" {
			fmt.Fprintf(w, "      prompt:  %q\n", t.PromptHead)
		}
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, wn := range r.Warnings {
			fmt.Fprintf(w, "  ⚠ %s\n", wn)
		}
	}
	fmt.Fprintln(w, "\n(dry-run: nothing was created)")
}
