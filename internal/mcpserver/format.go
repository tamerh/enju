package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// --- Rich formatting for MCP tool responses ---

func formatProjectList(data []byte) string {
	var projects []map[string]interface{}
	if err := json.Unmarshal(data, &projects); err != nil {
		return string(data)
	}
	if len(projects) == 0 {
		return "No projects found."
	}

	var b strings.Builder
	b.WriteString("Enju Projects (long-lived workspaces)\n\n")
	for _, p := range projects {
		name, _ := p["name"].(string)
		desc, _ := p["description"].(string)
		runCount, _ := p["run_count"].(float64)
		id := jsonID(p["id"])

		b.WriteString(fmt.Sprintf("  #%s  %-30s  %d runs", id, name, int(runCount)))
		if desc != "" {
			b.WriteString(fmt.Sprintf("  — %s", desc))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nTip: A project holds many runs over time. Use enju_list_runs to see all runs, or filter to a project.")
	return b.String()
}

func formatCreateProjectResult(data []byte) string {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data)
	}
	if errMsg, ok := result["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to create project: %s", errMsg)
	}
	name, _ := result["name"].(string)
	id := jsonID(result["id"])
	return fmt.Sprintf("✓ Project #%s created: %s", id, name)
}

func formatRunList(data []byte) string {
	var runs []map[string]interface{}
	if err := json.Unmarshal(data, &runs); err != nil {
		return string(data)
	}

	if len(runs) == 0 {
		return "No runs found."
	}

	var b strings.Builder
	b.WriteString("Enju Runs\n\n")

	for _, p := range runs {
		name, _ := p["name"].(string)
		state, _ := p["state"].(string)
		taskCount, _ := p["task_count"].(float64)
		projectID := jsonID(p["project_id"])
		seq, _ := p["seq"].(float64)

		icon := stateIcon(state)
		b.WriteString(fmt.Sprintf("  %s project #%s → run #%d  %-30s [%s]  %d tasks\n",
			icon, projectID, int(seq), name, state, int(taskCount)))
	}

	return b.String()
}

