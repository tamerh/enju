package notify

// Layer 1 defaults — compiled-in rules that fire for every
// citizen running the notify loop. The "platform pulse" of
// docs/notifications.md: things you'd want to know about
// without configuring anything. Toggleable per-name via
// Config.DisableDefaults.
//
// Today's set is deliberately small. Most of the design doc's
// proposed defaults (assigned_task_ready, my_review_resolved,
// issue_in_owned_project) need server-side enrichment of the
// events response — fields like assign_to and run.creator
// aren't currently included. Phase 4d (server enrichment +
// Tier 1 wiring) unlocks the rest. For now we ship two
// defaults that work with the current Event shape so the
// framework is exercised end-to-end:
//
//   my_task_completed — fires when a task you acted on resolves
//   my_task_failed   — fires when a task you acted on fails
//
// Both filter by Citizen=={{me}} which matches today's event
// emission semantics (the citizen who submitted the work).
// Adding richer defaults later is purely additive — append to
// compiledDefaults and define rate limits in ratelimit.go.

// compiledDefaults returns the built-in Layer 1 rules.
// Returned fresh each call so callers can safely mutate the
// slice without affecting future calls.
func compiledDefaults() []Rule {
	return []Rule{
		{
			Name: "my_task_completed",
			When: Predicate{
				EventType: "task_completed",
				Citizen:   "{{me}}",
			},
			Kind:    "desktop",
			Message: "✓ Task {{task_id}} completed ({{type}}/{{subtype}})",
		},
		{
			Name: "my_task_failed",
			When: Predicate{
				EventType: "task_failed",
				Citizen:   "{{me}}",
			},
			Kind:    "desktop",
			Message: "✗ Task {{task_id}} failed",
		},
	}
}

// effectiveDefaults filters compiledDefaults by the user's
// DisableDefaults list. The literal "all" disables every
// built-in default; otherwise individual names are removed.
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
