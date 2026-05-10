// Package format renders the human-readable text that MCP tools
// return to the agent. Inputs are typically JSON bytes from a
// store/HTTP layer; outputs are formatted strings ready to send
// over the MCP wire.
//
// Lives in core/ because both the fat-client and (post-Phase-3.4)
// the coordinator's MCP handlers produce identical textual
// responses for the same conceptual operation. Sharing the
// formatter here keeps the two surfaces in lockstep — a wording
// change shows up in both places automatically.
//
// Pure logic only: no I/O, no DB, no internal-package
// dependencies on either side. See docs/architecture-boundaries.md.
//
// Surface caveat (Phase 3.3 — pre 3.4): every formatter and
// helper is exported here in anticipation of cross-side use,
// even though the coord-side handlers don't exist yet. Once
// 3.4 settles and the actually-used coord-side subset is
// visible, do a re-internalization pass — anything still
// fat-client-only goes back to lowercase. Tracked in TODO.md
// as "format/ surface re-audit post-3.4".
package format

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

func ProjectMemberList(data []byte, projectID int64) string {
	var members []map[string]interface{}
	if err := json.Unmarshal(data, &members); err != nil {
		var errEnv map[string]interface{}
		if json.Unmarshal(data, &errEnv) == nil {
			if msg, ok := errEnv["error"].(string); ok && msg != "" {
				return msg
			}
		}
		return string(data)
	}
	if len(members) == 0 {
		return fmt.Sprintf("Project #%d has no recorded members (legacy open project).", projectID)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Members of project #%d\n\n", projectID))
	for _, m := range members {
		username, _ := m["username"].(string)
		name, _ := m["name"].(string)
		role, _ := m["role"].(string)
		addedBy, _ := m["added_by"].(string)
		addedAt, _ := m["added_at"].(string)
		line := fmt.Sprintf("  @%-20s  %-7s", username, role)
		if name != "" && name != username {
			line += fmt.Sprintf("  (%s)", name)
		}
		if addedBy != "" {
			line += fmt.Sprintf("  — added by @%s", addedBy)
		} else {
			line += "  — project creator"
		}
		if addedAt != "" {
			line += fmt.Sprintf("  on %s", addedAt)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func ProjectList(data []byte) string {
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
		id := JsonID(p["id"])

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

// ProjectRemoteStatus renders the live remote-status diagnostic
// returned by GET /projects/{id}/remote/status. Renders different
// guidance for ahead vs diverged so the user knows whether a plain
// sync is safe or whether force-push would be destructive.
func ProjectRemoteStatus(data []byte) string {
	var r map[string]interface{}
	if err := json.Unmarshal(data, &r); err != nil {
		return string(data)
	}
	if errMsg, ok := r["error"].(string); ok {
		return "✗ " + errMsg
	}
	projectID := JsonID(r["project_id"])
	remote, _ := r["remote_url"].(string)
	status, _ := r["status"].(string)
	ahead := IntFromJSON(r["ahead_by"])
	behind := IntFromJSON(r["behind_by"])

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Project #%s remote status\n", projectID))
	// For path-mode projects (adopted folder + managed bare under
	// <project>/enju/.bare.git/), RemoteStatusReport surfaces the
	// project home as `workspace` and the bare path as `remote_url`.
	// Show them on separate lines so the user understands the
	// "your tree → managed bare" routing.
	if workspace, ok := r["workspace"].(string); ok && workspace != "" {
		b.WriteString("  workspace: " + workspace + " (adopted folder)\n")
		b.WriteString("  git remote: " + remote + "\n")
	} else {
		b.WriteString("  remote:    " + remote + "\n")
	}
	b.WriteString("  status:    " + HumanRemoteStatus(status, ahead, behind) + "\n")
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

// ProjectSyncResult renders the outcome of a sync attempt,
// including the non-error "refused" and "noop" paths where the
// server declined to push for safety reasons.
func ProjectSyncResult(data []byte) string {
	var r map[string]interface{}
	if err := json.Unmarshal(data, &r); err != nil {
		return string(data)
	}
	projectID := JsonID(r["project_id"])
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

// HumanRemoteStatus translates the structured RemoteComparison
// status code into a human-readable label with ahead/behind counts
// where applicable.
func HumanRemoteStatus(code string, ahead, behind int) string {
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
	default:
		return code
	}
}

// IntFromJSON coerces a numeric JSON field (always decoded as
// float64 by encoding/json) into an int. Returns 0 if the field is
// missing or not a number.
func IntFromJSON(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func CreateProjectResult(data []byte) string {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data)
	}
	if errMsg, ok := result["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to create project: %s", errMsg)
	}
	name, _ := result["name"].(string)
	id := JsonID(result["id"])
	return fmt.Sprintf("✓ Project #%s created: %s", id, name)
}

func RunList(data []byte) string {
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
		projectID := JsonID(p["project_id"])
		seq, _ := p["seq"].(float64)

		icon := StateIcon(state)
		b.WriteString(fmt.Sprintf("  %s project #%s → run #%d  %-30s [%s]  %d tasks\n",
			icon, projectID, int(seq), name, state, int(taskCount)))
	}

	return b.String()
}

func ReadyTasks(data []byte) string {
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
		runNum := JsonID(t["run_id"])
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
			b.WriteString(fmt.Sprintf("    \"%s\"\n", Truncate(prompt, 120)))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// Requirements renders the task environment requirements for display.
// Returns empty string if no requirements declared.
func Requirements(reqRaw string) string {
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
			WriteRequirementCategory(&b, cat, v)
			seen[cat] = true
		}
	}
	// Any other keys not in the standard list
	for k, v := range reqs {
		if !seen[k] {
			WriteRequirementCategory(&b, k, v)
		}
	}

	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

func WriteRequirementCategory(b *strings.Builder, name string, value interface{}) {
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

// OutputsSchema renders the named outputs schema for display.
// Returns empty string if no outputs declared.
// AssignmentSchema renders the assign_to and require_role
// restrictions on a task. Both are optional — when neither is set the
// task is open to any citizen and this function returns "" so the
// formatter doesn't show any assignment box at all.
func AssignmentSchema(assignTo []string, requireRole string) string {
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

// ArtifactsSchema renders the reads_artifacts and writes_artifacts
// declarations on a task. Either or both can be empty. Returns "" if
// nothing to show.
func ArtifactsSchema(reads, writes []string) string {
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

// ResolvedArtifactsBlock renders the Resolved Artifacts box for
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
func ResolvedArtifactsBlock(artifacts map[string]interface{}, missing []string) string {
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
		SortStrings(sortedMissing)
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
	SortStrings(paths)

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

// ReviewingBlock renders the target's content for an
// action:review claim so the reviewer can see what they're
// evaluating without a second enju_get_task call. Mirrors the
// resolved-artifacts block in shape: header line + indented
// content, truncated with a pointer to a richer tool for large
// payloads.
func ReviewingBlock(reviewing map[string]interface{}) string {
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
		b.WriteString(fmt.Sprintf("  Commit: %s\n", ShortSHA(commitSHA)))
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

// SortStrings sorts a slice of strings in place. Tiny helper to avoid
// pulling sort into format.go for a single call site.
func SortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// StringSliceFromAny extracts a []string from a JSON-decoded value
// (which is []interface{} when it came through json.Unmarshal).
func StringSliceFromAny(v interface{}) []string {
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

// WriteArtifactPathsFromAny extracts just the path strings from a
// writes_artifacts payload — paths live at different shapes
// depending on the source:
//
//   - Legacy / pre-Phase-A rows wrote bare strings: ["a","b"].
//   - Current wire format is the object form:
//     [{"path":"a","track":true}, {"path":"b","track":false}].
//
// Bug-fix helper: an earlier version of the display path used
// StringSliceFromAny, which silently dropped every entry once
// the wire shape became polymorphic — the entire "Writes" block
// disappeared from claim + get_task output. This helper handles
// both forms and mixed lists.
func WriteArtifactPathsFromAny(v interface{}) []string {
	if v == nil {
		return nil
	}
	xs, ok := v.([]interface{})
	if !ok {
		// Fall back to the plain extractor for legacy typed
		// inputs (e.g. []string directly from a test fixture).
		return StringSliceFromAny(v)
	}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		switch e := x.(type) {
		case string:
			out = append(out, e)
		case map[string]interface{}:
			if p, _ := e["path"].(string); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// TruncateRunes returns a rune-aware truncation of s to at most max
// runes. Falls back to s unchanged if the string already fits. Used to
// keep fixed-width dashboard boxes from overflowing when the user has
// a long display name.
func TruncateRunes(s string, max int) string {
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

// HumanizeRelTime formats a timestamp as a short relative duration
// suitable for list views. Falls back to the full string on parse errors.
//
// Examples: "5s ago", "2m ago", "3h ago", "yesterday", "4d ago",
// "2026-04-13" for anything older than 30 days.
func HumanizeRelTime(ts string) string {
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

func OutputsSchema(outputsRaw string) string {
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
		pairs = append(pairs, fmt.Sprintf("%q: %s", f.name, ExampleValueForFormat(f.format)))
	}
	b.WriteString("\nExample:\n")
	b.WriteString("  outputs_json='{" + strings.Join(pairs, ", ") + "}'\n")
	b.WriteString("────────────────────────────────────────────\n")
	return b.String()
}

// ExampleValueForFormat returns a JSON-literal example value
// matching the declared format. Used in the outputs_json
// example so the LLM sees a shape-correct template instead
// of a generic "content here" placeholder.
func ExampleValueForFormat(format string) string {
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

// ClaimResult renders the response from claiming a task.
// Optional trailing args: reviewFeedback (JSON from fetchReviewFeedback)
// and previousSubmission (JSON with "content" key).
func ClaimResult(claimData []byte, inputsData []byte, viewer string, extra ...[]byte) string {
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
		b.WriteString(Requirements(reqRaw))
	}

	// Show outputs schema if present
	if outputsRaw, ok := task["outputs"].(string); ok && outputsRaw != "" {
		b.WriteString(OutputsSchema(outputsRaw))
	}

	// Show access restrictions (assign_to, require_role) if any.
	assignTo := StringSliceFromAny(task["assign_to"])
	requireRole, _ := task["require_role"].(string)
	if s := AssignmentSchema(assignTo, requireRole); s != "" {
		b.WriteString(s)
	}

	// Show artifact reads/writes schema if present.
	// writes_artifacts is polymorphic on the wire post-Phase-A
	// (object form {path,track}) — extractor handles both.
	reads := StringSliceFromAny(task["reads_artifacts"])
	writes := WriteArtifactPathsFromAny(task["writes_artifacts"])
	if s := ArtifactsSchema(reads, writes); s != "" {
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
			missingArtifacts = StringSliceFromAny(inputs["missing_artifacts"])

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
				b.WriteString(ResolvedArtifactsBlock(resolvedArtifacts, missingArtifacts))
			}

			// Review tasks: surface the reviewed target's content
			// inline. fetchAndResolveLocally attaches a "reviewing"
			// block to the inputs response for action:review tasks,
			// populated from the local clone at the target's
			// accepted commit_sha. Mirrors the reads_artifacts
			// block — the claimer shouldn't need a second
			// round-trip to see what they're evaluating.
			if reviewing, ok := inputs["reviewing"].(map[string]interface{}); ok {
				b.WriteString(ReviewingBlock(reviewing))
			}
		}
	}

	// Vote tasks: render the declared options list inline so
	// the voter sees what they're choosing between without a
	// separate task-detail fetch. Independent of the inputs
	// branch because options live on the task record itself,
	// not in the resolved prompt.
	if voteOptsRaw, _ := task["vote_options"].(string); voteOptsRaw != "" {
		if opts := ParseVoteOptionsForDisplay(voteOptsRaw); len(opts) > 0 {
			b.WriteString(VoteOptionsBlock(opts, task))
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
		activeClaimants := StringSliceFromAny(task["active_claimants"])
		state, _ := task["state"].(string)
		taskAction, _ := task["action"].(string)
		visibility, _ := task["visibility"].(string)
		anonymize, _ := task["anonymize"].(bool)
		deadline, _ := task["vote_deadline"].(string)
		deadlineAt, _ := task["vote_deadline_at"].(string)
		b.WriteString(VotingBlock(taskAction, citizens, minQuorum, threshold, state, voteSubs, activeClaimants, visibility, viewer, anonymize, deadline, deadlineAt))
	}

	if inputsData == nil {
		// Always render the raw (unresolved) prompt when no
		// inputs block is available. Pre-lean-claim this was
		// gated on `deps == ""` because resolved-prompt
		// rendering handled the with-deps case via inputsData;
		// the lean claim path deliberately skips inputsData
		// regardless of deps, and a scripted citizen still
		// needs to see the prompt template to know what the
		// task asks for.
		b.WriteString("── Prompt ──────────────────────────────────\n")
		b.WriteString(prompt)
		b.WriteString("\n────────────────────────────────────────────\n")
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

// TallyResult renders the response from
// /tasks/{id}/tally as a short status summary. Distinguishes
// "resolved → winning option/verdict + cascade" from
// "still collecting → reason + current counts".
func TallyResult(data []byte, taskID string) string {
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
				SortStrings(parts)
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

// InvalidateResult renders the response from /tasks/{id}/invalidate.
// Shows the target, cascaded descendants, rolled-back artifacts, and
// the reason if provided.
func InvalidateResult(data []byte, taskID string) string {
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
	parked, _ := result["parked"].([]interface{})

	b.WriteString(fmt.Sprintf("✓ Invalidated: %s\n", taskID))
	if reason != "" {
		b.WriteString(fmt.Sprintf("  Reason: %s\n", reason))
	}

	// Compose a summary line that distinguishes the cascade
	// categories so the user can see at a glance why each
	// task was affected. Dynamic-for_each descendants are
	// listed separately because they were PARKED (data kept,
	// hidden from the scheduler), not deleted — matched keys
	// restore on re-accept, stale keys get deleted at that
	// point.
	b.WriteString(fmt.Sprintf("\n%d task(s) changed state (target", int(changed)))
	if len(descendants) > 0 {
		b.WriteString(fmt.Sprintf(" + %d task descendant(s)", len(descendants)))
	}
	if len(parked) > 0 {
		b.WriteString(fmt.Sprintf(" + %d dynamic descendant(s) parked", len(parked)))
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
	// invalidated source's output list and are now PARKED.
	// On re-accept with a matching output list they restore
	// in-place (no re-work, commits + ballots preserved); on
	// a non-matching re-accept, the stale keys get deleted at
	// that point.
	if len(parked) > 0 {
		b.WriteString("\nParked (will restore on matching re-accept, or delete on non-match):\n")
		for _, d := range parked {
			b.WriteString(fmt.Sprintf("  ⏸ %v\n", d))
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

func SubmitResult(data []byte, taskID string) string {
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
			if target != "" {
				// reject = terminal failure on the target with
				// artifact rollback + descendant skip cascade.
				// Phrasing must not reuse the old
				// request_changes "bounced back to READY" line
				// — see docs/rollback.md § Review verdicts.
				descendants, _ := reviewCascade["descendants"].([]interface{})
				rollbacks, _ := reviewCascade["rollbacks_count"].(float64)
				line := fmt.Sprintf("  → target %q rejected (terminal)", target)
				var parts []string
				if int(rollbacks) > 0 {
					parts = append(parts, fmt.Sprintf("%d artifact(s) rolled back", int(rollbacks)))
				}
				if n := len(descendants); n > 0 {
					parts = append(parts, fmt.Sprintf("%d descendant(s) skipped", n))
				}
				if len(parts) > 0 {
					line += " — " + strings.Join(parts, ", ")
				}
				b.WriteString(line + "\n")
			}
		}
	case decision == "request_changes":
		b.WriteString(fmt.Sprintf("↺ Changes requested: %s\n", taskID))
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
	contribNum := int(JsonFloat(result["contribution_number"]))
	projectsMonth := int(JsonFloat(result["projects_this_month"]))
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

// ArtifactList renders the list of artifacts in a project.
func ArtifactList(data []byte, projectID int64) string {
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
			HumanizeRelTime(updatedAt),
		))
	}
	return b.String()
}

// ArtifactDetail renders the content + provenance of one artifact.
func ArtifactDetail(data []byte) string {
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

// ArtifactHistory renders the git commit history of one artifact.
func ArtifactHistory(data []byte) string {
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

func RunStatus(runData []byte, tasksData []byte, viewer ...string) string {
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
	_ = viewerName // used below in RenderYourQueue

	var tasks []map[string]interface{}
	if err := json.Unmarshal(tasksData, &tasks); err != nil {
		return string(tasksData)
	}

	var b strings.Builder

	name, _ := run["name"].(string)
	state, _ := run["state"].(string)
	projectID := JsonID(run["project_id"])
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
	// Parked tasks are NOT done — they're waiting for a
	// reconciliation-driven restore or delete. Keep them out
	// of the progress numerator so the bar accurately reflects
	// "what's settled" and surface them separately below.

	progressLine := fmt.Sprintf("Status: %s    Progress: %d/%d", state, done, total)
	if counts["skipped"] > 0 || counts["failed"] > 0 || counts["parked"] > 0 {
		parts := fmt.Sprintf("%d accepted", counts["accepted"])
		if counts["skipped"] > 0 {
			parts += fmt.Sprintf(", %d skipped", counts["skipped"])
		}
		if counts["failed"] > 0 {
			parts += fmt.Sprintf(", %d failed", counts["failed"])
		}
		if counts["parked"] > 0 {
			// ⏸ mirrors the glyph in the per-task rows so the
			// reader can connect the summary line to the
			// paused entries below.
			parts += fmt.Sprintf(", %d ⏸ parked", counts["parked"])
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
			line += fmt.Sprintf(" @%s", ShortSHA(sourceSHA))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(progressLine + "\n")
	b.WriteString(fmt.Sprintf("%s\n\n", ProgressBar(done, total, 30)))

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

	// Pending merge resolutions — system-spawned merge_resolve
	// tasks (parallel-merge phase 3) flag a non-FF auto-merge
	// that hit a content conflict and needs a human (or future
	// merge-resolver bot) to finish the merge by hand. Render
	// in their own block so non-assignees scanning run_status
	// see the attention signal regardless of who owns the task.
	var mergeResolveTasks []map[string]interface{}
	for _, t := range tasks {
		if a, _ := t["action"].(string); a == "merge_resolve" {
			if s, _ := t["state"].(string); s != "accepted" && s != "skipped" && s != "failed" {
				mergeResolveTasks = append(mergeResolveTasks, t)
			}
		}
	}
	if len(mergeResolveTasks) > 0 {
		b.WriteString(fmt.Sprintf("  ⚠ Merge resolutions awaiting human (%d):\n", len(mergeResolveTasks)))
		for _, t := range mergeResolveTasks {
			tid, _ := t["id"].(string)
			tstate, _ := t["state"].(string)
			claimedBy, _ := t["claimed_by"].(string)
			line := fmt.Sprintf("    %s [%s]", tid, tstate)
			if claimedBy != "" {
				line += " — claimed by " + claimedBy
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	// Template-level summary: group by task_def_id, show
	// counts per state. Readable regardless of DAG size.
	b.WriteString(RenderTemplateSummary(tasks))

	// Your queue: tasks the current viewer can act on
	// (claimed by them or available to claim).
	b.WriteString(RenderYourQueue(tasks, viewerName))

	return b.String()
}

// RenderDAGTree builds a tree-shaped task list from dependency edges.
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
func RenderDAGTree(tasks []map[string]interface{}) string {
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
		skipReason, _ := t["skip_reason"].(string)
		icon := StateIconFor(state, skipReason)

		// Build the display name with task ID.
		displayName := TaskShortName(t)
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
		// Skipped-because-upstream-failed surfaces the upstream
		// id inline so the reader can immediately see which
		// failure blocked this task — distinct from vote-cascade
		// skips (no reason), which are expected/intentional.
		if state == "skipped" && skipReason != "" {
			line += fmt.Sprintf(" (%s)", skipReason)
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
		icon := StateIcon(state)
		displayName := TaskShortName(t)
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

// TaskShortName returns a compact display name for a task:
// "instance:taskdef" for for_each instances, just "taskdef" otherwise.
func TaskShortName(t map[string]interface{}) string {
	taskDefID, _ := t["task_def_id"].(string)
	instanceKey, _ := t["instance_key"].(string)
	if instanceKey != "" {
		return instanceKey + ":" + taskDefID
	}
	return taskDefID
}

// RenderTemplateSummary groups tasks by task_def_id and shows a
// one-line summary per template: "discover  4/4 ✅" or
// "review  1 in progress · 3 available".
//
// Skipped tasks are rendered as ⚫ by default. When the skipped
// task carries a non-empty skip_reason (only set on fail-cascade
// skips today), it's rendered as ⊘ with the reason suffix so the
// reader can tell "skipped because the gate picked the other
// branch" apart from "skipped because an upstream task failed."
func RenderTemplateSummary(tasks []map[string]interface{}) string {
	type templateInfo struct {
		defID   string
		total   int
		byState map[string]int
		// skipReasons collects the unique skip_reason strings
		// seen on this template's skipped tasks. Populated only
		// when at least one skipped task has an upstream-failure
		// reason; empty for vote-cascade skips.
		skipReasons map[string]bool
		order       int // preserve first-seen order
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
			info = &templateInfo{defID: defID, byState: map[string]int{}, skipReasons: map[string]bool{}, order: len(order)}
			templates[defID] = info
			order = append(order, defID)
		}
		s, _ := t["state"].(string)
		info.total++
		info.byState[s]++
		if s == "skipped" {
			if reason, _ := t["skip_reason"].(string); reason != "" {
				info.skipReasons[reason] = true
			}
		}
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
		parked := info.byState["parked"]

		var statusParts []string
		switch {
		case allTerminal && skipped == 0 && failed == 0 && parked == 0:
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
				// ⊘ signals "blocked by a failure"; ⚫ is the
				// vote-cascade skip (intentional gating).
				glyph := "⚫"
				suffix := ""
				if len(info.skipReasons) > 0 {
					glyph = "⊘"
					suffix = " (" + JoinSkipReasons(info.skipReasons) + ")"
				}
				statusParts = append(statusParts, fmt.Sprintf("%d %s skipped%s", skipped, glyph, suffix))
			}
			if failed > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d 🔴 failed", failed))
			}
		default:
			if n := info.byState["claimed"] + info.byState["running"]; n > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d in progress", n))
			}
			// Phase 8.3 — SUBMITTED is "submitted, awaiting
			// merge confirmation." Renders as a distinct
			// in-flight bucket so the operator can tell
			// "claimed but stuck" (in progress) from "actually
			// done with their work, just integrating" (the
			// brief's silent-cascade-stall case made this
			// distinction load-bearing).
			if n := info.byState["submitted"]; n > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d ⏳ submitted", n))
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
			if parked > 0 {
				// Parked instances stay visible in the default
				// (in-progress) rollup so the reader can see
				// "this template has paused work" rather than
				// just missing rows.
				statusParts = append(statusParts, fmt.Sprintf("%d ⏸ parked", parked))
			}
		}
		b.WriteString(fmt.Sprintf("  %-14s %s\n", defID, strings.Join(statusParts, " · ")))
	}
	return b.String()
}

// JoinSkipReasons renders the unique skip_reason set (from the
// template's skipped tasks) as a compact suffix for the summary
// line. One reason → verbatim; multiple distinct reasons →
// deduped and comma-joined in sorted order so output is stable
// across runs.
func JoinSkipReasons(set map[string]bool) string {
	if len(set) == 0 {
		return ""
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// MaxQueueEntriesPerTemplate caps how many "🟡 ready" rows we
// spell out per task_def_id before collapsing the rest to a
// single "...plus N more" line. Picked so a for_each producing
// tens of instances stops drowning the status output while the
// first few are still individually callable-out for a glance.
// Claimed-by-viewer rows are always shown in full — those are
// the tasks the user is actively holding.
const MaxQueueEntriesPerTemplate = 6

// RenderYourQueue shows tasks the viewer can act on: claimed by
// them (finish first) and available to claim.
//
// Available tasks are grouped by task_def_id and clipped past
// MaxQueueEntriesPerTemplate per group. For a run where one
// template has fanned out into dozens of instances, the output
// stays scannable instead of listing 30+ near-identical rows.
// Full list is always reachable via enju_list_ready_tasks.
func RenderYourQueue(tasks []map[string]interface{}, viewer string) string {
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

	// Claimed-by-viewer first — the reader's own work in flight.
	for _, t := range claimed {
		name := TaskShortName(t)
		tid, _ := t["id"].(string)
		b.WriteString(fmt.Sprintf("  🔵 %s [%s] — in progress\n", name, tid))
	}

	// Bucket available tasks by task_def_id, preserving the
	// order of first appearance so the output is stable and
	// matches the "By task:" block above.
	type group struct {
		defID string
		tasks []map[string]interface{}
	}
	groupIndex := map[string]int{}
	var groups []*group
	for _, t := range available {
		defID, _ := t["task_def_id"].(string)
		if defID == "" {
			// Shouldn't happen in practice, but guard so an
			// ill-formed task row still renders somewhere.
			defID = "(no template)"
		}
		idx, ok := groupIndex[defID]
		if !ok {
			idx = len(groups)
			groupIndex[defID] = idx
			groups = append(groups, &group{defID: defID})
		}
		groups[idx].tasks = append(groups[idx].tasks, t)
	}

	clipped := false
	for _, g := range groups {
		n := len(g.tasks)
		head := n
		if head > MaxQueueEntriesPerTemplate {
			head = MaxQueueEntriesPerTemplate
		}
		for i := 0; i < head; i++ {
			t := g.tasks[i]
			name := TaskShortName(t)
			tid, _ := t["id"].(string)
			b.WriteString(fmt.Sprintf("  🟡 %s [%s]\n", name, tid))
		}
		if n > head {
			clipped = true
			b.WriteString(fmt.Sprintf("     ...plus %d more of same template (%s)\n", n-head, g.defID))
		}
	}
	if clipped {
		b.WriteString("  → Use enju_list_ready_tasks for the full list.\n")
	}
	return b.String()
}

// Edge is a single directed connection in the Mermaid graph —
// from source (either an in-run full task id, or a
// pre-sanitized external-artifact node id) to a destination
// full task id. Kept at package scope so TransitivelyReduce
// can operate on slices of it without an anonymous-struct
// signature.
type Edge struct {
	From, To string
}

// RenderMermaidBody produces the **raw** Mermaid source for a
// run's DAG — `flowchart TD` header, node declarations, edges,
// classDef lines. No ``` fences, no ``%% run N`` comment header.
// This is the bytes that go into a `.mmd` file: consumers
// (mermaid.live, GitHub markdown, `mmdc` CLI, a preprint's
// minted block) add their own rendering wrapper as needed.
//
// Node ids are derived from task ids with non-alphanumeric
// characters replaced so Mermaid's parser accepts them. Labels
// use the human-readable short name + state glyph. Edges come
// from `depends_on`; artifact-derived edges are intentionally
// omitted to keep the graph readable — depends_on is the
// authoring-layer relation the user actually wrote down.
//
// Terminal states get CSS classes so the downstream renderer
// can color-code accepted / failed / skipped branches at a
// glance (mermaid.live, GitHub, and most editors honor the
// class definitions).
//
// Returns "" when the tasks JSON is malformed or the run
// response carried an error — callers that want an error
// surface should check for that and decide whether to write
// the file anyway.
func RenderMermaidBody(runData []byte, tasksData []byte) string {
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return ""
	}
	if errMsg, ok := run["error"].(string); ok && errMsg != "" {
		return ""
	}
	var tasks []map[string]interface{}
	if err := json.Unmarshal(tasksData, &tasks); err != nil {
		return ""
	}

	// Index full-task-id → sanitized node id. We keep this
	// map so Edge declarations can look up the node id without
	// re-sanitizing and risking drift.
	nodeID := make(map[string]string, len(tasks))
	// Track only task ids present in this run — edges pointing
	// outside (shouldn't happen for depends_on, but guarded)
	// get skipped rather than declaring mystery nodes.
	present := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		id, _ := t["id"].(string)
		if id == "" {
			continue
		}
		present[id] = true
		nodeID[id] = MermaidNodeID(id)
	}

	// Build the Edge set from depends_on. Collect first, reduce,
	// emit — so transitive reduction has the full picture before
	// any output lands. Without collecting upfront, a naive
	// emit-as-you-go would need a second pass anyway. The `Edge`
	// type is package-level so TransitivelyReduce can take and
	// return slices of it without an anonymous-struct dance.
	var edges []Edge
	edgeSet := map[Edge]bool{}
	addEdge := func(from, to string) {
		e := Edge{from, to}
		if edgeSet[e] {
			return
		}
		edgeSet[e] = true
		edges = append(edges, e)
	}
	for _, t := range tasks {
		id, _ := t["id"].(string)
		deps, _ := t["depends_on"].(string)
		if id == "" || deps == "" {
			continue
		}
		for _, parent := range strings.Split(deps, ",") {
			parent = strings.TrimSpace(parent)
			if parent == "" || !present[parent] {
				continue
			}
			addEdge(parent, id)
		}
	}

	// Cross-run "external artifact" node rendering was removed
	// with the branch-per-run model — branches isolate a run's
	// artifact state from other runs, so there's no cross-run
	// writer to surface as a dashed external node.

	// Transitive reduction on the combined Edge set. For each
	// Edge u→v, drop it if v is reachable from any other
	// out-neighbor of u through the remaining edges. Canonical
	// DAG form for visualization: no edges whose endpoint is
	// already reachable via a longer path. Applied uniformly to
	// intra-run and external-artifact edges so a "discover →
	// tag" redundancy disappears the same way a "📎 path → tag"
	// redundancy would if `tag` transitively already depends on
	// it through another task.
	edges = TransitivelyReduce(edges)

	// Emit.
	var b strings.Builder
	b.WriteString("flowchart TD\n")

	// Task nodes.
	for _, t := range tasks {
		id, _ := t["id"].(string)
		if id == "" {
			continue
		}
		state, _ := t["state"].(string)
		skipReason, _ := t["skip_reason"].(string)
		label := MermaidEscape(TaskShortName(t)) + " " + StateIconFor(state, skipReason)
		cls := MermaidStateClass(state)
		b.WriteString(fmt.Sprintf("    %s[\"%s\"]", nodeID[id], label))
		if cls != "" {
			b.WriteString(":::" + cls)
		}
		b.WriteString("\n")
	}

	// Edges — intra-run depends_on only. Cross-run external
	// nodes were removed with the branch-per-run model.
	b.WriteString("\n")
	for _, e := range edges {
		from := nodeID[e.From]
		if from == "" {
			from = e.From
		}
		to := nodeID[e.To]
		if to == "" {
			to = e.To
		}
		b.WriteString(fmt.Sprintf("    %s --> %s\n", from, to))
	}

	// Class definitions — one per terminal/active state. Colors
	// are deliberately mild so the graph reads well on both
	// white and dark backgrounds. External-artifact nodes use a
	// dashed stroke so the viewer can tell them apart from
	// in-run work at a glance.
	b.WriteString("\n")
	b.WriteString("    classDef accepted fill:#d4edda,stroke:#28a745,color:#000\n")
	b.WriteString("    classDef active fill:#cce5ff,stroke:#007bff,color:#000\n")
	b.WriteString("    classDef ready fill:#fff3cd,stroke:#ffc107,color:#000\n")
	b.WriteString("    classDef pending fill:#f8f9fa,stroke:#6c757d,color:#000\n")
	b.WriteString("    classDef failed fill:#f8d7da,stroke:#dc3545,color:#000\n")
	b.WriteString("    classDef skipped fill:#e2e3e5,stroke:#6c757d,stroke-dasharray:4 2,color:#000\n")
	b.WriteString("    classDef parked fill:#e7e3f4,stroke:#6f42c1,stroke-dasharray:2 2,color:#000\n")
	return b.String()
}

// TransitivelyReduce removes edges whose endpoint is already
// reachable from the source through other edges. The canonical
// DAG visualization convention: if u → v and u → w → ... → v
// both exist, drop u → v so the diagram doesn't clutter with
// a redundant direct Edge.
//
// Algorithm: build the forward adjacency once, then for each
// Edge (u, v) BFS from every other out-neighbor of u looking
// for v. If any reaches v through the remaining edges, u → v
// is redundant. O(V·E) for the reachability checks — trivially
// fast for the scale of runs we visualize.
//
// Input is a slice of edges; output preserves the input order
// of retained edges so the emitted Mermaid output stays stable
// across calls (useful for the no-op detection in
// enju_export_diagram — re-export should produce byte-identical
// content when the graph hasn't changed).
func TransitivelyReduce(edges []Edge) []Edge {
	if len(edges) < 2 {
		return edges
	}
	// Forward adjacency.
	adj := map[string][]string{}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	// Reachable(w, target) with a BFS that skips the direct
	// Edge u→v we're testing. We parameterize the skip so the
	// caller can ask "is v reachable from w without using u→v
	// directly?" — critical when u→v is a redundancy candidate
	// that would also count itself as a "longer path."
	reachableSkipping := func(start, target, skipFrom, skipTo string) bool {
		if start == target {
			return true
		}
		visited := map[string]bool{start: true}
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, next := range adj[cur] {
				if cur == skipFrom && next == skipTo {
					continue
				}
				if next == target {
					return true
				}
				if visited[next] {
					continue
				}
				visited[next] = true
				queue = append(queue, next)
			}
		}
		return false
	}
	kept := make([]Edge, 0, len(edges))
	for _, e := range edges {
		redundant := false
		for _, w := range adj[e.From] {
			if w == e.To {
				continue
			}
			if reachableSkipping(w, e.To, e.From, e.To) {
				redundant = true
				break
			}
		}
		if !redundant {
			kept = append(kept, e)
		}
	}
	return kept
}

// RunStatusMermaid is the tool-reply wrapper: renders the
// raw body (see RenderMermaidBody) and wraps it in a ```mermaid
// code fence + a %% comment header naming the run, so the LLM
// can paste the whole block into its reply and have it render
// in any Markdown viewer. For **file** writes (enju_export_diagram)
// use RenderMermaidBody directly — the file should be pure
// Mermaid source, no fence.
//
func RunStatusMermaid(runData []byte, tasksData []byte) string {
	// Error paths: mirror the old behavior so existing callers
	// still see a friendly "✗ Run not found" line instead of an
	// empty fenced block. RenderMermaidBody returns "" on those
	// inputs, so we detect them here by re-peeking at the run.
	var run map[string]interface{}
	if err := json.Unmarshal(runData, &run); err != nil {
		return string(runData)
	}
	if errMsg, ok := run["error"].(string); ok && errMsg != "" {
		return fmt.Sprintf("✗ Run not found: %s", errMsg)
	}
	body := RenderMermaidBody(runData, tasksData)
	if body == "" {
		return string(tasksData)
	}

	runName, _ := run["name"].(string)
	projectID := JsonID(run["project_id"])
	seq, _ := run["seq"].(float64)

	var b strings.Builder
	b.WriteString("```mermaid\n")
	b.WriteString(fmt.Sprintf("%%%% Run %s #%d — %s\n", projectID, int(seq), runName))
	b.WriteString(body)
	b.WriteString("```\n")
	return b.String()
}

// MermaidNodeID sanitizes a full task id like "1:2:alpha:expand"
// into a valid Mermaid node identifier. Mermaid requires ids to
// start with a letter and contain only [A-Za-z0-9_], so we
// prefix with "t_" and replace any non-alphanumeric run with a
// single underscore.
func MermaidNodeID(taskID string) string {
	var b strings.Builder
	b.WriteString("t_")
	prevUnderscore := false
	for _, r := range taskID {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return b.String()
}

// MermaidEscape escapes the handful of characters that would
// break a Mermaid node label quoted with ". `"` becomes `#quot;`
// (Mermaid's own HTML-entity escape) and literal `]` is swapped
// for `)` since it's the bracket we use to delimit the label.
// No-op for most task names in practice.
func MermaidEscape(s string) string {
	s = strings.ReplaceAll(s, `"`, `#quot;`)
	s = strings.ReplaceAll(s, `]`, `)`)
	return s
}

// MermaidStateClass maps a task state to the Mermaid class
// name declared at the bottom of the flowchart. Non-terminal
// "claimed" / "running" / "collecting" all share the `active`
// class — they're all "in flight" to the reader.
func MermaidStateClass(state string) string {
	switch state {
	case "accepted", "completed":
		return "accepted"
	case "ready":
		return "ready"
	case "claimed", "running", "collecting":
		return "active"
	case "pending":
		return "pending"
	case "failed":
		return "failed"
	case "skipped":
		return "skipped"
	case "parked":
		return "parked"
	default:
		return ""
	}
}

func TaskDetail(taskData []byte, inputsData []byte, viewer string) string {
	var task map[string]interface{}
	if err := json.Unmarshal(taskData, &task); err != nil {
		return string(taskData)
	}

	if errMsg, ok := task["error"].(string); ok {
		return fmt.Sprintf("✗ Task not found: %s", errMsg)
	}

	var b strings.Builder

	id, _ := task["id"].(string)
	runID := JsonID(task["run_id"])
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
	icon := StateIcon(state)
	b.WriteString(fmt.Sprintf("Task #%d: %s %s\n", int(seq), id, icon))
	b.WriteString(fmt.Sprintf("  Run:  #%s\n", runID))
	b.WriteString(fmt.Sprintf("  Action:   %s\n", FriendlyActionLabel(action)))
	b.WriteString(fmt.Sprintf("  State:    %s\n", StateLabel(state)))
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
		// Model attribution (operator/model design): when the
		// task has a completed submission, show which LLM was
		// credited. Empty for pre-1.4 rows and unaided humans —
		// in both cases the line is suppressed entirely so it
		// doesn't show up as "Model: " with nothing after.
		if model, _ := task["model"].(string); model != "" {
			b.WriteString(fmt.Sprintf("  Model:    %s\n", model))
		}
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
			b.WriteString(fmt.Sprintf("  Commit:   %s\n", ShortSHA(c)))
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
		if opts := ParseVoteOptionsForDisplay(voteOptsRaw); len(opts) > 0 {
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
		activeClaims := StringSliceFromAny(task["active_claimants"])
		state, _ := task["state"].(string)
		taskAction, _ := task["action"].(string)
		visibility, _ := task["visibility"].(string)
		anonymize, _ := task["anonymize"].(bool)
		deadline, _ := task["vote_deadline"].(string)
		deadlineAt, _ := task["vote_deadline_at"].(string)
		b.WriteString(VotingBlock(taskAction, citizens, minQuorum, threshold, state, voteSubs, activeClaims, visibility, viewer, anonymize, deadline, deadlineAt))
	}

	// Show environment requirements if present
	if reqRaw, ok := task["requirements"].(string); ok {
		b.WriteString(Requirements(reqRaw))
	}

	// Show named outputs schema if present
	if outputsRaw, ok := task["outputs"].(string); ok {
		b.WriteString(OutputsSchema(outputsRaw))
	}

	// Show access restrictions (assign_to, require_role) if any.
	assignTo := StringSliceFromAny(task["assign_to"])
	requireRole, _ := task["require_role"].(string)
	if s := AssignmentSchema(assignTo, requireRole); s != "" {
		b.WriteString(s)
	}

	// Show artifact reads/writes schema if present.
	// writes_artifacts is polymorphic on the wire post-Phase-A
	// (object form {path,track}) — extractor handles both.
	reads := StringSliceFromAny(task["reads_artifacts"])
	writes := WriteArtifactPathsFromAny(task["writes_artifacts"])
	if s := ArtifactsSchema(reads, writes); s != "" {
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
			missingArts := StringSliceFromAny(inputs["missing_artifacts"])
			if len(artMap) > 0 || len(missingArts) > 0 {
				b.WriteString("\n")
				b.WriteString(ResolvedArtifactsBlock(artMap, missingArts))
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

// ListTemplates renders the enju_list_templates response
// as a scannable menu. Each entry includes the template's path,
// name, one-line description snippet, and a compact param
// summary ("disease, tissue=whole blood") so the LLM can pick
// a recipe without drilling into each one.
func ListTemplates(data []byte) string {
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if errMsg, ok := resp["error"].(string); ok {
		return "✗ " + errMsg
	}
	templates, _ := resp["templates"].([]interface{})
	if len(templates) == 0 {
		return "No templates found in this project.\n\nTemplates are reusable run recipes stored under enju/templates/*.yaml in the project git repo. To add one, commit a YAML file to enju/templates/ with a top-level params: block. Any existing run YAML can be promoted to a template by copying it into enju/templates/."
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

// DescribeTemplate renders the full metadata for one
// template as a drill-down view. This is what the LLM reads
// right before gathering param values from the user: it has
// the full description prose, plus every param's type,
// default, and human-readable description.
func DescribeTemplate(data []byte) string {
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

func CreateRun(data []byte) string {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data)
	}

	if errMsg, ok := result["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to create run: %s", errMsg)
	}

	name, _ := result["name"].(string)
	projectID := JsonID(result["project_id"])
	seq, _ := result["seq"].(float64)
	taskCount, _ := result["task_count"].(float64)
	sourcePath, _ := result["source_path"].(string)
	sourceSHA, _ := result["source_commit_sha"].(string)
	branch, _ := result["branch"].(string)

	var b strings.Builder
	if branch != "" {
		b.WriteString(fmt.Sprintf("✓ Run created in project #%s as run #%d: %s on branch %q\n", projectID, int(seq), name, branch))
	} else {
		b.WriteString(fmt.Sprintf("✓ Run created in project #%s as run #%d: %s\n", projectID, int(seq), name))
	}
	b.WriteString(fmt.Sprintf("  Tasks: %d\n", int(taskCount)))
	if sourcePath != "" {
		line := fmt.Sprintf("  Source: %s", sourcePath)
		if sourceSHA != "" {
			line += fmt.Sprintf(" @%s", ShortSHA(sourceSHA))
		}
		b.WriteString(line + "\n")
	}

	// Parse-time warnings are non-fatal advisories (missing
	// review-consumer, compute task with no declared deps,
	// etc). The run was created successfully, but the author
	// should see these so they can fix the YAML before the
	// warning becomes a silent runtime failure.
	if warnings, ok := result["warnings"].([]interface{}); ok && len(warnings) > 0 {
		b.WriteString("\n⚠ Warnings:\n")
		for _, w := range warnings {
			msg, _ := w.(string)
			if msg == "" {
				continue
			}
			b.WriteString("  - " + msg + "\n")
		}
	}

	b.WriteString(fmt.Sprintf("\nUse enju_run_status(project_id=%s, run_id=%d) or enju_list_ready_tasks to see tasks.", projectID, int(seq)))
	return b.String()
}

// ShortSHA returns the first 7 chars of a git commit SHA, the
// standard abbreviation. Leaves short inputs alone.
func ShortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// --- Helpers ---

// Profile renders a citizen's profile with their
// contribution record. Phase G: no scoring formula, just
// factual counts from the contribution-events log.
// contribData may be nil (best-effort fetch).
func Profile(data []byte, contribData []byte) string {
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
			completed := int(JsonFloat(contrib["tasks_completed"]))
			rejected := int(JsonFloat(contrib["tasks_rejected"]))
			released := int(JsonFloat(contrib["tasks_released"]))
			reviews := int(JsonFloat(contrib["reviews_given"]))
			approves := int(JsonFloat(contrib["review_approves"]))
			rejects := int(JsonFloat(contrib["review_rejects"]))
			votes := int(JsonFloat(contrib["votes_cast"]))
			tokens := int64(JsonFloat(contrib["tokens_total"]))
			projects := int(JsonFloat(contrib["project_count"]))

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
				impactTasks := int(JsonFloat(impact["tasks"]))
				impactProjects := int(JsonFloat(impact["projects"]))
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

func JsonFloat(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func Dashboard(data []byte) string {
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
			runID := JsonID(tm["run_id"])
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
			runID := JsonID(tm["run_id"])
			b.WriteString(fmt.Sprintf("  ✓ #%d [%s]    run #%s\n", int(seq), tid, runID))
		}
	}

	return b.String()
}

// JsonID extracts an ID that could be a string or a number from JSON.
func JsonID(v interface{}) string {
	switch id := v.(type) {
	case float64:
		return fmt.Sprintf("%d", int64(id))
	case string:
		return id
	default:
		return fmt.Sprintf("%v", v)
	}
}

// VotingBlock renders the multi-citizen voting / review
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
func VotingBlock(action string, citizens, minQuorum int, threshold, state string, voteSubs []interface{}, activeClaimants []string, visibility, viewerUsername string, anonymize bool, deadline, deadlineAt string) string {
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
	type ballot struct{ user, option, model string }
	ballots := make([]ballot, 0, submitted)
	for _, v := range voteSubs {
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		user, _ := m["username"].(string)
		option, _ := m["option"].(string)
		model, _ := m["model"].(string)
		counts[option]++
		ballots = append(ballots, ballot{user: user, option: option, model: model})
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
		SortStrings(keys)
		tally := make([]string, 0, len(keys))
		for _, k := range keys {
			tally = append(tally, fmt.Sprintf("%s=%d", k, counts[k]))
		}
		b.WriteString("  Tally:    " + strings.Join(tally, ", ") + "\n")
	}

	// Per-voter ballots. The optional " (model)" suffix surfaces
	// per-voter model attribution from the operator/model design
	// — useful for cross-model quorum runs where seeing which
	// model produced each verdict is the whole point. Suppressed
	// per-entry when the field is empty (pre-1.4 rows or unaided
	// humans) so the line stays compact when nobody declared a
	// model.
	if len(ballots) > 0 {
		parts := make([]string, 0, len(ballots))
		for _, b := range ballots {
			entry := fmt.Sprintf("%s→%s", b.user, b.option)
			if b.model != "" {
				entry += fmt.Sprintf(" (%s)", b.model)
			}
			parts = append(parts, entry)
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

// VoteOptionsBlock renders a vote task's declared options as
// a `── Options ──` block for inclusion in the claim response,
// mirroring ReviewingBlock. Highlights the winning option
// if one has already been recorded (vote_choice is populated),
// labels each option, and surfaces `activates:` targets so the
// voter can see the structural consequences of each choice.
// When vote_threshold / vote_deadline / min_quorum are set, they
// ride along as a trailing summary line.
func VoteOptionsBlock(opts []voteOptionView, task map[string]interface{}) string {
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

// ParseVoteOptionsForDisplay decodes the JSON-encoded vote_options
// column into the display projection. Returns nil on any parse
// failure rather than propagating an error — the formatter
// should degrade gracefully if the column is malformed (a
// storage-side consistency bug is surfaced elsewhere).
func ParseVoteOptionsForDisplay(optionsJSON string) []voteOptionView {
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

func StateIcon(state string) string {
	return StateIconFor(state, "")
}

// StateIconFor is StateIcon with skip-reason context so the
// tree renderer can distinguish "skipped because upstream
// failed" (blocked by a real failure — ⊘) from vote-cascade
// skips (intentional gating — ⚫). Callers that don't have
// the reason handy use the plain StateIcon wrapper.
func StateIconFor(state, skipReason string) string {
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
		if skipReason != "" {
			// "skipped because a dependency failed" — ⊘
			// signals blocking, not intentional gating.
			return "⊘"
		}
		return "⚫"
	case "failed":
		return "🔴"
	case "invalid", "invalidated", "rejected":
		return "🔴"
	case "parked":
		// ⏸ (pause) signals "work preserved, awaiting
		// reconciliation". Visually distinct from ⚫ (vote-
		// cascade terminal skip) and ⊘ (blocked by failure)
		// because the semantics are different: a parked task
		// WILL come back — either restored to its prior state
		// on matching re-accept, or deleted on non-match.
		return "⏸"
	default:
		return "?"
	}
}

func FriendlyActionLabel(action string) string {
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


func StateLabel(state string) string {
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
	case "parked":
		return "parked (awaiting reconciliation)"
	default:
		return state
	}
}

func ProgressBar(done, total, width int) string {
	if total == 0 {
		return ""
	}
	filled := (done * width) / total
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	pct := (done * 100) / total
	return fmt.Sprintf("%s %d%%", bar, pct)
}

// SetDefaultBranchResult renders the JSON returned by PUT
// /projects/{p}/default_branch.
func SetDefaultBranchResult(data []byte) string {
	var resp struct {
		ProjectID     int64  `json:"project_id"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	return fmt.Sprintf("✓ Default branch for project #%d is now: %s", resp.ProjectID, resp.DefaultBranch)
}

// AddMemberResult renders the JSON returned by POST
// /projects/{p}/members.
func AddMemberResult(data []byte) string {
	var resp struct {
		Username string `json:"username"`
		Name     string `json:"name,omitempty"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if resp.Name != "" && resp.Name != resp.Username {
		return fmt.Sprintf("✓ Added @%s (%s) as %s", resp.Username, resp.Name, resp.Role)
	}
	return fmt.Sprintf("✓ Added @%s as %s", resp.Username, resp.Role)
}

// RemoveMemberResult renders the JSON returned by DELETE
// /projects/{p}/members/.... Distinguishes self-leave from
// owner-removes-other in the rendered text.
func RemoveMemberResult(data []byte) string {
	var resp struct {
		ProjectID int64  `json:"project_id"`
		Citizen   string `json:"citizen"`
		SelfLeave bool   `json:"self_leave"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if resp.SelfLeave {
		return fmt.Sprintf("✓ Left project #%d", resp.ProjectID)
	}
	return fmt.Sprintf("✓ Removed @%s from project #%d", resp.Citizen, resp.ProjectID)
}

// SetMemberRoleResult renders the JSON returned by PUT
// /projects/{p}/members/.../role. Distinguishes a fresh role
// flip from a no-op.
func SetMemberRoleResult(data []byte, verb string) string {
	var resp struct {
		Citizen string `json:"citizen"`
		Role    string `json:"role"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if !resp.Changed {
		return fmt.Sprintf("• @%s already has role %s [no-op]", resp.Citizen, resp.Role)
	}
	return fmt.Sprintf("✓ %s @%s — role is now %s", verb, resp.Citizen, resp.Role)
}

// UpdateProfileResult renders the JSON returned by PUT
// /citizens/by-username/{user}/profile. Single-line confirm
// echoing the new display name (or username when name not
// touched).
func UpdateProfileResult(data []byte, fallbackLabel string) string {
	var resp struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(data, &resp)
	return fmt.Sprintf("✓ Profile updated: %s", fallbackLabel)
}

// RegisterBotResult renders the JSON returned by POST
// /citizens/me/bots. Token shown ONCE — formatter highlights
// it so the caller can't miss it.
func RegisterBotResult(data []byte) string {
	var resp struct {
		Username   string `json:"username"`
		Name       string `json:"name"`
		ParentName string `json:"parent_name"`
		Label      string `json:"label,omitempty"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✓ Bot registered: @%s (%s)\n", resp.Username, resp.Name)
	fmt.Fprintf(&b, "  Owned by: @%s\n", resp.ParentName)
	if resp.Label != "" {
		fmt.Fprintf(&b, "  Label:    %s\n", resp.Label)
	}
	fmt.Fprintf(&b, "\n  TOKEN (stash this NOW — cannot be retrieved later):\n  %s\n", resp.Token)
	fmt.Fprintf(&b, "\n  To use: drop it into the bot's launcher as the Bearer token.\n")
	fmt.Fprintf(&b, "  To revoke: enju_revoke_token token=%s\n", resp.Token)
	return b.String()
}

// RevokeTokenResult renders the JSON returned by POST
// /tokens/revoke.
func RevokeTokenResult(data []byte) string {
	var resp struct {
		Revoked bool `json:"revoked"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if resp.Revoked {
		return "✓ Token revoked. It will no longer authenticate."
	}
	return "Token state unchanged."
}

// RegisterModelResult renders the JSON returned by POST
// /models.
func RegisterModelResult(data []byte) string {
	var resp struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	return fmt.Sprintf("✓ Model registered: %s (%s)", resp.Username, resp.DisplayName)
}

// FileIssueResult renders the JSON returned by POST
// /projects/{p}/issues. Single-line confirmation with the
// canonical ISSUE-NNN slug and the title.
func FileIssueResult(data []byte) string {
	var resp struct {
		Slug     string `json:"slug"`
		Severity string `json:"severity"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	return fmt.Sprintf("✓ Filed %s [%s]: %s", resp.Slug, resp.Severity, resp.Title)
}

// TriageIssueResult renders the issue JSON returned after a
// triage update. The api emits the full issue map; we only
// surface a short confirmation with the new severity.
func TriageIssueResult(data []byte) string {
	var resp struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	return fmt.Sprintf("✓ Triaged %s [severity=%s]", resp.ID, resp.Severity)
}

// CloseIssueResult renders the issue JSON returned after a
// close. Surfaces the canonical slug + final status.
func CloseIssueResult(data []byte) string {
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	return fmt.Sprintf("✓ Closed %s [status=%s]", resp.ID, resp.Status)
}

// SetCycleBudgetResult renders the JSON returned by POST
// /projects/{p}/runs/{r}/cycle_budget. Carries the
// post-update used/max so the caller can confirm the new cap.
func SetCycleBudgetResult(data []byte) string {
	var resp struct {
		RunID       string `json:"run_id"`
		CycleBudget struct {
			Used int `json:"used"`
			Max  int `json:"max"`
		} `json:"cycle_budget"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	return fmt.Sprintf("✓ Cycle budget set to %d for run %s (used: %d)", resp.CycleBudget.Max, resp.RunID, resp.CycleBudget.Used)
}

// PauseRunResult renders the JSON returned by POST
// /projects/{p}/runs/{r}/pause as a friendly one-line status.
// The "changed" flag distinguishes a fresh pause from an
// already-paused no-op so the user can tell whether the call
// actually did anything.
func PauseRunResult(data []byte) string {
	var resp struct {
		RunID   string `json:"run_id"`
		State   string `json:"state"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if !resp.Changed {
		return fmt.Sprintf("• Run %s already paused (state: %s) [no-op]", resp.RunID, resp.State)
	}
	return fmt.Sprintf("✓ Run %s paused (state: %s)", resp.RunID, resp.State)
}

// ResumeRunResult renders the JSON returned by POST
// /projects/{p}/runs/{r}/resume. State lands on `idle` when no
// ready work exists, `active` when ready tasks are present —
// surfaced verbatim so the caller can react.
func ResumeRunResult(data []byte) string {
	var resp struct {
		RunID string `json:"run_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	return fmt.Sprintf("✓ Run %s resumed (state: %s)", resp.RunID, resp.State)
}

// TerminateRunResult renders the JSON returned by POST
// /projects/{p}/runs/{r}/terminate. Surfaces the cascade
// fan-out (skipped tasks + abandoned claims) so the caller
// sees how much in-flight work just got dropped without a
// follow-up read.
func TerminateRunResult(data []byte) string {
	var resp struct {
		RunID           string `json:"run_id"`
		State           string `json:"state"`
		PriorState      string `json:"prior_state"`
		SkippedTasks    int    `json:"skipped_tasks"`
		AbandonedClaims int    `json:"abandoned_claims"`
		Reason          string `json:"reason"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	out := fmt.Sprintf("⊘ Run %s terminated (was: %s)\n", resp.RunID, resp.PriorState)
	out += fmt.Sprintf("  %d task(s) skipped, %d open claim(s) abandoned\n", resp.SkippedTasks, resp.AbandonedClaims)
	if resp.Reason != "" {
		out += fmt.Sprintf("  reason: %s\n", resp.Reason)
	}
	out += "  topic branches preserved in git; late-arriving submits will be refused"
	return out
}

// BotList renders the JSON returned by GET /citizens/me/bots
// (and the equivalent native MCP handler). Empty list returns
// the friendly bootstrap hint pointing at enju_register_bot.
//
// Wire shape: {"bots":[{id,username,name,role,registered,
// tokens:[{id,label,issued_at,revoked_at?}]}]}.
func BotList(data []byte) string {
	var resp struct {
		Bots []struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
			Role     string `json:"role"`
			Tokens   []struct {
				ID        int64  `json:"id"`
				Label     string `json:"label"`
				IssuedAt  string `json:"issued_at"`
				RevokedAt string `json:"revoked_at,omitempty"`
			} `json:"tokens"`
		} `json:"bots"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if len(resp.Bots) == 0 {
		return "You don't own any bots yet. Use enju_register_bot to create one."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your bots (%d):\n", len(resp.Bots))
	for _, bot := range resp.Bots {
		fmt.Fprintf(&b, "\n@%s — %s (role: %s)\n", bot.Username, bot.Name, bot.Role)
		if len(bot.Tokens) == 0 {
			b.WriteString("  (no tokens)\n")
			continue
		}
		for _, t := range bot.Tokens {
			label := t.Label
			if label == "" {
				label = "(no label)"
			}
			status := "active"
			if t.RevokedAt != "" {
				status = "revoked " + t.RevokedAt
			}
			fmt.Fprintf(&b, "  token #%d  %s  issued %s  [%s]\n", t.ID, label, t.IssuedAt, status)
		}
	}
	return b.String()
}

// ModelList renders the model catalog JSON. Empty list returns
// the catalog-empty hint (the migration seeds 10 popular models;
// empty means something's wrong server-side).
//
// Wire shape: {"models":[{id,username,name}]}.
func ModelList(data []byte) string {
	var resp struct {
		Models []struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}
	if len(resp.Models) == 0 {
		return "Catalog is empty. (Unexpected — the migration seeds 10 popular models. Check coordinator logs.)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Model catalog (%d):\n", len(resp.Models))
	for _, m := range resp.Models {
		fmt.Fprintf(&b, "  %-30s  %s\n", m.Username, m.Name)
	}
	b.WriteString("\nUse the username (left column) as the -model flag value.\n")
	return b.String()
}

// EventsStatus renders the EventStore stats JSON as a
// human-readable summary. Both the fat-client HTTP-forwarder
// and the coord-side native handler call this so operators
// see identical output.
//
// Field shape: enabled (bool), enqueued / persisted / dropped
// (int64 counters), queue_depth + queue_capacity (int).
func EventsStatus(data []byte) string {
	var status struct {
		Enabled       bool  `json:"enabled"`
		Enqueued      int64 `json:"enqueued"`
		Persisted     int64 `json:"persisted"`
		Dropped       int64 `json:"dropped"`
		QueueDepth    int   `json:"queue_depth"`
		QueueCapacity int   `json:"queue_capacity"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return string(data)
	}
	state := "ENABLED"
	if !status.Enabled {
		state = "DISABLED"
	}
	return fmt.Sprintf(
		"Event store: %s\n"+
			"  Enqueued:    %d events\n"+
			"  Persisted:   %d events\n"+
			"  Dropped:     %d events\n"+
			"  Queue depth: %d / %d (in-flight / capacity)\n",
		state, status.Enqueued, status.Persisted, status.Dropped, status.QueueDepth, status.QueueCapacity,
	)
}

// IssueList renders the JSON returned by GET /api/v1/projects/
// {id}/issues (and the equivalent native MCP handler) as a
// per-issue summary line. Empty list returns the canonical
// "(no issues match)" string.
func IssueList(data []byte) string {
	var issues []map[string]interface{}
	if err := json.Unmarshal(data, &issues); err != nil {
		return string(data)
	}
	if len(issues) == 0 {
		return "(no issues match)"
	}
	var b strings.Builder
	for _, it := range issues {
		id, _ := it["id"].(string)
		title, _ := it["title"].(string)
		status, _ := it["status"].(string)
		severity, _ := it["severity"].(string)
		b.WriteString(fmt.Sprintf("• %s [%s/%s] %s\n", id, status, severity, title))
	}
	return b.String()
}

// IssueDetail renders one issue as a YAML-frontmatter +
// markdown body — the same shape a future filesystem mirror
// will write to disk. Optional fields are emitted only when
// non-nil so the rendered output stays parseable.
func IssueDetail(data []byte) string {
	var it map[string]interface{}
	if err := json.Unmarshal(data, &it); err != nil {
		return string(data)
	}
	out := fmt.Sprintf("---\nid: %v\ntitle: %v\nstatus: %v\nseverity: %v\nfiled_by: %v\nfiled_at: %v\n",
		it["id"], it["title"], it["status"], it["severity"], it["filed_by"], it["filed_at"])
	for _, k := range []string{"found_in_run_seq", "found_in_task_id", "triaged_at", "triaged_by", "closed_at", "closed_by_task_id"} {
		if v, ok := it[k]; ok && v != nil {
			out += fmt.Sprintf("%s: %v\n", k, v)
		}
	}
	out += "---\n"
	if body, _ := it["body"].(string); body != "" {
		out += "\n" + body + "\n"
	}
	return out
}

// EventListJSONL renders an event list as one compact JSON
// object per line. Matches the fat-client enju_show_events
// output exactly so both transports produce identical text.
//
// Empty list returns the canonical "no events match" string;
// the caller doesn't need to check len() first.
func EventListJSONL(events []map[string]interface{}) string {
	if len(events) == 0 {
		return "(no events match the given filters)"
	}
	var b strings.Builder
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// EventLineRecent renders one event as a concise human-readable
// line: "[timestamp] type[/subtype] task=... by=...". Matches
// the fat-client enju_recent_events per-line output.
//
// Timestamp is trimmed to "YYYY-MM-DD HH:MM:SS" UTC for
// readability (vs the RFC3339Nano shape on the wire). Optional
// fields are omitted cleanly when missing.
func EventLineRecent(e map[string]interface{}) string {
	ts, _ := e["ts"].(string)
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = t.UTC().Format("2006-01-02 15:04:05")
	}
	etype, _ := e["type"].(string)
	if subtype, _ := e["subtype"].(string); subtype != "" {
		etype = etype + "/" + subtype
	}
	line := fmt.Sprintf("[%s] %s", ts, etype)
	if task, _ := e["task_id"].(string); task != "" {
		line += " task=" + task
	}
	if cit, _ := e["citizen"].(string); cit != "" {
		line += " by=@" + cit
	}
	return line
}

// EventListRecent renders an event list as concise per-line
// text via EventLineRecent. Empty list returns the canonical
// "no recent events" string.
func EventListRecent(events []map[string]interface{}) string {
	if len(events) == 0 {
		return "(no recent events)"
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString(EventLineRecent(e))
		b.WriteByte('\n')
	}
	return b.String()
}

// IterationList renders the JSON returned by
// GET /api/v1/tasks/{id}/iterations (and the equivalent native
// MCP handler) as a plain-text history table. Both the
// fat-client HTTP-forwarder and the coord-side native handler
// produce identical output by routing through here.
//
// Field shape (matches api.handleListIterations / coord-side
// equivalent): seq, citizen, outcome, claimed_at, submitted_at?,
// commit_sha?, branch?, review_decision?, option?, model?,
// content? (legacy — content lives in git now, but the field is
// still rendered if present for backwards compat).
func IterationList(taskID string, data []byte) string {
	var iters []map[string]interface{}
	if err := json.Unmarshal(data, &iters); err != nil {
		return string(data)
	}
	if len(iters) == 0 {
		return fmt.Sprintf("(no iterations for %s — task hasn't been claimed yet)", taskID)
	}
	out := fmt.Sprintf("Iteration history for %s:\n\n", taskID)
	for _, it := range iters {
		seq, _ := it["seq"].(float64)
		citizen, _ := it["citizen"].(string)
		outcome, _ := it["outcome"].(string)
		out += fmt.Sprintf("  iter-%d  @%s  [%s]\n", int(seq), citizen, outcome)
		if claimed, ok := it["claimed_at"].(string); ok {
			out += "    claimed_at:  " + claimed + "\n"
		}
		if submitted, ok := it["submitted_at"].(string); ok {
			out += "    submitted_at: " + submitted + "\n"
		}
		if commit, ok := it["commit_sha"].(string); ok && commit != "" {
			short := commit
			if len(short) > 8 {
				short = short[:8]
			}
			out += "    commit:       " + short + "\n"
		}
		if branch, ok := it["branch"].(string); ok && branch != "" {
			out += "    branch:       " + branch + "\n"
		}
		if dec, ok := it["review_decision"].(string); ok && dec != "" {
			out += "    review:       " + dec + "\n"
		}
		if opt, ok := it["option"].(string); ok && opt != "" {
			out += "    option:       " + opt + "\n"
		}
		if model, ok := it["model"].(string); ok && model != "" {
			out += "    model:        " + model + "\n"
		}
		if content, ok := it["content"].(string); ok && content != "" {
			snippet := content
			if len(snippet) > 80 {
				snippet = snippet[:77] + "..."
			}
			out += "    content:      " + snippet + "\n"
		}
	}
	return out
}

func Truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
