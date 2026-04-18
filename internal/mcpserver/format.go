package mcpserver

import (
	"encoding/json"
	"fmt"
	"sort"
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

		// Surface git remote + push status so silent divergence
		// from the remote is visible ambiently. Added in iteration 6
		// after the iteration 4 feedback flagged the silent-failure
		// operational hazard.
		if remote, _ := p["remote_url"].(string); remote != "" {
			pushErr, _ := p["last_push_error"].(string)
			lastPush, _ := p["last_push_at"].(string)
			if pushErr != "" {
				b.WriteString(fmt.Sprintf("      remote: %s  ⚠ last push failed: %s\n", remote, pushErr))
				if lastPush != "" {
					b.WriteString(fmt.Sprintf("      last attempt: %s — use enju_project_remote_status for details, enju_project_sync to retry\n", lastPush))
				}
			} else {
				line := "      remote: " + remote
				if lastPush != "" {
					line += "  ✓ last push: " + lastPush
				}
				b.WriteString(line + "\n")
			}
		}
	}
	b.WriteString("\nTip: A project holds many runs over time. Use enju_list_runs to see all runs, or filter to a project.")
	return b.String()
}

// formatProjectRemoteStatus renders the live remote-status diagnostic
// returned by GET /projects/{id}/remote/status. Renders different
// guidance for ahead vs diverged so the user knows whether a plain
// sync is safe or whether force-push would be destructive.
func formatProjectRemoteStatus(data []byte) string {
	var r map[string]interface{}
	if err := json.Unmarshal(data, &r); err != nil {
		return string(data)
	}
	if errMsg, ok := r["error"].(string); ok {
		return "✗ " + errMsg
	}
	projectID := jsonID(r["project_id"])
	remote, _ := r["remote_url"].(string)
	status, _ := r["status"].(string)
	ahead := intFromJSON(r["ahead_by"])
	behind := intFromJSON(r["behind_by"])

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Project #%s remote status\n", projectID))
	if remote == "" {
		b.WriteString("  (no remote configured)\n")
		return b.String()
	}
	// For init'd projects, show workspace path + actual git remote separately.
	if workspace, ok := r["workspace"].(string); ok && workspace != "" {
		b.WriteString("  workspace: " + workspace + " (adopted folder)\n")
		b.WriteString("  git remote: " + remote + "\n")
	} else if strings.HasPrefix(remote, "/") && !strings.HasSuffix(remote, ".git") {
		b.WriteString("  workspace: " + remote + " (local, no git remote)\n")
	} else {
		b.WriteString("  remote:    " + remote + "\n")
	}
	b.WriteString("  status:    " + humanRemoteStatus(status, ahead, behind) + "\n")
	if localHead, ok := r["local_head"].(string); ok && localHead != "" {
		short := localHead
		if len(short) > 8 {
			short = short[:8]
		}
		b.WriteString("  local:     " + short + "\n")
	}
	if remoteHead, ok := r["remote_head"].(string); ok && remoteHead != "" {
		short := remoteHead
		if len(short) > 8 {
			short = short[:8]
		}
		b.WriteString("  remote @:  " + short + "\n")
	}
	if lastPush, ok := r["last_push_at"].(string); ok && lastPush != "" {
		b.WriteString("  last push: " + lastPush + "\n")
	}
	if pushErr, ok := r["last_push_error"].(string); ok && pushErr != "" {
		b.WriteString("  ⚠ error:   " + pushErr + "\n")
	}
	if remoteErr, ok := r["remote_error"].(string); ok && remoteErr != "" {
		b.WriteString("  ⚠ unreachable: " + remoteErr + "\n")
	}

	// Action guidance — spelled out so the user knows what to do
	// next without having to interpret the status code.
	switch status {
	case "ahead":
		b.WriteString("  → enju_project_sync will fast-forward the remote safely\n")
	case "behind":
		b.WriteString("  → nothing to push; remote is already ahead of local\n")
	case "diverged":
		b.WriteString("  ⚠ local and remote both have unique commits\n")
		b.WriteString("  → fetch + merge/rebase manually, or enju_project_sync(force=true) to overwrite the remote (destructive)\n")
	case "unrelated":
		b.WriteString("  ⚠ local and remote share no history\n")
		b.WriteString("  → only enju_project_sync(force=true) will succeed, and it will replace the remote\n")
	case "remote_empty":
		b.WriteString("  → enju_project_sync will populate the empty remote\n")
	}
	return b.String()
}

// formatProjectSyncResult renders the outcome of a sync attempt,
// including the non-error "refused" and "noop" paths where the
// server declined to push for safety reasons.
func formatProjectSyncResult(data []byte) string {
	var r map[string]interface{}
	if err := json.Unmarshal(data, &r); err != nil {
		return string(data)
	}
	projectID := jsonID(r["project_id"])
	remote, _ := r["remote_url"].(string)
	result, _ := r["result"].(string)
	message, _ := r["message"].(string)

	switch result {
	case "pushed":
		return fmt.Sprintf("✓ Project #%s pushed to %s", projectID, remote)
	case "force_pushed":
		return fmt.Sprintf("✓ Project #%s force-pushed to %s (remote was overwritten)", projectID, remote)
	case "noop":
		return fmt.Sprintf("• Project #%s: %s", projectID, message)
	case "refused":
		return fmt.Sprintf("⚠ Project #%s refused: %s", projectID, message)
	case "failed":
		if errMsg, ok := r["error"].(string); ok {
			return fmt.Sprintf("✗ Project #%s sync failed: %s", projectID, errMsg)
		}
		return fmt.Sprintf("✗ Project #%s sync failed", projectID)
	}
	// Fallback: earlier error shape from other handlers
	if errMsg, ok := r["error"].(string); ok {
		return "✗ " + errMsg
	}
	return fmt.Sprintf("Project #%s sync: %s", projectID, result)
}

// humanRemoteStatus translates the structured RemoteComparison
// status code into a human-readable label with ahead/behind counts
// where applicable.
func humanRemoteStatus(code string, ahead, behind int) string {
	switch code {
	case "in_sync":
		return "in sync"
	case "ahead":
		return fmt.Sprintf("ahead by %d commit(s) — fast-forwardable", ahead)
	case "behind":
		return fmt.Sprintf("behind by %d commit(s) — nothing to push", behind)
	case "diverged":
		return fmt.Sprintf("diverged — local ahead by %d, behind by %d", ahead, behind)
	case "unrelated":
		return "unrelated — local and remote share no history"
	case "remote_empty":
		return "remote has no refs/heads/main yet (new remote)"
	case "local_empty":
		return "local has no HEAD yet"
	case "unreachable":
		return "unreachable — ls-remote failed"
	case "no_remote":
		return "no remote configured"
	default:
		return code
	}
}

