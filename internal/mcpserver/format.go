package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
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
	b.WriteString("Enju Projects\n\n")

	for _, p := range projects {
		name, _ := p["name"].(string)
		state, _ := p["state"].(string)
		taskCount, _ := p["task_count"].(float64)
		id := jsonID(p["id"])

		icon := stateIcon(state)
		b.WriteString(fmt.Sprintf("  %s #%s  %-40s [%s]  %d tasks\n",
			icon, id, name, state, int(taskCount)))
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

	for _, t := range tasks {
		id, _ := t["id"].(string)
		_ = t["project_id"] // used via jsonID above
		prompt, _ := t["prompt"].(string)
		mode, _ := t["mode"].(string)
		deps, _ := t["depends_on"].(string)
		instanceKey, _ := t["instance_key"].(string)

		seq, _ := t["seq"].(float64)
		projectNum := jsonID(t["project_id"])
		b.WriteString(fmt.Sprintf("  → #%d [%s]\n", int(seq), id))
		b.WriteString(fmt.Sprintf("    Project: #%s", projectNum))
		if instanceKey != "" {
			b.WriteString(fmt.Sprintf("    Instance: %s", instanceKey))
		}
		if mode == "assisted" {
			b.WriteString("    Mode: assisted (human input needed)")
		}
		b.WriteString("\n")
		if deps != "" {
			b.WriteString(fmt.Sprintf("    Upstream: %s ✓\n", deps))
		}
		b.WriteString(fmt.Sprintf("    Prompt: \"%s\"\n", truncate(prompt, 120)))
		b.WriteString("\n")
	}

	return b.String()
}

// formatOutputsSchema renders the named outputs schema for display.
// Returns empty string if no outputs declared.
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

	// Show outputs schema if present
	if outputsRaw, ok := task["outputs"].(string); ok && outputsRaw != "" {
		b.WriteString(formatOutputsSchema(outputsRaw))
	}

	b.WriteString("\n")

	// Show resolved prompt if inputs available
	if inputsData != nil {
		var inputs map[string]interface{}
		if json.Unmarshal(inputsData, &inputs) == nil {
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
	completed, _ := result["project_completed"].(bool)

	b.WriteString(fmt.Sprintf("✓ Result accepted: %s\n", taskID))

	if completed {
		b.WriteString("\n🎉 Project completed! All tasks are done.\n")
	} else if newlyReady > 0 {
		b.WriteString(fmt.Sprintf("\nImpact: %d new task(s) unlocked and ready for work.\n", int(newlyReady)))
	}

	_ = status
	return b.String()
}

func formatProjectStatus(projectData []byte, tasksData []byte) string {
	var project map[string]interface{}
	if err := json.Unmarshal(projectData, &project); err != nil {
		return string(projectData)
	}

	var tasks []map[string]interface{}
	if err := json.Unmarshal(tasksData, &tasks); err != nil {
		return string(tasksData)
	}

	var b strings.Builder

	name, _ := project["name"].(string)
	state, _ := project["state"].(string)
	id := jsonID(project["id"])

	// Count by state
	counts := map[string]int{}
	for _, t := range tasks {
		s, _ := t["state"].(string)
		counts[s]++
	}
	total := len(tasks)
	done := counts["accepted"]

	b.WriteString(fmt.Sprintf("Project #%s: %s\n", id, name))
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
	projectID := jsonID(task["project_id"])
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
	b.WriteString(fmt.Sprintf("  Project:  #%s\n", projectID))
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

	// Show named outputs schema if present
	if outputsRaw, ok := task["outputs"].(string); ok {
		b.WriteString(formatOutputsSchema(outputsRaw))
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
				b.WriteString("\n── Resolved (with upstream data) ───────────\n")
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
		}
	}

	return b.String()
}

func formatCreateProject(data []byte) string {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data)
	}

	if errMsg, ok := result["error"].(string); ok {
		return fmt.Sprintf("✗ Failed to create project: %s", errMsg)
	}

	name, _ := result["name"].(string)
	id := jsonID(result["id"])
	taskCount, _ := result["task_count"].(float64)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("✓ Project #%s created: %s\n", id, name))
	b.WriteString(fmt.Sprintf("  Tasks: %d\n", int(taskCount)))
	b.WriteString(fmt.Sprintf("\nUse enju_list_ready_tasks(project_id=\"%s\") to see available tasks.", id))
	return b.String()
}

// --- Helpers ---

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
	score, _ := citizen["score"].(float64)
	completed, _ := citizen["tasks_completed"].(float64)
	timedOut, _ := citizen["tasks_timed_out"].(float64)

	var b strings.Builder

	// Profile
	b.WriteString(fmt.Sprintf("┌─────────────────────────────────────────┐\n"))
	b.WriteString(fmt.Sprintf("│  %-20s        Score: %-4.0f │\n", name, score))
	b.WriteString(fmt.Sprintf("│  Tasks: %.0f completed", completed))
	if timedOut > 0 {
		b.WriteString(fmt.Sprintf(", %.0f timed out", timedOut))
	}
	b.WriteString(fmt.Sprintf(", %d active", len(activeTasks)))
	b.WriteString(fmt.Sprintf("      │\n"))
	b.WriteString(fmt.Sprintf("└──────��─────────────────────────────────��┘\n"))

	// Active claims
	if len(activeTasks) > 0 {
		b.WriteString("\nActive claims:\n")
		for _, t := range activeTasks {
			tm, _ := t.(map[string]interface{})
			tid, _ := tm["id"].(string)
			seq, _ := tm["seq"].(float64)
			projectID := jsonID(tm["project_id"])
			b.WriteString(fmt.Sprintf("  ⏳ #%d [%s]    project #%s\n", int(seq), tid, projectID))
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
			projectID := jsonID(tm["project_id"])
			b.WriteString(fmt.Sprintf("  ✓ #%d [%s]    project #%s\n", int(seq), tid, projectID))
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
