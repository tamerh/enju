package notify

// Layer 1 defaults — compiled-in rules that fire for every
// citizen running the notify loop. The "platform pulse" of
// docs/notifications.md: things you'd want to know about
// without configuring anything. Toggleable per-name via
// Config.DisableDefaults.
//
// Project scope is implicit: notifySession polls one project's
// /events feed, so every event reaching this matcher is already
// the active project's event. Predicate filters narrow further
// (Citizen=={{me}} for "my-X" rules; type-only for project-pulse
// rules that fire on any project member's action).
//
// Three categories shipped:
//
//   "my-X" defaults — fire only on events I caused/own
//     my_task_completed
//     my_task_failed
//
//   "assigned to me" defaults — fire on tasks routed to me
//     assigned_task_ready
//
//   project-pulse defaults — fire on any project member's events
//     branch_merged
//     issue_filed
//     cycle_budget_exhausted
//     task_request_changes
//     run_completed
//     run_paused
//     run_resumed
//
// Adding richer "for me specifically" rules (my_review_resolved,
// my_run_failed) needs further server-side enrichment of
// /events — see notifications.md.

// compiledDefaults returns the built-in Layer 1 rules.
// Returned fresh each call so callers can safely mutate the
// slice without affecting future calls.
func compiledDefaults() []Rule {
	return []Rule{
		// --- "my-X" defaults — citizen-scoped ---
		{
			Name: "my_task_completed",
			When: Predicate{
				EventType: "task_completed",
				Citizen:   "{{me}}",
			},
			Message: "Task {{task_id}} completed ({{type}}/{{subtype}})",
		},
		{
			Name: "my_task_failed",
			When: Predicate{
				EventType: "task_failed",
				Citizen:   "{{me}}",
			},
			Message: "Task {{task_id}} failed",
		},

		// --- "assigned to me" defaults ---
		{
			Name: "assigned_task_ready",
			When: Predicate{
				EventType: "task_ready",
				AssignTo:  "{{me}}",
			},
			Message: "Task {{task_id}} is ready for you ({{type}}/{{subtype}})",
		},

		// --- project-pulse defaults — fire on any actor ---
		{
			Name: "branch_merged",
			When: Predicate{
				EventType: "branch_merged",
			},
			Message: "Topic merged for {{task_id}}",
		},
		{
			Name: "issue_filed",
			When: Predicate{
				EventType: "issue_filed",
			},
			Message: "Issue filed by @{{citizen}}",
		},
		{
			Name: "cycle_budget_exhausted",
			When: Predicate{
				EventType: "cycle_budget_exhausted",
			},
			Message: "Cycle budget exhausted — run auto-paused (runaway loop suspected)",
		},
		{
			Name: "task_request_changes",
			When: Predicate{
				EventType: "task_request_changes",
			},
			// Without assign_to enrichment we can't say "your work
			// needs revision." The active-project scope already
			// narrows this to events the user cares about; the
			// message points at the task so the user can check.
			Message: "Review requested changes on {{task_id}}",
		},
		{
			Name: "run_completed",
			When: Predicate{
				EventType: "run_completed",
			},
			Message: "Run completed",
		},
		{
			Name: "run_paused",
			When: Predicate{
				EventType: "run_paused",
			},
			Message: "Run paused by @{{citizen}}",
		},
		{
			Name: "run_resumed",
			When: Predicate{
				EventType: "run_resumed",
			},
			Message: "Run resumed by @{{citizen}}",
		},
	}
}

// EffectiveDefaults filters compiledDefaults by the user's
// DisableDefaults list. The literal "all" disables every
// built-in default; otherwise individual names are removed.
// Exported so consumers (the enju_notifications MCP tool)
// can reuse the same filter logic the poll loop used.
func EffectiveDefaults(disabled []string) []Rule {
	return effectiveDefaults(disabled)
}

// effectiveDefaults is the unexported variant — kept for
// backward-compat with internal callers.
func effectiveDefaults(disabled []string) []Rule {
	if isDisabled("all", disabled) {
		return nil
	}
	all := compiledDefaults()
	out := all[:0]
	for _, r := range all {
		if isDisabled(r.Name, disabled) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func isDisabled(name string, list []string) bool {
	for _, d := range list {
		if d == name {
			return true
		}
	}
	return false
}