// intFromJSON coerces a numeric JSON field (always decoded as
// float64 by encoding/json) into an int. Returns 0 if the field is
// missing or not a number.
func intFromJSON(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
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

// formatReviewingBlock renders the target's content for an
// action:review claim so the reviewer can see what they're
// evaluating without a second enju_get_task call. Mirrors the
// resolved-artifacts block in shape: header line + indented
// content, truncated with a pointer to a richer tool for large
// payloads.
func formatReviewingBlock(reviewing map[string]interface{}) string {
	if len(reviewing) == 0 {
		return ""
	}
	targetID, _ := reviewing["target_def_id"].(string)
	claimedBy, _ := reviewing["claimed_by"].(string)
	content, _ := reviewing["content"].(string)
	commitSHA, _ := reviewing["commit_sha"].(string)
	resultPath, _ := reviewing["result_path"].(string)

	var b strings.Builder
	b.WriteString("── Reviewing ───────────────────────────────\n")

	header := "▸ " + targetID
	if claimedBy != "" {
		header += " (by @" + claimedBy + ")"
	}
	if resultPath != "" {
		b.WriteString(fmt.Sprintf("  Path:   %s/result.md\n", resultPath))
	}
	if commitSHA != "" {
		b.WriteString(fmt.Sprintf("  Commit: %s\n", shortSHA(commitSHA)))
	}
	header += fmt.Sprintf(" — %d chars", len(content))

	truncated := false
	display := content
	if len(display) > artifactInlineLimit {
		display = display[:artifactInlineLimit]
		truncated = true
		header += ", truncated — use enju_get_task for full content"
	}

	b.WriteString(header)
	b.WriteString("\n\n")
	indented := "  " + strings.ReplaceAll(display, "\n", "\n  ")
	b.WriteString(indented)
	if !strings.HasSuffix(display, "\n") {
		b.WriteString("\n")
	}
	_ = truncated
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

	// Collect (name, description, file, format) tuples in
	// stable sort order so the example JSON and the field
	// listing always agree.
	type outField struct {
		name   string
		desc   string
		file   string
		format string
	}
	fields := make([]outField, 0, len(outputs))
	for name, rawSpec := range outputs {
		f := outField{name: name}
		switch v := rawSpec.(type) {
		case string:
			f.desc = v
		case map[string]interface{}:
			f.desc, _ = v["Description"].(string)
			f.file, _ = v["File"].(string)
			f.format, _ = v["Format"].(string)
		}
		fields = append(fields, f)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })

	var b strings.Builder
	b.WriteString("\n── Expected Outputs ─────────────────────────\n")
	b.WriteString("This task has named outputs. Submit using outputs_json:\n\n")
	for _, f := range fields {
		// Start with the name, then add description if
		// present (prefixed with em-dash), then file spec
		// with arrow, then type in parens. The previous
		// implementation always emitted the em-dash even
		// when the description was empty, which produced
		// `name —  (list<string>)` with a double space.
		line := "  " + f.name
		if f.desc != "" {
			line += " — " + f.desc
		}
		if f.file != "" {
			line += "  →  " + f.file
		}
		if f.format != "" {
			line += " (" + f.format + ")"
		}
		b.WriteString(line + "\n")
	}
	// Build a type-aware example JSON object using the
	// task's actual output names so the LLM gets something
	// directly copy-pasteable instead of
	// output_name_1/output_name_2 placeholders.
	var pairs []string
	for _, f := range fields {
		pairs = append(pairs, fmt.Sprintf("%q: %s", f.name, exampleValueForFormat(f.format)))
	}
	b.WriteString("\nExample:\n")
	b.WriteString("  outputs_json='{" + strings.Join(pairs, ", ") + "}'\n")
	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

// exampleValueForFormat returns a JSON-literal example value
// matching the declared format. Used in the outputs_json
// example so the LLM sees a shape-correct template instead
// of a generic "content here" placeholder.
func exampleValueForFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "list<string>":
		return `["item1", "item2"]`
	case "int", "integer":
		return "42"
	case "bool", "boolean":
		return "true"
	case "json":
		return `{"key": "value"}`
	}
	return `"content here"`
}

// formatClaimResult renders the response from claiming a task.
// Optional trailing args: reviewFeedback (JSON from fetchReviewFeedback)
// and previousSubmission (JSON with "content" key).
func formatClaimResult(claimData []byte, inputsData []byte, viewer string, extra ...[]byte) string {
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
	if iterationLabel, _ := task["iteration_label"].(string); iterationLabel != "" {
		b.WriteString(fmt.Sprintf("  Iteration: %s\n", iterationLabel))
	}
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

			// Review tasks: surface the reviewed target's content
			// inline. fetchAndResolveLocally attaches a "reviewing"
			// block to the inputs response for action:review tasks,
			// populated from the local clone at the target's
			// accepted commit_sha. Mirrors the reads_artifacts
			// block — the claimer shouldn't need a second
			// round-trip to see what they're evaluating.
			if reviewing, ok := inputs["reviewing"].(map[string]interface{}); ok {
				b.WriteString(formatReviewingBlock(reviewing))
			}
		}
	}

	// Vote tasks: render the declared options list inline so
	// the voter sees what they're choosing between without a
	// separate task-detail fetch. Independent of the inputs
	// branch because options live on the task record itself,
	// not in the resolved prompt.
	if voteOptsRaw, _ := task["vote_options"].(string); voteOptsRaw != "" {
		if opts := parseVoteOptionsForDisplay(voteOptsRaw); len(opts) > 0 {
			b.WriteString(formatVoteOptionsBlock(opts, task))
		}
	}

	// Multi-citizen voting / review status block — shown on
	// claim so the newly-arrived citizen sees the group state
	// before submitting: who else has voted/reviewed, the live
	// tally, and whether quorum is still outstanding.
	if citizensFloat, _ := task["citizens"].(float64); citizensFloat > 1 {
		citizens := int(citizensFloat)
		minQuorum := 0
		if q, _ := task["min_quorum"].(float64); q > 0 {
			minQuorum = int(q)
		}
		threshold, _ := task["vote_threshold"].(string)
		voteSubs, _ := task["vote_submissions"].([]interface{})
		activeClaimants := stringSliceFromAny(task["active_claimants"])
		state, _ := task["state"].(string)
		taskAction, _ := task["action"].(string)
		visibility, _ := task["visibility"].(string)
		anonymize, _ := task["anonymize"].(bool)
		deadline, _ := task["vote_deadline"].(string)
		deadlineAt, _ := task["vote_deadline_at"].(string)
		b.WriteString(formatVotingBlock(taskAction, citizens, minQuorum, threshold, state, voteSubs, activeClaimants, visibility, viewer, anonymize, deadline, deadlineAt))
	}

	if inputsData == nil {
		if deps == "" {
			b.WriteString("── Prompt ──────────────────────────────────\n")
			b.WriteString(prompt)
			b.WriteString("\n────────────────────────────────────────────\n")
		}
	}

	// Show previous submission + reviewer feedback if this task
	// was bounced back via request_changes.
	var feedbackData, prevData []byte
	if len(extra) > 0 {
		feedbackData = extra[0]
	}
	if len(extra) > 1 {
		prevData = extra[1]
	}

	if prevData != nil {
		var prev map[string]string
		if json.Unmarshal(prevData, &prev) == nil {
			if content := prev["content"]; content != "" {
				b.WriteString("\n── Previous submission ─────────────────────\n")
				display := content
				if len(display) > 500 {
					display = display[:500] + "…"
				}
				b.WriteString("  " + strings.ReplaceAll(display, "\n", "\n  "))
				b.WriteString("\n────────────────────────────────────────────\n")
			}
		}
	}

	if feedbackData != nil {
		var fb map[string]interface{}
		if json.Unmarshal(feedbackData, &fb) == nil {
			reviewer, _ := fb["reviewer"].(string)
			decision, _ := fb["decision"].(string)
			content, _ := fb["content"].(string)
			if content != "" {
				b.WriteString("\n── Reviewer feedback ───────────────────────\n")
				header := "▸ "
				if reviewer != "" {
					header += "@" + reviewer
				}
				if decision != "" {
					header += " — " + decision
				}
				b.WriteString(header + "\n\n")
				b.WriteString("  " + strings.ReplaceAll(content, "\n", "\n  "))
				b.WriteString("\n────────────────────────────────────────────\n")
			}
		}
	}

	b.WriteString("\nSubmit your result when ready.")
	return b.String()
}