func formatReadyTasks(data []byte) string {
	var tasks []map[string]interface{}
	if err := json.Unmarshal(data, &tasks); err != nil {
		return string(data)
	}

	if len(tasks) == 0 {
		return "No tasks available right now."
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Available tasks (%d)\n\n", len(tasks)))

	// Group tasks by run for clarity
	byRun := map[string][]map[string]interface{}{}
	runOrder := []string{}
	for _, t := range tasks {
		runNum := jsonID(t["run_id"])
		if _, seen := byRun[runNum]; !seen {
			runOrder = append(runOrder, runNum)
		}
		byRun[runNum] = append(byRun[runNum], t)
	}

	for _, runNum := range runOrder {
		b.WriteString(fmt.Sprintf("── Run #%s ──\n", runNum))
		for _, t := range byRun[runNum] {
			id, _ := t["id"].(string)
			prompt, _ := t["prompt"].(string)
			mode, _ := t["mode"].(string)
			deps, _ := t["depends_on"].(string)
			instanceKey, _ := t["instance_key"].(string)
			seq, _ := t["seq"].(float64)

			b.WriteString(fmt.Sprintf("  → #%d [%s]", int(seq), id))
			if instanceKey != "" {
				b.WriteString(fmt.Sprintf("  instance:%s", instanceKey))
			}
			if mode == "assisted" {
				b.WriteString("  [assisted]")
			}
			b.WriteString("\n")
			if deps != "" {
				b.WriteString(fmt.Sprintf("    upstream: %s ✓\n", deps))
			}
			b.WriteString(fmt.Sprintf("    \"%s\"\n", truncate(prompt, 120)))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// formatRequirements renders the task environment requirements for display.
// Returns empty string if no requirements declared.
func formatRequirements(reqRaw string) string {
	if reqRaw == "" {
		return ""
	}
	var reqs map[string]interface{}
	if json.Unmarshal([]byte(reqRaw), &reqs) != nil || len(reqs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n── Environment Requirements ─────────────────\n")
	b.WriteString("Before claiming, verify your environment has these. You can use bash to check.\n\n")

	// Render categories in a consistent order
	categoryOrder := []string{"tools", "packages", "mcp_servers", "env_vars", "files", "network", "resources", "custom"}
	seen := map[string]bool{}

	for _, cat := range categoryOrder {
		if v, ok := reqs[cat]; ok {
			writeRequirementCategory(&b, cat, v)
			seen[cat] = true
		}
	}
	// Any other keys not in the standard list
	for k, v := range reqs {
		if !seen[k] {
			writeRequirementCategory(&b, k, v)
		}
	}

	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

func writeRequirementCategory(b *strings.Builder, name string, value interface{}) {
	b.WriteString(fmt.Sprintf("  %s:\n", name))
	switch v := value.(type) {
	case map[string]interface{}:
		for k, val := range v {
			b.WriteString(fmt.Sprintf("    %s: %v\n", k, val))
		}
	case []interface{}:
		for _, item := range v {
			b.WriteString(fmt.Sprintf("    - %v\n", item))
		}
	default:
		b.WriteString(fmt.Sprintf("    %v\n", v))
	}
}

// formatOutputsSchema renders the named outputs schema for display.
// Returns empty string if no outputs declared.
// formatAssignmentSchema renders the assign_to and require_role
// restrictions on a task. Both are optional — when neither is set the
// task is open to any citizen and this function returns "" so the
// formatter doesn't show any assignment box at all.
func formatAssignmentSchema(assignTo []string, requireRole string) string {
	if len(assignTo) == 0 && requireRole == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n── Access ──────────────────────────────────\n")
	if len(assignTo) > 0 {
		if len(assignTo) == 1 {
			b.WriteString(fmt.Sprintf("Assigned to: %s\n", assignTo[0]))
		} else {
			b.WriteString(fmt.Sprintf("Assigned to (any of): %s\n", strings.Join(assignTo, ", ")))
		}
	}
	if requireRole != "" {
		b.WriteString(fmt.Sprintf("Requires role: %s\n", requireRole))
	}
	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

// formatArtifactsSchema renders the reads_artifacts and writes_artifacts
// declarations on a task. Either or both can be empty. Returns "" if
// nothing to show.
func formatArtifactsSchema(reads, writes []string) string {
	if len(reads) == 0 && len(writes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n── Artifacts ───────────────────────────────\n")
	if len(writes) > 0 {
		b.WriteString("Writes (declared upper bound — submit via artifacts_json):\n")
		for _, p := range writes {
			b.WriteString(fmt.Sprintf("  - %s\n", p))
		}
	}
	if len(reads) > 0 {
		if len(writes) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Reads (resolved automatically into the prompt):\n")
		for _, p := range reads {
			b.WriteString(fmt.Sprintf("  - %s\n", p))
		}
	}
	if len(writes) > 0 {
		b.WriteString("\nExample:\n")
		b.WriteString("  artifacts_json='{\"" + writes[0] + "\":\"new file content here\"}'\n")
	}
	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

// artifactInlineLimit caps how many bytes of a single artifact we inline
// in a claim/task-detail response. Files smaller than this are shown in
// full; larger files are truncated with a footer telling the caller to
// use enju_get_artifact for the full content.
//
// 4 KiB is enough for most source files, configs, READMEs, prompt
// templates — the kinds of artifacts citizens actually need to read in
// order to do their work — without blowing up the response on a large
// dataset accidentally getting declared as a read.
const artifactInlineLimit = 4096

// formatResolvedArtifactsBlock renders the Resolved Artifacts box for
// a claim/task-detail response. It has two parts:
//
//   - A warning section listing any declared reads_artifacts paths
//     that are missing from disk. Each is shown with a ⚠ prefix.
//     Missing paths can happen legitimately after a cascade rollback
//     that deleted the artifact, or when a YAML author declared a
//     read to a path that was never written.
//
//   - A content section listing each present artifact's content,
//     inlined so the claimer doesn't need a separate get_artifact
//     round trip.
//
// The caller should not call this function when both `artifacts` and
// `missing` are empty — the Resolved Artifacts block is meaningless
// in that case and should simply be omitted from the response.
func formatResolvedArtifactsBlock(artifacts map[string]interface{}, missing []string) string {
	if len(artifacts) == 0 && len(missing) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("── Resolved Artifacts ──────────────────────\n")

	// Missing section first so the warning is visually prominent —
	// if the reader ignores the block entirely they still see the
	// warning before scrolling to the content.
	if len(missing) > 0 {
		sortedMissing := append([]string(nil), missing...)
		sortStrings(sortedMissing)
		for _, p := range sortedMissing {
			b.WriteString(fmt.Sprintf("⚠ %s (missing — artifact does not exist)\n", p))
		}
		if len(artifacts) > 0 {
			b.WriteString("\n")
		}
	}

	// Present section — render each artifact's content inline.
	// Entries are sorted so the output is deterministic across calls
	// (Go map iteration is randomized).
	paths := make([]string, 0, len(artifacts))
	for p := range artifacts {
		paths = append(paths, p)
	}
	sortStrings(paths)

	for i, path := range paths {
		raw := artifacts[path]
		content, _ := raw.(string)

		truncated := false
		display := content
		if len(display) > artifactInlineLimit {
			display = display[:artifactInlineLimit]
			truncated = true
		}

		header := fmt.Sprintf("▸ %s (%d chars", path, len(content))
		if truncated {
			header += ", truncated — use enju_get_artifact for full content"
		}
		header += ")"

		b.WriteString(header)
		b.WriteString("\n\n")

		// Indent the content two spaces so it visually nests under the
		// header. Splitting on "\n" and rejoining with "\n  " is enough
		// — the leading two spaces are added before the first line
		// separately.
		indented := "  " + strings.ReplaceAll(display, "\n", "\n  ")
		b.WriteString(indented)
		if !strings.HasSuffix(display, "\n") {
			b.WriteString("\n")
		}

		// Blank line between entries (but not after the last one).
		if i < len(paths)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

// sortStrings sorts a slice of strings in place. Tiny helper to avoid
// pulling sort into format.go for a single call site.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// stringSliceFromAny extracts a []string from a JSON-decoded value
// (which is []interface{} when it came through json.Unmarshal).
func stringSliceFromAny(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch xs := v.(type) {
	case []string:
		return xs
	case []interface{}:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// truncateRunes returns a rune-aware truncation of s to at most max
// runes. Falls back to s unchanged if the string already fits. Used to
// keep fixed-width dashboard boxes from overflowing when the user has
// a long display name.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// humanizeRelTime formats a timestamp as a short relative duration
// suitable for list views. Falls back to the full string on parse errors.
//
// Examples: "5s ago", "2m ago", "3h ago", "yesterday", "4d ago",
// "2026-04-13" for anything older than 30 days.
func humanizeRelTime(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return t.Format("2006-01-02")
	case d < 60*time.Second:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < 60*time.Minute:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

func formatOutputsSchema(outputsRaw string) string {
	if outputsRaw == "" {
		return ""
	}
	var outputs map[string]interface{}
	if json.Unmarshal([]byte(outputsRaw), &outputs) != nil || len(outputs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n── Expected Outputs ─────────────────────────\n")
	b.WriteString("This task has named outputs. Submit using outputs_json:\n\n")
	for name, rawSpec := range outputs {
		switch v := rawSpec.(type) {
		case string:
			b.WriteString(fmt.Sprintf("  %s — %s\n", name, v))
		case map[string]interface{}:
			desc, _ := v["Description"].(string)
			file, _ := v["File"].(string)
			format, _ := v["Format"].(string)
			line := fmt.Sprintf("  %s — %s", name, desc)
			if file != "" {
				line += fmt.Sprintf("  →  %s", file)
			}
			if format != "" {
				line += fmt.Sprintf(" (%s)", format)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\nExample:\n")
	b.WriteString("  outputs_json='{\"output_name_1\":\"content here\",\"output_name_2\":\"more content\"}'\n")
	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

func formatClaimResult(claimData []byte, inputsData []byte) string {
	var claim map[string]interface{}
	if err := json.Unmarshal(claimData, &claim); err != nil {
		return string(claimData)
	}

	// Check for error
	if errMsg, ok := claim["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to claim: %s", errMsg)
	}

	var b strings.Builder

	task, _ := claim["task"].(map[string]interface{})
	deadline, _ := claim["deadline"].(string)
	taskID, _ := task["id"].(string)
	prompt, _ := task["prompt"].(string)
	mode, _ := task["mode"].(string)
	deps, _ := task["depends_on"].(string)
	seq, _ := task["seq"].(float64)

	b.WriteString(fmt.Sprintf("✓ Claimed: #%d [%s]\n", int(seq), taskID))
	b.WriteString(fmt.Sprintf("  Deadline: %s\n", deadline))
	if mode == "assisted" {
		b.WriteString("  Mode: assisted (your input will be structured by LLM)\n")
	}

	// Show environment requirements if present
	if reqRaw, ok := task["requirements"].(string); ok && reqRaw != "" {
		b.WriteString(formatRequirements(reqRaw))
	}

	// Show outputs schema if present
	if outputsRaw, ok := task["outputs"].(string); ok && outputsRaw != "" {
		b.WriteString(formatOutputsSchema(outputsRaw))
	}

	// Show access restrictions (assign_to, require_role) if any.
	assignTo := stringSliceFromAny(task["assign_to"])
	requireRole, _ := task["require_role"].(string)
	if s := formatAssignmentSchema(assignTo, requireRole); s != "" {
		b.WriteString(s)
	}

	// Show artifact reads/writes schema if present
	reads := stringSliceFromAny(task["reads_artifacts"])
	writes := stringSliceFromAny(task["writes_artifacts"])
	if s := formatArtifactsSchema(reads, writes); s != "" {
		b.WriteString(s)
	}

	b.WriteString("\n")

	// Show resolved prompt if inputs available
	var resolvedArtifacts map[string]interface{}
	var missingArtifacts []string
	if inputsData != nil {
		var inputs map[string]interface{}
		if json.Unmarshal(inputsData, &inputs) == nil {
			resolvedArtifacts, _ = inputs["artifacts"].(map[string]interface{})
			missingArtifacts = stringSliceFromAny(inputs["missing_artifacts"])

			if resolved, ok := inputs["resolved_prompt"].(string); ok && resolved != "" && resolved != prompt {
				b.WriteString("── Prompt (with upstream context) ──────────\n")
				b.WriteString(resolved)
				b.WriteString("\n────────────────────────────────────────────\n")
			} else {
				b.WriteString("── Prompt ──────────────────────────────────\n")
				b.WriteString(prompt)
				b.WriteString("\n────────────────────────────────────────────\n")
			}

			// Show upstream sources
			if inputMap, ok := inputs["inputs"].(map[string]interface{}); ok && len(inputMap) > 0 {
				b.WriteString("\nUpstream results available from: ")
				var sources []string
				for k := range inputMap {
					sources = append(sources, k)
				}
				b.WriteString(strings.Join(sources, ", "))
				b.WriteString("\n")
			}

			// Inline the actual content of any artifacts read by the
			// task, so the claimer doesn't need a separate get_artifact
			// round trip. Both explicit reads_artifacts declarations
			// and {{artifact:path}} template references land here.
			// Missing declared reads get a warning line in the same
			// block so the claimer can see the contract was broken.
			if len(resolvedArtifacts) > 0 || len(missingArtifacts) > 0 {
				b.WriteString(formatResolvedArtifactsBlock(resolvedArtifacts, missingArtifacts))
			}
		}
	} else {
		if deps == "" {
			b.WriteString("── Prompt ──────────────────────────────────\n")
			b.WriteString(prompt)
			b.WriteString("\n────────────────────────────────────────────\n")
		}
	}

	b.WriteString("\nSubmit your result when ready.")
	return b.String()
}

// formatInvalidateResult renders the response from /tasks/{id}/invalidate.
// Shows the target, cascaded descendants, rolled-back artifacts, and
// the reason if provided.
func formatInvalidateResult(data []byte, taskID string) string {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data)
	}
	if errMsg, ok := result["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to invalidate %s: %s", taskID, errMsg)
	}

	var b strings.Builder
	changed, _ := result["changed"].(float64)
	reason, _ := result["reason"].(string)
	descendants, _ := result["descendants"].([]interface{})
	readers, _ := result["artifact_readers"].([]interface{})
	rollbacks, _ := result["artifacts_rolled_back"].([]interface{})

	b.WriteString(fmt.Sprintf("✓ Invalidated: %s\n", taskID))
	if reason != "" {
		b.WriteString(fmt.Sprintf("  Reason: %s\n", reason))
	}

	// Compose a summary line that distinguishes the two cascade
	// categories so the user can see at a glance why each task was
	// affected.
	b.WriteString(fmt.Sprintf("\n%d task(s) changed state (target", int(changed)))
	if len(descendants) > 0 {
		b.WriteString(fmt.Sprintf(" + %d task descendant(s)", len(descendants)))
	}
	if len(readers) > 0 {
		b.WriteString(fmt.Sprintf(" + %d artifact-reader descendant(s)", len(readers)))
	}
	b.WriteString(").\n")

	if len(descendants) > 0 {
		b.WriteString("\nCascaded to PENDING (will re-promote to READY as upstreams re-complete):\n")
		for _, d := range descendants {
			b.WriteString(fmt.Sprintf("  ↩ %v\n", d))
		}
	}

	// Cross-run artifact readers. These are tasks in other runs that
	// read an artifact whose writer is being invalidated. They're now
	// re-opened (no longer accepted) so they can be re-claimed with
	// the rolled-back artifact state as their new input.
	if len(readers) > 0 {
		b.WriteString("\nArtifact-reader tasks re-opened (now READY for re-claim with updated inputs):\n")
		for _, r := range readers {
			b.WriteString(fmt.Sprintf("  ↩ %v\n", r))
		}
	}

	// Artifact rollback summary. Describes what happened to each file
	// the invalidated tasks had in writes_artifacts.
	if len(rollbacks) > 0 {
		b.WriteString("\nArtifacts rolled back:\n")
		for _, r := range rollbacks {
			rb, _ := r.(map[string]interface{})
			path, _ := rb["path"].(string)
			if rb["deleted"] == true {
				b.WriteString(fmt.Sprintf("  ✗ %s (no prior writer — deleted)\n", path))
			} else {
				restoredFrom, _ := rb["restored_from_task"].(string)
				b.WriteString(fmt.Sprintf("  ↩ %s ← %s\n", path, restoredFrom))
			}
		}
	}

	b.WriteString(fmt.Sprintf("\nTarget %s is now READY and can be re-claimed.\n", taskID))
	b.WriteString("The previous result is preserved in git history.\n")
	return b.String()
}

func formatSubmitResult(data []byte, taskID string) string {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data)
	}

	if errMsg, ok := result["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to submit: %s", errMsg)
	}

	var b strings.Builder

	status, _ := result["status"].(string)
	newlyReady, _ := result["newly_ready"].(float64)
	completed, _ := result["run_completed"].(bool)
	artifactsWritten, _ := result["artifacts_written"].([]interface{})

	b.WriteString(fmt.Sprintf("✓ Result accepted: %s\n", taskID))

	if len(artifactsWritten) > 0 {
		b.WriteString("\nArtifacts written:\n")
		for _, p := range artifactsWritten {
			b.WriteString(fmt.Sprintf("  - %v\n", p))
		}
	}

	if completed {
		b.WriteString("\n🎉 Run completed! All tasks are done.\n")
	} else if newlyReady > 0 {
		b.WriteString(fmt.Sprintf("\nImpact: %d new task(s) unlocked and ready for work.\n", int(newlyReady)))
	}

	_ = status
	return b.String()
}

// formatArtifactList renders the list of artifacts in a project.
func formatArtifactList(data []byte, projectID int64) string {
	var artifacts []map[string]interface{}
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return string(data)
	}

	if len(artifacts) == 0 {
		return fmt.Sprintf("No artifacts in project #%d.", projectID)
	}

	// Compute column widths for clean alignment. Cap path width so a
	// single huge path doesn't blow up the layout.
	const pathColCap = 40
	pathW, taskW := 0, 0
	for _, a := range artifacts {
		if p, _ := a["path"].(string); len(p) > pathW {
			pathW = len(p)
		}
		if t, _ := a["last_task_id"].(string); len(t) > taskW {
			taskW = len(t)
		}
	}
	if pathW > pathColCap {
		pathW = pathColCap
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Artifacts in project #%d (%d total):\n\n", projectID, len(artifacts)))
	for _, a := range artifacts {
		path, _ := a["path"].(string)
		taskID, _ := a["last_task_id"].(string)
		updatedAt, _ := a["updated_at"].(string)

		// Truncate path on the left if it overflows.
		displayPath := path
		if len(displayPath) > pathColCap {
			displayPath = "…" + displayPath[len(displayPath)-pathColCap+1:]
		}

		b.WriteString(fmt.Sprintf("  %-*s   %-*s   %s\n",
			pathW, displayPath,
			taskW, taskID,
			humanizeRelTime(updatedAt),
		))
	}
	return b.String()
}

// formatArtifactDetail renders the content + provenance of one artifact.
func formatArtifactDetail(data []byte) string {
	var a map[string]interface{}
	if err := json.Unmarshal(data, &a); err != nil {
		return string(data)
	}

	if errMsg, ok := a["error"].(string); ok {
		return "✗ " + errMsg
	}

	var b strings.Builder
	path, _ := a["path"].(string)
	content, _ := a["content"].(string)
	writer, _ := a["last_writer"].(string)
	taskID, _ := a["last_task_id"].(string)
	updatedAt, _ := a["updated_at"].(string)

	b.WriteString(fmt.Sprintf("Artifact: %s\n", path))
	if writer != "" || taskID != "" {
		b.WriteString(fmt.Sprintf("Last write: %s by %s", taskID, writer))
		if updatedAt != "" {
			b.WriteString(" at " + updatedAt)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n--- content ---\n")
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func formatRunStatus(runData []byte, tasksData []byte) string {
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return string(runData)
	}

	var tasks []map[string]interface{}
	if err := json.Unmarshal(tasksData, &tasks); err != nil {
		return string(tasksData)
	}

	var b strings.Builder

	name, _ := run["name"].(string)
	state, _ := run["state"].(string)
	projectID := jsonID(run["project_id"])
	seq, _ := run["seq"].(float64)

	// Count by state
	counts := map[string]int{}
	for _, t := range tasks {
		s, _ := t["state"].(string)
		counts[s]++
	}
	total := len(tasks)
	done := counts["accepted"]

	b.WriteString(fmt.Sprintf("Project #%s → Run #%d: %s\n", projectID, int(seq), name))
	b.WriteString(fmt.Sprintf("Status: %s    Progress: %d/%d\n", state, done, total))
	b.WriteString(fmt.Sprintf("%s\n\n", progressBar(done, total, 30)))

	// Build task list
	b.WriteString("Tasks:\n\n")
	for _, t := range tasks {
		tid, _ := t["id"].(string)
		tstate, _ := t["state"].(string)
		claimedBy, _ := t["claimed_by"].(string)
		deps, _ := t["depends_on"].(string)
		prompt, _ := t["prompt"].(string)

		seq, _ := t["seq"].(float64)
		icon := stateIcon(tstate)

		// Task ID line with number for quick reference
		b.WriteString(fmt.Sprintf("  %s #%d [%s] %s\n", icon, int(seq), tid, stateLabel(tstate)))

		switch tstate {
		case "accepted":
			if claimedBy != "" {
				b.WriteString(fmt.Sprintf("    Completed by: %s\n", claimedBy))
			}
		case "claimed":
			if claimedBy != "" {
				b.WriteString(fmt.Sprintf("    Working: %s\n", claimedBy))
			}
		case "pending":
			if deps != "" {
				b.WriteString(fmt.Sprintf("    Blocked by: %s\n", deps))
			}
		}

		// Prompt preview — hide template vars for blocked tasks
		if prompt != "" {
			preview := truncate(prompt, 100)
			b.WriteString(fmt.Sprintf("    \"%s\"\n", preview))
		}

		b.WriteString("\n")
	}

	b.WriteString("Tip: Use enju_get_task(task_id=\"...\") to see full details of a task.")

	return b.String()
}

func formatTaskDetail(taskData []byte, inputsData []byte) string {
	var task map[string]interface{}
	if err := json.Unmarshal(taskData, &task); err != nil {
		return string(taskData)
	}

	if errMsg, ok := task["error"].(string); ok {
		return fmt.Sprintf("✗ Task not found: %s", errMsg)
	}

	var b strings.Builder

	id, _ := task["id"].(string)
	runID := jsonID(task["run_id"])
	state, _ := task["state"].(string)
	prompt, _ := task["prompt"].(string)
	userPrompt, _ := task["user_prompt"].(string)
	claimedBy, _ := task["claimed_by"].(string)
	deps, _ := task["depends_on"].(string)
	ref, _ := task["ref"].(string)
	instanceKey, _ := task["instance_key"].(string)
	action, _ := task["action"].(string)

	seq, _ := task["seq"].(float64)
	icon := stateIcon(state)
	b.WriteString(fmt.Sprintf("Task #%d: %s %s\n", int(seq), id, icon))
	b.WriteString(fmt.Sprintf("  Run:  #%s\n", runID))
	b.WriteString(fmt.Sprintf("  Action:   %s\n", friendlyActionLabel(action)))
	b.WriteString(fmt.Sprintf("  State:    %s\n", stateLabel(state)))
	if instanceKey != "" {
		b.WriteString(fmt.Sprintf("  Instance: %s\n", instanceKey))
	}
	if ref != "" {
		b.WriteString(fmt.Sprintf("  Ref:      %s\n", ref))
	}
	if claimedBy != "" {
		b.WriteString(fmt.Sprintf("  Claimed:  %s\n", claimedBy))
	}
	if deps != "" {
		b.WriteString(fmt.Sprintf("  Depends:  %s\n", deps))
	}

	// Show environment requirements if present
	if reqRaw, ok := task["requirements"].(string); ok {
		b.WriteString(formatRequirements(reqRaw))
	}

	// Show named outputs schema if present
	if outputsRaw, ok := task["outputs"].(string); ok {
		b.WriteString(formatOutputsSchema(outputsRaw))
	}

	// Show access restrictions (assign_to, require_role) if any.
	assignTo := stringSliceFromAny(task["assign_to"])
	requireRole, _ := task["require_role"].(string)
	if s := formatAssignmentSchema(assignTo, requireRole); s != "" {
		b.WriteString(s)
	}

	// Show artifact reads/writes schema if present
	reads := stringSliceFromAny(task["reads_artifacts"])
	writes := stringSliceFromAny(task["writes_artifacts"])
	if s := formatArtifactsSchema(reads, writes); s != "" {
		b.WriteString(s)
	}

	// Show prompt
	b.WriteString("\n── Prompt ──────────────────────────────────\n")
	if userPrompt != "" {
		b.WriteString(fmt.Sprintf("[User prompt]: %s\n\n", userPrompt))
	}
	b.WriteString(prompt)
	b.WriteString("\n────────────────────────────────────────────\n")

	// Show resolved prompt if inputs available
	if inputsData != nil {
		var inputs map[string]interface{}
		if json.Unmarshal(inputsData, &inputs) == nil {
			if resolved, ok := inputs["resolved_prompt"].(string); ok && resolved != "" && resolved != prompt {
				b.WriteString("\n── Resolved (with upstream data + artifacts) ──\n")
				b.WriteString(resolved)
				b.WriteString("\n────────────────────────────────────────────\n")
			}

			if inputMap, ok := inputs["inputs"].(map[string]interface{}); ok && len(inputMap) > 0 {
				b.WriteString("\n── Upstream Results ────────────────────────\n")
				for depID, depResult := range inputMap {
					b.WriteString(fmt.Sprintf("\nFrom %s:\n", depID))
					if depMap, ok := depResult.(map[string]interface{}); ok {
						if content, ok := depMap["content"].(string); ok {
							if len(content) > 500 {
								b.WriteString(content[:500] + "...(truncated)")
							} else {
								b.WriteString(content)
							}
						}
					}
					b.WriteString("\n")
				}
				b.WriteString("────────────────────────────────────────────\n")
			}

			// Surface artifact contents inline so callers don't need a
			// separate get_artifact round trip. Missing declared reads
			// get a warning line in the same block.
			artMap, _ := inputs["artifacts"].(map[string]interface{})
			missingArts := stringSliceFromAny(inputs["missing_artifacts"])
			if len(artMap) > 0 || len(missingArts) > 0 {
				b.WriteString("\n")
				b.WriteString(formatResolvedArtifactsBlock(artMap, missingArts))
			}
		}
	}

	return b.String()
}

func formatCreateRun(data []byte) string {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data)
	}

	if errMsg, ok := result["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to create run: %s", errMsg)
	}

	name, _ := result["name"].(string)
	projectID := jsonID(result["project_id"])
	seq, _ := result["seq"].(float64)
	taskCount, _ := result["task_count"].(float64)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("✓ Run created in project #%s as run #%d: %s\n", projectID, int(seq), name))
	b.WriteString(fmt.Sprintf("  Tasks: %d\n", int(taskCount)))
	b.WriteString(fmt.Sprintf("\nUse enju_run_status(project_id=%s, run_id=%d) or enju_list_ready_tasks to see tasks.", projectID, int(seq)))
	return b.String()
}

// --- Helpers ---

// formatProfile renders a citizen's profile — their stable handle,
// display name, role, email, and summary stats. This is what
// enju_my_profile returns so citizens can discover their own username
// without spelunking provenance.
func formatProfile(data []byte) string {
	var c map[string]interface{}
	if err := json.Unmarshal(data, &c); err != nil {
		return string(data)
	}
	if errMsg, ok := c["error"].(string); ok {
		return "✗ " + errMsg
	}

	username, _ := c["username"].(string)
	name, _ := c["name"].(string)
	email, _ := c["email"].(string)
	role, _ := c["role"].(string)
	score, _ := c["score"].(float64)
	completed, _ := c["tasks_completed"].(float64)

	var b strings.Builder
	b.WriteString("── Profile ─────────────────────────────────\n")
	b.WriteString(fmt.Sprintf("Username:   @%s\n", username))
	if name != "" && !strings.EqualFold(name, username) {
		b.WriteString(fmt.Sprintf("Display:    %s\n", name))
	}
	if email != "" {
		b.WriteString(fmt.Sprintf("Email:      %s\n", email))
	}
	if role != "" {
		b.WriteString(fmt.Sprintf("Role:       %s\n", role))
	}
	b.WriteString(fmt.Sprintf("Score:      %.0f\n", score))
	b.WriteString(fmt.Sprintf("Completed:  %.0f task(s)\n", completed))
	b.WriteString("────────────────────────────────────────────\n\n")
	b.WriteString(fmt.Sprintf("Others reference you as: %s  (this is what goes in `assign_to:`)\n", username))
	return b.String()
}

func formatDashboard(data []byte) string {
	var dash map[string]interface{}
	if err := json.Unmarshal(data, &dash); err != nil {
		return string(data)
	}

	if errMsg, ok := dash["error"].(string); ok {
		return fmt.Sprintf("✗ %s", errMsg)
	}

	citizen, _ := dash["citizen"].(map[string]interface{})
	activeTasks, _ := dash["active_tasks"].([]interface{})
	recentTasks, _ := dash["recent_tasks"].([]interface{})

	name, _ := citizen["name"].(string)
	username, _ := citizen["username"].(string)
	role, _ := citizen["role"].(string)
	score, _ := citizen["score"].(float64)
	completed, _ := citizen["tasks_completed"].(float64)
	timedOut, _ := citizen["tasks_timed_out"].(float64)

	// Render the header as "Display Name (@username)" when they differ,
	// or just "@username" when the display name is the same as the
	// username (which happens when the caller registered with a simple
	// lowercase name like -name claude). This way the stable handle is
	// always visible — the thing others use in assign_to — and the
	// display name only shows when it adds something beyond the handle.
	var header string
	switch {
	case username != "" && name != "" && !strings.EqualFold(name, username):
		header = fmt.Sprintf("%s (@%s)", name, username)
	case username != "":
		header = "@" + username
	default:
		header = name
	}
	if role != "" && role != "citizen" {
		header += " · " + role
	}

	var b strings.Builder

	// Profile
	const dashBorderTop = "┌─────────────────────────────────────────────┐\n"
	const dashBorderBot = "└─────────────────────────────────────────────┘\n"
	b.WriteString(dashBorderTop)
	b.WriteString(fmt.Sprintf("│  %-32s  Score: %-4.0f │\n", truncateRunes(header, 32), score))
	b.WriteString(fmt.Sprintf("│  Tasks: %.0f completed", completed))
	if timedOut > 0 {
		b.WriteString(fmt.Sprintf(", %.0f timed out", timedOut))
	}
	b.WriteString(fmt.Sprintf(", %d active", len(activeTasks)))
	b.WriteString("          │\n")
	b.WriteString(dashBorderBot)

	// Active claims
	if len(activeTasks) > 0 {
		b.WriteString("\nActive claims:\n")
		for _, t := range activeTasks {
			tm, _ := t.(map[string]interface{})
			tid, _ := tm["id"].(string)
			seq, _ := tm["seq"].(float64)
			runID := jsonID(tm["run_id"])
			b.WriteString(fmt.Sprintf("  ⏳ #%d [%s]    run #%s\n", int(seq), tid, runID))
		}
	} else {
		b.WriteString("\nNo active claims. Use enju_list_ready_tasks to find work.\n")
	}

	// Recent completions
	if len(recentTasks) > 0 {
		b.WriteString("\nRecent completions:\n")
		for _, t := range recentTasks {
			tm, _ := t.(map[string]interface{})
			tid, _ := tm["id"].(string)
			seq, _ := tm["seq"].(float64)
			runID := jsonID(tm["run_id"])
			b.WriteString(fmt.Sprintf("  ✓ #%d [%s]    run #%s\n", int(seq), tid, runID))
		}
	}

	return b.String()
}

// jsonID extracts an ID that could be a string or a number from JSON.
func jsonID(v interface{}) string {
	switch id := v.(type) {
	case float64:
		return fmt.Sprintf("%d", int64(id))
	case string:
		return id
	default:
		return fmt.Sprintf("%v", v)
	}
}

func stateIcon(state string) string {
	switch state {
	case "accepted", "completed":
		return "✓"
	case "ready":
		return "→"
	case "claimed", "running":
		return "⏳"
	case "pending":
		return "○"
	case "invalid", "invalidated", "rejected":
		return "✗"
	default:
		return "?"
	}
}

func friendlyActionLabel(action string) string {
	switch action {
	case "answer":
		return "Answer a question"
	case "contribute":
		return "Share your input"
	case "compute":
		return "Run a computation"
	case "review":
		return "Review work"
	case "vote":
		return "Vote on a decision"
	default:
		return action
	}
}


func stateLabel(state string) string {
	switch state {
	case "accepted":
		return "completed"
	case "ready":
		return "available — claim this task"
	case "claimed", "running":
		return "in progress"
	case "pending":
		return "blocked"
	case "invalid", "invalidated":
		return "invalidated"
	default:
		return state
	}
}

func progressBar(done, total, width int) string {
	if total == 0 {
		return ""
	}
	filled := (done * width) / total
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	pct := (done * 100) / total
	return fmt.Sprintf("%s %d%%", bar, pct)
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