// formatTallyResult renders the response from
// /tasks/{id}/tally as a short status summary. Distinguishes
// "resolved → winning option/verdict + cascade" from
// "still collecting → reason + current counts".
func formatTallyResult(data []byte, taskID string) string {
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return fmt.Sprintf("✗ Tally failed: %s", errMsg)
	}
	var b strings.Builder
	status, _ := resp["status"].(string)
	switch status {
	case "resolved":
		if winning, _ := resp["winning_option"].(string); winning != "" {
			b.WriteString(fmt.Sprintf("✓ Vote resolved: %s — winning option: %s\n", taskID, winning))
		} else if verdict, _ := resp["verdict"].(string); verdict != "" {
			icon := "✓"
			if verdict == "reject" {
				icon = "✗"
			}
			b.WriteString(fmt.Sprintf("%s Review resolved: %s — verdict: %s\n", icon, taskID, verdict))
		} else {
			b.WriteString(fmt.Sprintf("✓ Task resolved: %s\n", taskID))
		}
		if skipped, _ := resp["skipped"].([]interface{}); len(skipped) > 0 {
			b.WriteString(fmt.Sprintf("  → %d task(s) on losing branches flipped to SKIPPED\n", len(skipped)))
		}
		if nr, _ := resp["newly_ready"].(float64); nr > 0 {
			b.WriteString(fmt.Sprintf("  → %d downstream task(s) newly ready\n", int(nr)))
		}
	case "already_resolved":
		b.WriteString(fmt.Sprintf("• Task already resolved: %s\n", taskID))
	case "collecting":
		b.WriteString(fmt.Sprintf("⏳ Still collecting: %s\n", taskID))
		if tally, _ := resp["tally"].(map[string]interface{}); tally != nil {
			if reason, _ := tally["reason"].(string); reason != "" {
				b.WriteString("  Reason: " + reason + "\n")
			}
			if counts, _ := tally["counts"].(map[string]interface{}); len(counts) > 0 {
				b.WriteString("  Counts: ")
				parts := make([]string, 0, len(counts))
				for k, v := range counts {
					parts = append(parts, fmt.Sprintf("%s=%v", k, v))
				}
				sortStrings(parts)
				b.WriteString(strings.Join(parts, ", "))
				b.WriteString("\n")
			}
			if approves, hasApproves := tally["approves"].(float64); hasApproves {
				rejects, _ := tally["rejects"].(float64)
				total, _ := tally["total_reviews"].(float64)
				b.WriteString(fmt.Sprintf("  Reviews: %d approve / %d reject / %d total\n",
					int(approves), int(rejects), int(total)))
			}
		}
	default:
		b.WriteString(fmt.Sprintf("Tally result for %s: %s\n", taskID, status))
	}
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
	dematerialized, _ := result["dematerialized"].([]interface{})

	b.WriteString(fmt.Sprintf("✓ Invalidated: %s\n", taskID))
	if reason != "" {
		b.WriteString(fmt.Sprintf("  Reason: %s\n", reason))
	}

	// Compose a summary line that distinguishes the cascade
	// categories so the user can see at a glance why each
	// task was affected. Dynamic-for_each descendants are
	// listed separately because they were DELETED, not just
	// flipped to PENDING — the caller may want to know that
	// those tasks ceased to exist.
	b.WriteString(fmt.Sprintf("\n%d task(s) changed state (target", int(changed)))
	if len(descendants) > 0 {
		b.WriteString(fmt.Sprintf(" + %d task descendant(s)", len(descendants)))
	}
	if len(dematerialized) > 0 {
		b.WriteString(fmt.Sprintf(" + %d dynamic descendant(s) dematerialized", len(dematerialized)))
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

	// Dynamic descendants: they were materialized from the
	// invalidated source's output list and have been deleted
	// entirely. On re-accept the source will re-materialize
	// fresh instances matching whatever the new list
	// contains.
	if len(dematerialized) > 0 {
		b.WriteString("\nDematerialized (deleted — will be re-created on re-accept):\n")
		for _, d := range dematerialized {
			b.WriteString(fmt.Sprintf("  ✗ %v\n", d))
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
	decision, _ := result["decision"].(string)
	reviewCascade, _ := result["review_cascade"].(map[string]interface{})
	voteRes, _ := result["vote_resolution"].(map[string]interface{})
	topStatus, _ := result["status"].(string)

	// Detect a collecting-mode vote submission: the task is
	// still waiting for more citizens before the tally can
	// resolve.
	collectingVote := false
	if voteRes != nil {
		if c, _ := voteRes["collecting"].(bool); c {
			collectingVote = true
		}
	}
	if topStatus == "collecting" {
		collectingVote = true
	}

	// Review / vote tasks get an outcome-first summary so the
	// submitter sees what happened at a glance. Regular tasks
	// fall through to the generic "Result accepted" headline.
	switch {
	case decision == "approve":
		b.WriteString(fmt.Sprintf("✓ Review approved: %s\n", taskID))
	case decision == "reject":
		b.WriteString(fmt.Sprintf("✗ Review rejected: %s\n", taskID))
		if reviewCascade != nil {
			target, _ := reviewCascade["target"].(string)
			changed, _ := reviewCascade["changed"].(float64)
			if target != "" {
				b.WriteString(fmt.Sprintf("  → target %q invalidated and bounced back to READY (%d task(s) reset)\n", target, int(changed)))
			}
		}
	case collectingVote && voteRes != nil:
		votesSoFar, _ := voteRes["votes_so_far"].(float64)
		reason, _ := voteRes["reason"].(string)
		b.WriteString(fmt.Sprintf("⏳ Vote recorded: %s — still collecting (%d vote(s) so far)\n", taskID, int(votesSoFar)))
		if reason != "" {
			b.WriteString(fmt.Sprintf("  Waiting: %s\n", reason))
		}
		if counts, ok := voteRes["counts"].(map[string]interface{}); ok && len(counts) > 0 {
			b.WriteString("  Current tally: ")
			first := true
			for id, c := range counts {
				if !first {
					b.WriteString(", ")
				}
				b.WriteString(fmt.Sprintf("%s=%v", id, c))
				first = false
			}
			b.WriteString("\n")
		}
	case voteRes != nil:
		winning, _ := voteRes["winning_option"].(string)
		b.WriteString(fmt.Sprintf("✓ Vote resolved: %s — winning option: %s\n", taskID, winning))
		if votesTallied, _ := voteRes["votes_tallied"].(float64); votesTallied > 0 {
			b.WriteString(fmt.Sprintf("  (tallied %d vote(s))\n", int(votesTallied)))
		}
		if skippedCount, _ := voteRes["skipped_count"].(float64); skippedCount > 0 {
			b.WriteString(fmt.Sprintf("  → %d task(s) on losing branches flipped to SKIPPED\n", int(skippedCount)))
		}
	default:
		b.WriteString(fmt.Sprintf("✓ Result accepted: %s\n", taskID))
	}

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

	// Contribution counter — attribution, not scoring.
	contribNum := int(jsonFloat(result["contribution_number"]))
	projectsMonth := int(jsonFloat(result["projects_this_month"]))
	if contribNum > 0 {
		line := fmt.Sprintf("Contribution #%d", contribNum)
		if projectsMonth > 0 {
			line += fmt.Sprintf(" — %d project(s) this month", projectsMonth)
		}
		b.WriteString("\n" + line + "\n")
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

// formatArtifactHistory renders the git commit history of one artifact.
func formatArtifactHistory(data []byte) string {
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if errMsg, ok := resp["error"].(string); ok {
		return "✗ " + errMsg
	}
	path, _ := resp["path"].(string)
	history, _ := resp["history"].([]interface{})

	var b strings.Builder
	b.WriteString(fmt.Sprintf("History: %s  (%d commit(s))\n", path, len(history)))
	if len(history) == 0 {
		b.WriteString("\n(no commits recorded for this path)\n")
		return b.String()
	}
	b.WriteString("\n")
	for i, raw := range history {
		entry, _ := raw.(map[string]interface{})
		hash, _ := entry["hash"].(string)
		subject, _ := entry["subject"].(string)
		t, _ := entry["time"].(string)
		taskID, _ := entry["task_id"].(string)
		owner, _ := entry["owner"].(string)
		annotation, _ := entry["annotation"].(string)

		short := hash
		if len(short) > 8 {
			short = short[:8]
		}
		// A.5 polish: render the current-pointer / invalidated
		// annotation inline in the header line so users can see
		// which version is active at a glance.
		annotationSuffix := ""
		if annotation != "" {
			annotationSuffix = "  [" + annotation + "]"
		}
		b.WriteString(fmt.Sprintf("%d. %s  %s%s\n", i+1, short, t, annotationSuffix))
		if taskID != "" {
			b.WriteString(fmt.Sprintf("   task %s by @%s\n", taskID, owner))
		}
		b.WriteString(fmt.Sprintf("   %s\n", subject))
	}
	return b.String()
}

func formatRunStatus(runData []byte, tasksData []byte, viewer ...string) string {
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return string(runData)
	}
	// Pretty-print coordinator error responses instead of leaking
	// the raw {"error": "..."} shape. Matches the vocabulary the
	// other tool formatters use. If the server's error message is
	// a literal restatement of "run not found", don't echo it —
	// "✗ Run not found: run not found" reads as a duplication bug.
	if errMsg, ok := run["error"].(string); ok && errMsg != "" {
		lower := strings.ToLower(strings.TrimSpace(errMsg))
		if lower == "run not found" {
			return "✗ Run not found"
		}
		return fmt.Sprintf("✗ Run not found: %s", errMsg)
	}

	viewerName := ""
	if len(viewer) > 0 {
		viewerName = viewer[0]
	}
	_ = viewerName // used below in renderYourQueue

	var tasks []map[string]interface{}
	if err := json.Unmarshal(tasksData, &tasks); err != nil {
		return string(tasksData)
	}

	var b strings.Builder

	name, _ := run["name"].(string)
	state, _ := run["state"].(string)
	projectID := jsonID(run["project_id"])
	projectName, _ := run["_project_name"].(string)
	seq, _ := run["seq"].(float64)

	// Count by state
	counts := map[string]int{}
	for _, t := range tasks {
		s, _ := t["state"].(string)
		counts[s]++
	}
	total := len(tasks)
	// SKIPPED tasks are terminal (losing branch of a vote) — they
	// count as "done" for progress purposes so the bar agrees with
	// the run state. Otherwise a completed run with skipped tasks
	// shows 75% and confuses the user.
	done := counts["accepted"] + counts["skipped"] + counts["failed"]

	progressLine := fmt.Sprintf("Status: %s    Progress: %d/%d", state, done, total)
	if counts["skipped"] > 0 || counts["failed"] > 0 {
		parts := fmt.Sprintf("%d accepted", counts["accepted"])
		if counts["skipped"] > 0 {
			parts += fmt.Sprintf(", %d skipped", counts["skipped"])
		}
		if counts["failed"] > 0 {
			parts += fmt.Sprintf(", %d failed", counts["failed"])
		}
		progressLine += fmt.Sprintf(" (%s)", parts)
	}
	header := fmt.Sprintf("Project #%s", projectID)
	if projectName != "" {
		header += " (" + projectName + ")"
	}
	header += fmt.Sprintf(" → Run #%d: %s", int(seq), name)
	b.WriteString(header + "\n")
	if sourcePath, _ := run["source_path"].(string); sourcePath != "" {
		line := fmt.Sprintf("Source: %s", sourcePath)
		if sourceSHA, _ := run["source_commit_sha"].(string); sourceSHA != "" {
			line += fmt.Sprintf(" @%s", shortSHA(sourceSHA))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(progressLine + "\n")
	b.WriteString(fmt.Sprintf("%s\n\n", progressBar(done, total, 30)))

	// Failed tasks at top — needs immediate attention.
	var failedTasks []map[string]interface{}
	for _, t := range tasks {
		if s, _ := t["state"].(string); s == "failed" {
			failedTasks = append(failedTasks, t)
		}
	}
	if len(failedTasks) > 0 {
		b.WriteString("  ❌ Failed:\n")
		for _, t := range failedTasks {
			tid, _ := t["id"].(string)
			reason, _ := t["fail_reason"].(string)
			line := fmt.Sprintf("    %s", tid)
			if reason != "" {
				line += " — " + reason
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	// Template-level summary: group by task_def_id, show
	// counts per state. Readable regardless of DAG size.
	b.WriteString(renderTemplateSummary(tasks))

	// Your queue: tasks the current viewer can act on
	// (claimed by them or available to claim).
	b.WriteString(renderYourQueue(tasks, viewerName))

	return b.String()
}

// renderDAGTree builds a tree-shaped task list from dependency edges.
// Root tasks (no dependencies) are top-level; children are indented
// under their parent with box-drawing connectors.
//
// Output example:
//
//	discover ✓
//	├── BRCA1:analyze ✓
//	│   └── BRCA1:check ⏳
//	├── TP53:analyze →
//	│   └── TP53:check ○
//	└── synthesize ○
func renderDAGTree(tasks []map[string]interface{}) string {
	// Build index: full task ID → task map.
	byID := make(map[string]map[string]interface{}, len(tasks))
	for _, t := range tasks {
		id, _ := t["id"].(string)
		byID[id] = t
	}

	// Build parent map: task ID → list of parent IDs.
	parentMap := map[string][]string{}
	for _, t := range tasks {
		id, _ := t["id"].(string)
		depsStr, _ := t["depends_on"].(string)
		if depsStr == "" {
			continue
		}
		for _, d := range strings.Split(depsStr, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				parentMap[id] = append(parentMap[id], d)
			}
		}
	}

	// Compute depth of each task in the DAG (longest path from root).
	// Used to place multi-parent tasks under their deepest parent.
	depth := map[string]int{}
	var computeDepth func(id string) int
	computeDepth = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		maxParent := -1
		for _, p := range parentMap[id] {
			if pd := computeDepth(p); pd > maxParent {
				maxParent = pd
			}
		}
		depth[id] = maxParent + 1
		return depth[id]
	}
	for _, t := range tasks {
		id, _ := t["id"].(string)
		computeDepth(id)
	}

	// Build children map + roots. Tasks with zero deps are roots.
	// Tasks with multiple parents (fan-in) go under their deepest
	// parent so they nest naturally at the bottom of their branch.
	children := map[string][]string{}
	roots := []string{}
	for _, t := range tasks {
		id, _ := t["id"].(string)
		parents := parentMap[id]
		if len(parents) == 0 {
			roots = append(roots, id)
		} else if len(parents) == 1 {
			children[parents[0]] = append(children[parents[0]], id)
		} else {
			// Pick the deepest parent.
			best := parents[0]
			bestDepth := depth[best]
			for _, p := range parents[1:] {
				if depth[p] > bestDepth {
					best = p
					bestDepth = depth[p]
				}
			}
			children[best] = append(children[best], id)
		}
	}

	// Deduplicate children: guard against a task appearing
	// twice under the same parent.
	placed := map[string]bool{}
	for _, r := range roots {
		placed[r] = true
	}
	dedup := func(parentID string) []string {
		var result []string
		for _, c := range children[parentID] {
			if !placed[c] {
				placed[c] = true
				result = append(result, c)
			}
		}
		return result
	}

	var b strings.Builder

	// Recursive tree printer.
	var walk func(id, prefix string, isLast bool)
	walk = func(id, prefix string, isLast bool) {
		t := byID[id]
		if t == nil {
			return
		}
		state, _ := t["state"].(string)
		claimedBy, _ := t["claimed_by"].(string)
		icon := stateIcon(state)

		// Build the display name with task ID.
		displayName := taskShortName(t)
		tid, _ := t["id"].(string)

		// Connector.
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		line := fmt.Sprintf("%s%s%s %s [%s]", prefix, connector, displayName, icon, tid)
		if (state == "claimed" || state == "running") && claimedBy != "" {
			line += fmt.Sprintf(" (%s)", claimedBy)
		}
		b.WriteString(line + "\n")

		// Child prefix: if this node is the last sibling, don't
		// draw a vertical line; otherwise continue the line.
		childPrefix := prefix + "│   "
		if isLast {
			childPrefix = prefix + "    "
		}

		kids := dedup(id)
		for i, kid := range kids {
			walk(kid, childPrefix, i == len(kids)-1)
		}
	}

	// Sort roots by task sequence for stable output.
	sort.Slice(roots, func(i, j int) bool {
		si, _ := byID[roots[i]]["seq"].(float64)
		sj, _ := byID[roots[j]]["seq"].(float64)
		return si < sj
	})

	// Print roots (no connector prefix for roots).
	for _, r := range roots {
		t := byID[r]
		state, _ := t["state"].(string)
		claimedBy, _ := t["claimed_by"].(string)
		icon := stateIcon(state)
		displayName := taskShortName(t)
		tid, _ := t["id"].(string)

		line := fmt.Sprintf("%s %s [%s]", displayName, icon, tid)
		if (state == "claimed" || state == "running") && claimedBy != "" {
			line += fmt.Sprintf(" (%s)", claimedBy)
		}
		b.WriteString(line + "\n")

		kids := dedup(r)
		for i, kid := range kids {
			walk(kid, "", i == len(kids)-1)
		}
	}

	return b.String()
}

// taskShortName returns a compact display name for a task:
// "instance:taskdef" for for_each instances, just "taskdef" otherwise.
func taskShortName(t map[string]interface{}) string {
	taskDefID, _ := t["task_def_id"].(string)
	instanceKey, _ := t["instance_key"].(string)
	if instanceKey != "" {
		return instanceKey + ":" + taskDefID
	}
	return taskDefID
}

// renderTemplateSummary groups tasks by task_def_id and shows a
// one-line summary per template: "discover  4/4 ✅" or
// "review  1 in progress · 3 available".
func renderTemplateSummary(tasks []map[string]interface{}) string {
	type templateInfo struct {
		defID string
		total int
		byState map[string]int
		order int // preserve first-seen order
	}
	templates := map[string]*templateInfo{}
	var order []string
	for _, t := range tasks {
		defID, _ := t["task_def_id"].(string)
		if defID == "" {
			continue
		}
		info, ok := templates[defID]
		if !ok {
			info = &templateInfo{defID: defID, byState: map[string]int{}, order: len(order)}
			templates[defID] = info
			order = append(order, defID)
		}
		s, _ := t["state"].(string)
		info.total++
		info.byState[s]++
	}

	var b strings.Builder
	b.WriteString("By task:\n")
	for _, defID := range order {
		info := templates[defID]
		accepted := info.byState["accepted"]
		skipped := info.byState["skipped"]
		failed := info.byState["failed"]
		terminal := accepted + skipped + failed
		allTerminal := terminal == info.total

		// Build status description.
		var statusParts []string
		switch {
		case allTerminal && skipped == 0 && failed == 0:
			// All accepted — the clean "done" case.
			statusParts = append(statusParts, fmt.Sprintf("%d/%d ✅", accepted, info.total))
		case allTerminal:
			// Terminal but mixed: separate the three outcomes so
			// skipped/failed don't silently collapse into ✅.
			// Previously "4/4 ✅" was shown even when some were
			// skipped — hid vote-branch losses from the reader.
			if accepted > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d ✅", accepted))
			}
			if skipped > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d ⚫ skipped", skipped))
			}
			if failed > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d 🔴 failed", failed))
			}
		default:
			if n := info.byState["claimed"] + info.byState["running"]; n > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d in progress", n))
			}
			if n := info.byState["collecting"]; n > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d collecting", n))
			}
			if n := info.byState["ready"]; n > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d available", n))
			}
			if n := info.byState["pending"]; n > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d waiting", n))
			}
			if accepted > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d done", accepted))
			}
			if failed > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d failed", failed))
			}
			if skipped > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d skipped", skipped))
			}
		}
		b.WriteString(fmt.Sprintf("  %-14s %s\n", defID, strings.Join(statusParts, " · ")))
	}
	return b.String()
}

// renderYourQueue shows tasks the viewer can act on: claimed by
// them (finish first) and available to claim.
func renderYourQueue(tasks []map[string]interface{}, viewer string) string {
	var claimed, available []map[string]interface{}
	for _, t := range tasks {
		s, _ := t["state"].(string)
		claimedBy, _ := t["claimed_by"].(string)
		if (s == "claimed" || s == "running") && claimedBy == viewer {
			claimed = append(claimed, t)
		} else if s == "ready" {
			available = append(available, t)
		}
	}
	if len(claimed) == 0 && len(available) == 0 {
		return ""
	}

	var b strings.Builder
	total := len(claimed) + len(available)
	b.WriteString(fmt.Sprintf("\nYour queue (%d):\n", total))
	for _, t := range claimed {
		name := taskShortName(t)
		tid, _ := t["id"].(string)
		b.WriteString(fmt.Sprintf("  🔵 %s [%s] — in progress\n", name, tid))
	}
	for _, t := range available {
		name := taskShortName(t)
		tid, _ := t["id"].(string)
		b.WriteString(fmt.Sprintf("  🟡 %s [%s]\n", name, tid))
	}
	return b.String()
}

func formatTaskDetail(taskData []byte, inputsData []byte, viewer string) string {
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
	iterationLabel, _ := task["iteration_label"].(string)
	action, _ := task["action"].(string)

	seq, _ := task["seq"].(float64)
	icon := stateIcon(state)
	b.WriteString(fmt.Sprintf("Task #%d: %s %s\n", int(seq), id, icon))
	b.WriteString(fmt.Sprintf("  Run:  #%s\n", runID))
	b.WriteString(fmt.Sprintf("  Action:   %s\n", friendlyActionLabel(action)))
	b.WriteString(fmt.Sprintf("  State:    %s\n", stateLabel(state)))
	if iterationLabel != "" {
		b.WriteString(fmt.Sprintf("  Iteration: %s\n", iterationLabel))
	} else if instanceKey != "" {
		b.WriteString(fmt.Sprintf("  Instance: %s\n", instanceKey))
	}
	if ref != "" {
		b.WriteString(fmt.Sprintf("  Ref:      %s\n", ref))
	}
	// For single-citizen tasks, show the one-line "Claimed: X"
	// field. For multi-citizen tasks, suppress it — the Voting
	// block below renders the full voter list and status so a
	// single `claimed_by` display (just the most recent
	// claimer) would be misleading.
	multiCitizen := false
	if cf, _ := task["citizens"].(float64); cf > 1 {
		multiCitizen = true
	}
	if claimedBy != "" && !multiCitizen {
		b.WriteString(fmt.Sprintf("  Claimed:  %s\n", claimedBy))
	}
	if deps != "" {
		b.WriteString(fmt.Sprintf("  Depends:  %s\n", deps))
	}

	// Review-action metadata. Reviews target is set at run creation;
	// decision only after submit.
	if reviewsTarget, _ := task["reviews_target"].(string); reviewsTarget != "" {
		reviewLine := fmt.Sprintf("  Reviews:  %s", reviewsTarget)
		if by, _ := task["_review_target_claimed_by"].(string); by != "" {
			reviewLine += fmt.Sprintf(" (by @%s)", by)
		}
		b.WriteString(reviewLine + "\n")
		// Prefer absolute path; fall back to relative.
		if absPath, _ := task["_review_target_abs_path"].(string); absPath != "" {
			b.WriteString(fmt.Sprintf("  Path:     %s\n", absPath))
		} else if p, _ := task["_review_target_path"].(string); p != "" {
			b.WriteString(fmt.Sprintf("  Path:     %s/result.md\n", p))
		}
		if c, _ := task["_review_target_commit"].(string); c != "" {
			b.WriteString(fmt.Sprintf("  Commit:   %s\n", shortSHA(c)))
		}
		if preview, _ := task["_review_target_preview"].(string); preview != "" {
			b.WriteString(fmt.Sprintf("  Preview:  %s\n", preview))
		}
	}
	if decision, _ := task["review_decision"].(string); decision != "" {
		switch decision {
		case "approve":
			b.WriteString("  Decision: ✓ approved\n")
		case "request_changes":
			b.WriteString("  Decision: ↩ request changes (target back to ready)\n")
		case "reject":
			b.WriteString("  Decision: ✗ rejected (target failed)\n")
		case "comment":
			b.WriteString("  Decision: 💬 comment (non-blocking)\n")
		default:
			b.WriteString(fmt.Sprintf("  Decision: %s\n", decision))
		}
	}

	// Vote-action metadata — options list + winning choice if
	// the task has resolved. Declared once at run creation,
	// vote_choice set at submit time, cleared on invalidation.
	if voteOptsRaw, _ := task["vote_options"].(string); voteOptsRaw != "" {
		if opts := parseVoteOptionsForDisplay(voteOptsRaw); len(opts) > 0 {
			winningID, _ := task["vote_choice"].(string)
			b.WriteString("  Options:\n")
			for _, o := range opts {
				marker := "  "
				if o.ID == winningID {
					marker = "✓ "
				}
				line := fmt.Sprintf("    %s%s", marker, o.ID)
				if o.Label != "" && o.Label != o.ID {
					line += " — " + o.Label
				}
				if len(o.Activates) > 0 {
					line += fmt.Sprintf("  (activates: %s)", strings.Join(o.Activates, ", "))
				}
				b.WriteString(line + "\n")
			}
			if winningID != "" {
				b.WriteString(fmt.Sprintf("  Winning:  %s\n", winningID))
			}
		}
	}
	// Threshold / deadline: only show at top-level for
	// single-citizen tasks. For citizens > 1 these are rendered
	// inside the Voting / Review block (with relative-time
	// deadline), so don't duplicate them here.
	isMulti := false
	if c, _ := task["citizens"].(float64); c > 1 {
		isMulti = true
	}
	if !isMulti {
		if threshold, _ := task["vote_threshold"].(string); threshold != "" {
			b.WriteString(fmt.Sprintf("  Threshold: %s\n", threshold))
		}
		if deadline, _ := task["vote_deadline"].(string); deadline != "" {
			b.WriteString(fmt.Sprintf("  Deadline:  %s\n", deadline))
		}
	}

	// Multi-citizen voting / review block: citizens count,
	// quorum, live tally, active claimants, and per-voter
	// ballots. Renders for action:vote OR action:review when
	// citizens > 1.
	if citizensFloat, _ := task["citizens"].(float64); citizensFloat > 1 {
		citizens := int(citizensFloat)
		minQuorum := 0
		if q, _ := task["min_quorum"].(float64); q > 0 {
			minQuorum = int(q)
		}
		threshold, _ := task["vote_threshold"].(string)
		voteSubs, _ := task["vote_submissions"].([]interface{})
		activeClaims := stringSliceFromAny(task["active_claimants"])
		state, _ := task["state"].(string)
		taskAction, _ := task["action"].(string)
		visibility, _ := task["visibility"].(string)
		anonymize, _ := task["anonymize"].(bool)
		deadline, _ := task["vote_deadline"].(string)
		deadlineAt, _ := task["vote_deadline_at"].(string)
		b.WriteString(formatVotingBlock(taskAction, citizens, minQuorum, threshold, state, voteSubs, activeClaims, visibility, viewer, anonymize, deadline, deadlineAt))
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

	// Task history — show previous attempts when the task
	// has been invalidated and re-run. Only present when the
	// task response includes task_history (multiple claims).
	if historyRaw, ok := task["task_history"].([]interface{}); ok && len(historyRaw) > 0 {
		b.WriteString("\n── History ─────────────────────────────────\n")
		for i, raw := range historyRaw {
			h, _ := raw.(map[string]interface{})
			citizen, _ := h["citizen"].(string)
			claimedAt, _ := h["claimed_at"].(string)
			submittedAt, _ := h["submitted_at"].(string)
			outcome, _ := h["outcome"].(string)
			line := fmt.Sprintf("  %d. @%s claimed %s", i+1, citizen, claimedAt)
			if submittedAt != "" {
				line += fmt.Sprintf(", submitted %s", submittedAt)
			}
			if outcome != "" {
				line += fmt.Sprintf(" → %s", outcome)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("────────────────────────────────────────────\n")
	}

	return b.String()
}

// formatListTemplates renders the enju_list_templates response
// as a scannable menu. Each entry includes the template's path,
// name, one-line description snippet, and a compact param
// summary ("disease, tissue=whole blood") so the LLM can pick
// a recipe without drilling into each one.
func formatListTemplates(data []byte) string {
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if errMsg, ok := resp["error"].(string); ok {
		return "✗ " + errMsg
	}
	templates, _ := resp["templates"].([]interface{})
	if len(templates) == 0 {
		return "No templates found in this project.\n\nTemplates are reusable run recipes stored under enju_templates/*.yaml in the project git repo. To add one, commit a YAML file to enju_templates/ with a top-level params: block. Any existing run YAML can be promoted to a template by copying it into enju_templates/."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("── Templates (%d) — enju_list_templates ──\n", len(templates)))
	for _, t := range templates {
		m, _ := t.(map[string]interface{})
		path, _ := m["path"].(string)
		name, _ := m["name"].(string)
		desc, _ := m["description"].(string)
		params, _ := m["params"].([]interface{})
		parseErr, _ := m["parse_error"].(string)

		if parseErr != "" {
			// Surface parse failures inline. Users deserve to
			// see their template + the reason it's unusable,
			// not a menu that silently skipped it.
			b.WriteString(fmt.Sprintf("✗ %s  (unparseable — %s)\n", path, parseErr))
			continue
		}

		b.WriteString(fmt.Sprintf("▸ %s\n", path))
		if name != "" {
			b.WriteString(fmt.Sprintf("  Name:   %s\n", name))
		}
		if desc != "" {
			// Show the first line of the description as a preview;
			// full text comes from enju_describe_template.
			first := strings.SplitN(strings.TrimSpace(desc), "\n", 2)[0]
			b.WriteString(fmt.Sprintf("  About:  %s\n", first))
		}
		if len(params) > 0 {
			var parts []string
			for _, p := range params {
				pm, _ := p.(map[string]interface{})
				pname, _ := pm["name"].(string)
				required, _ := pm["required"].(bool)
				def := pm["default"]
				switch {
				case required:
					parts = append(parts, pname+"*")
				case def != nil:
					parts = append(parts, fmt.Sprintf("%s=%v", pname, def))
				default:
					parts = append(parts, pname)
				}
			}
			b.WriteString(fmt.Sprintf("  Params: %s\n", strings.Join(parts, ", ")))
		}
		b.WriteString("\n")
	}
	b.WriteString("────────────────────────────────────────────\n")
	b.WriteString("Starred params (*) are required. Call enju_describe_template <path> for full parameter docs, or enju_create_run path=... params={...} to instantiate.")
	return b.String()
}

// formatDescribeTemplate renders the full metadata for one
// template as a drill-down view. This is what the LLM reads
// right before gathering param values from the user: it has
// the full description prose, plus every param's type,
// default, and human-readable description.
func formatDescribeTemplate(data []byte) string {
	var tmpl map[string]interface{}
	if err := json.Unmarshal(data, &tmpl); err != nil {
		return string(data)
	}
	if errMsg, ok := tmpl["error"].(string); ok {
		return "✗ " + errMsg
	}
	path, _ := tmpl["path"].(string)
	name, _ := tmpl["name"].(string)
	desc, _ := tmpl["description"].(string)
	params, _ := tmpl["params"].([]interface{})

	var b strings.Builder
	b.WriteString(fmt.Sprintf("── Template: %s ──\n", path))
	if name != "" {
		b.WriteString(fmt.Sprintf("Name:        %s\n", name))
	}
	if desc != "" {
		b.WriteString("Description:\n")
		for _, line := range strings.Split(strings.TrimSpace(desc), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	if len(params) == 0 {
		b.WriteString("\nThis template declares no parameters — it can be instantiated with enju_create_run path=" + path + " and no params.")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("\nParameters (%d):\n", len(params)))
	for _, p := range params {
		m, _ := p.(map[string]interface{})
		pname, _ := m["name"].(string)
		ptype, _ := m["type"].(string)
		required, _ := m["required"].(bool)
		def := m["default"]
		pdesc, _ := m["description"].(string)

		marker := "  "
		if required {
			marker = "* "
		}
		b.WriteString(fmt.Sprintf("\n%s%s (%s", marker, pname, ptype))
		if required {
			b.WriteString(", required")
		} else if def != nil {
			b.WriteString(fmt.Sprintf(", default=%v", def))
		} else {
			b.WriteString(", optional")
		}
		b.WriteString(")\n")
		if pdesc != "" {
			for _, line := range strings.Split(strings.TrimSpace(pdesc), "\n") {
				b.WriteString("    " + line + "\n")
			}
		}
	}
	b.WriteString("\nStarred (*) parameters are required.\n")
	b.WriteString(fmt.Sprintf("To instantiate: enju_create_run project_id=<N> path=%s params={...}", path))
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
	sourcePath, _ := result["source_path"].(string)
	sourceSHA, _ := result["source_commit_sha"].(string)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("✓ Run created in project #%s as run #%d: %s\n", projectID, int(seq), name))
	b.WriteString(fmt.Sprintf("  Tasks: %d\n", int(taskCount)))
	if sourcePath != "" {
		line := fmt.Sprintf("  Source: %s", sourcePath)
		if sourceSHA != "" {
			line += fmt.Sprintf(" @%s", shortSHA(sourceSHA))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(fmt.Sprintf("\nUse enju_run_status(project_id=%s, run_id=%d) or enju_list_ready_tasks to see tasks.", projectID, int(seq)))
	return b.String()
}

// shortSHA returns the first 7 chars of a git commit SHA, the
// standard abbreviation. Leaves short inputs alone.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// --- Helpers ---

// formatProfile renders a citizen's profile with their
// contribution record. Phase G: no scoring formula, just
// factual counts from the contribution-events log.
// contribData may be nil (best-effort fetch).
func formatProfile(data []byte, contribData []byte) string {
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

	var b strings.Builder
	b.WriteString("── Profile ─────────────────────────────────\n")
	b.WriteString(fmt.Sprintf("Username:   @%s\n", username))
	if name != "" {
		b.WriteString(fmt.Sprintf("Display:    %s\n", name))
	}
	if email != "" {
		b.WriteString(fmt.Sprintf("Email:      %s\n", email))
	}
	if role != "" {
		b.WriteString(fmt.Sprintf("Role:       %s\n", role))
	}
	if model, _ := c["model"].(string); model != "" {
		b.WriteString(fmt.Sprintf("Model:      %s\n", model))
	}

	// Contribution record from the events log.
	if contribData != nil {
		var contrib map[string]interface{}
		if json.Unmarshal(contribData, &contrib) == nil {
			completed := int(jsonFloat(contrib["tasks_completed"]))
			rejected := int(jsonFloat(contrib["tasks_rejected"]))
			released := int(jsonFloat(contrib["tasks_released"]))
			reviews := int(jsonFloat(contrib["reviews_given"]))
			approves := int(jsonFloat(contrib["review_approves"]))
			rejects := int(jsonFloat(contrib["review_rejects"]))
			votes := int(jsonFloat(contrib["votes_cast"]))
			tokens := int64(jsonFloat(contrib["tokens_total"]))
			projects := int(jsonFloat(contrib["project_count"]))

			b.WriteString("\n── Contributions ───────────────────────────\n")
			b.WriteString(fmt.Sprintf("Tasks:     %d completed", completed))
			if rejected > 0 {
				b.WriteString(fmt.Sprintf(", %d rejected", rejected))
			}
			if released > 0 {
				b.WriteString(fmt.Sprintf(", %d released", released))
			}
			b.WriteString("\n")
			if reviews > 0 {
				b.WriteString(fmt.Sprintf("Reviews:   %d given (%d approve, %d reject)\n", reviews, approves, rejects))
			}
			if votes > 0 {
				b.WriteString(fmt.Sprintf("Votes:     %d cast\n", votes))
			}
			if tokens > 0 {
				b.WriteString(fmt.Sprintf("Tokens:    %d contributed\n", tokens))
			}
			if projects > 0 {
				b.WriteString(fmt.Sprintf("Projects:  %d active\n", projects))
			}

			// Downstream impact — show per-project breakdown
			// when multiple projects are involved.
			if impact, ok := contrib["downstream_impact"].(map[string]interface{}); ok {
				impactTasks := int(jsonFloat(impact["tasks"]))
				impactProjects := int(jsonFloat(impact["projects"]))
				if impactTasks > 0 {
					if impactProjects > 1 {
						b.WriteString(fmt.Sprintf("\nImpact:\n  Your outputs were used by %d downstream task(s)\n  across %d projects.\n", impactTasks, impactProjects))
					} else {
						b.WriteString(fmt.Sprintf("\nImpact:\n  Your outputs were used by %d downstream task(s).\n", impactTasks))
					}
				}
			}
		}
	}

	b.WriteString("────────────────────────────────────────────\n\n")
	b.WriteString(fmt.Sprintf("Others reference you as: %s  (this is what goes in `assign_to:`)\n", username))
	return b.String()
}

func jsonFloat(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
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

	// Profile — simple lines, no box drawing.
	b.WriteString(header + "\n")
	taskLine := fmt.Sprintf("Tasks: %.0f completed", completed)
	if timedOut > 0 {
		taskLine += fmt.Sprintf(", %.0f timed out", timedOut)
	}
	taskLine += fmt.Sprintf(", %d active", len(activeTasks))
	b.WriteString(taskLine + "\n")

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

// formatVotingBlock renders the multi-citizen voting / review
// status for citizens > 1 tasks: citizens count, quorum,
// effective rule, live tally, per-voter ballots, and any
// still-claimed-but-not-submitted citizens. Action-aware so
// review tasks get "Review" header + "approves/rejects" tally
// shape instead of raw option counts.
//
// visibility is the task's visibility policy ("open" or "blind").
// Blind collecting-state tasks hide sibling ballots from the
// current viewer; once the task resolves everyone sees
// everything regardless. viewerUsername identifies the caller
// so the blind filter can still show their own submission.
func formatVotingBlock(action string, citizens, minQuorum int, threshold, state string, voteSubs []interface{}, activeClaimants []string, visibility, viewerUsername string, anonymize bool, deadline, deadlineAt string) string {
	// Blind-collection filter: during COLLECTING, hide sibling
	// ballots from the current viewer. They can still see
	// their own submission (if any) so the claim response
	// can confirm "your vote landed." Once the task resolves
	// to accepted, everyone sees everything.
	totalSubmitted := len(voteSubs)
	hiddenSiblings := 0
	if visibility == "blind" && state == "collecting" {
		filtered := make([]interface{}, 0, len(voteSubs))
		for _, v := range voteSubs {
			m, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			user, _ := m["username"].(string)
			if user == viewerUsername && viewerUsername != "" {
				filtered = append(filtered, v)
			} else {
				hiddenSiblings++
			}
		}
		voteSubs = filtered
	}

	var b strings.Builder
	isReview := action == "review"
	if isReview {
		b.WriteString("── Review ──────────────────────────────────\n")
	} else {
		b.WriteString("── Voting ──────────────────────────────────\n")
	}

	effectiveThreshold := threshold
	if effectiveThreshold == "" {
		if isReview {
			effectiveThreshold = "any-reject-kills"
		} else {
			effectiveThreshold = "plurality"
		}
	}

	// Effective quorum. When the task author didn't set one
	// explicitly, the tally function defaults to `citizens`
	// for multi-citizen tasks (wait for everyone). Show the
	// effective value on every render so the voter sees the
	// bar they have to clear, not just the YAML-provided
	// override.
	effectiveQuorum := minQuorum
	if effectiveQuorum <= 0 {
		effectiveQuorum = citizens
	}

	// Header summarizes the rules.
	header := fmt.Sprintf("  Citizens: %d slots, quorum %d, threshold %s",
		citizens, effectiveQuorum, effectiveThreshold)
	if visibility != "" && visibility != "open" {
		header += ", visibility " + visibility
	}
	if anonymize {
		header += ", anonymous"
	}
	b.WriteString(header + "\n")
	if deadline != "" {
		line := "  Deadline: " + deadline
		if deadlineAt != "" {
			if t, err := time.Parse(time.RFC3339, deadlineAt); err == nil {
				rem := time.Until(t)
				if rem >= 0 {
					line += fmt.Sprintf(" (expires in %s)", rem.Round(time.Second))
				} else {
					line += fmt.Sprintf(" (expired %s ago)", (-rem).Round(time.Second))
				}
			}
		} else {
			line += " (not yet started — ticks from first claim)"
		}
		b.WriteString(line + "\n")
	}

	submitted := totalSubmitted
	// Tally counts per option and per-voter list.
	counts := map[string]int{}
	type ballot struct{ user, option string }
	ballots := make([]ballot, 0, submitted)
	for _, v := range voteSubs {
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		user, _ := m["username"].(string)
		option, _ := m["option"].(string)
		counts[option]++
		ballots = append(ballots, ballot{user: user, option: option})
	}

	// Status line varies by state. For review tasks, use the
	// word "reviews" instead of "votes."
	label := "votes"
	if isReview {
		label = "reviews"
	}
	switch state {
	case "accepted":
		b.WriteString(fmt.Sprintf("  Status:   ✓ resolved (%d/%d %s)\n", submitted, citizens, label))
	case "collecting":
		b.WriteString(fmt.Sprintf("  Status:   ⏳ collecting (%d/%d %s)\n", submitted, citizens, label))
	case "ready":
		claimed := len(activeClaimants) + submitted
		verb := "submitted"
		if isReview {
			verb = "reviewed"
		}
		b.WriteString(fmt.Sprintf("  Status:   → accepting claims (%d/%d claimed, %d/%d %s)\n", claimed, citizens, submitted, citizens, verb))
	default:
		b.WriteString(fmt.Sprintf("  Status:   %s (%d/%d %s)\n", state, submitted, citizens, label))
	}

	// Tally line — only show when there's at least one vote.
	if len(counts) > 0 {
		// Sort option ids for stable rendering.
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sortStrings(keys)
		tally := make([]string, 0, len(keys))
		for _, k := range keys {
			tally = append(tally, fmt.Sprintf("%s=%d", k, counts[k]))
		}
		b.WriteString("  Tally:    " + strings.Join(tally, ", ") + "\n")
	}

	// Per-voter ballots.
	if len(ballots) > 0 {
		parts := make([]string, 0, len(ballots))
		for _, b := range ballots {
			parts = append(parts, fmt.Sprintf("%s→%s", b.user, b.option))
		}
		if isReview {
			b.WriteString("  Reviewers: " + strings.Join(parts, ", ") + "\n")
		} else {
			b.WriteString("  Voters:   " + strings.Join(parts, ", ") + "\n")
		}
	}

	// Active claimants (claimed but haven't submitted yet).
	if len(activeClaimants) > 0 {
		suffix := "not yet voted"
		if isReview {
			suffix = "not yet reviewed"
		}
		b.WriteString("  Claimed:  " + strings.Join(activeClaimants, ", ") + " (" + suffix + ")\n")
	}

	if hiddenSiblings > 0 {
		noun := "ballot"
		if hiddenSiblings != 1 {
			noun = "ballots"
		}
		b.WriteString(fmt.Sprintf("  (%d sibling %s hidden — blind mode until task resolves)\n",
			hiddenSiblings, noun))
	}

	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

// formatVoteOptionsBlock renders a vote task's declared options as
// a `── Options ──` block for inclusion in the claim response,
// mirroring formatReviewingBlock. Highlights the winning option
// if one has already been recorded (vote_choice is populated),
// labels each option, and surfaces `activates:` targets so the
// voter can see the structural consequences of each choice.
// When vote_threshold / vote_deadline / min_quorum are set, they
// ride along as a trailing summary line.
func formatVoteOptionsBlock(opts []voteOptionView, task map[string]interface{}) string {
	if len(opts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("── Options ─────────────────────────────────\n")
	winningID, _ := task["vote_choice"].(string)
	for _, o := range opts {
		marker := "  "
		if o.ID == winningID {
			marker = "✓ "
		}
		// Only render the "— label" suffix when the label adds
		// information beyond the id. When label is empty or
		// identical to the id, showing "foo — foo" is noise.
		line := fmt.Sprintf("%s%s", marker, o.ID)
		if o.Label != "" && o.Label != o.ID {
			line += " — " + o.Label
		}
		if len(o.Activates) > 0 {
			line += fmt.Sprintf("  (activates: %s)", strings.Join(o.Activates, ", "))
		}
		b.WriteString(line + "\n")
	}
	// Trailing rules summary — only shown when any of the
	// fields is set. Keeps simple votes visually clean.
	var rules []string
	if threshold, _ := task["vote_threshold"].(string); threshold != "" {
		rules = append(rules, "threshold="+threshold)
	}
	if deadline, _ := task["vote_deadline"].(string); deadline != "" {
		rules = append(rules, "deadline="+deadline)
	}
	if minQuorum, _ := task["min_quorum"].(float64); minQuorum > 0 {
		rules = append(rules, fmt.Sprintf("min_quorum=%d", int(minQuorum)))
	}
	if len(rules) > 0 {
		b.WriteString("  (" + strings.Join(rules, ", ") + ")\n")
	}
	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

// voteOptionView is a display-only projection of a declared vote
// option. Lives in format.go rather than yaml/api because the
// formatter needs a minimal, loosely-typed view and shouldn't
// pull in the yaml package.
type voteOptionView struct {
	ID        string
	Label     string
	Activates []string
}

// parseVoteOptionsForDisplay decodes the JSON-encoded vote_options
// column into the display projection. Returns nil on any parse
// failure rather than propagating an error — the formatter
// should degrade gracefully if the column is malformed (a
// storage-side consistency bug is surfaced elsewhere).
func parseVoteOptionsForDisplay(optionsJSON string) []voteOptionView {
	if optionsJSON == "" {
		return nil
	}
	var raw []struct {
		ID        string   `json:"id"`
		Label     string   `json:"label,omitempty"`
		Activates []string `json:"activates,omitempty"`
	}
	if err := json.Unmarshal([]byte(optionsJSON), &raw); err != nil {
		return nil
	}
	out := make([]voteOptionView, len(raw))
	for i, r := range raw {
		out[i] = voteOptionView{ID: r.ID, Label: r.Label, Activates: r.Activates}
	}
	return out
}

func stateIcon(state string) string {
	switch state {
	// Emoji icons — colorful, double-width but scannable at a glance.
	// To switch to monochrome single-width, swap the return values:
	//   accepted  → "✓"    ready     → "▶"
	//   claimed   → "◐"    pending   → "○"
	//   failed    → "✗"    skipped   → "⊘"
	case "accepted", "completed":
		return "✅"
	case "ready":
		return "🟡"
	case "claimed", "running":
		return "🔵"
	case "collecting":
		return "🔵"
	case "pending":
		return "⚪"
	case "skipped":
		return "⚫"
	case "failed":
		return "🔴"
	case "invalid", "invalidated", "rejected":
		return "🔴"
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
		return "available"
	case "claimed", "running":
		return "in progress"
	case "pending":
		return "waiting"
	case "invalid", "invalidated":
		return "invalidated"
	case "collecting":
		return "collecting"
	case "skipped":
		return "skipped"
	case "failed":
		return "failed"
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
