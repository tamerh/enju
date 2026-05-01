package notify

// Rule matching. v1: simple field-equality predicates on event
// type/subtype + assign_to/citizen, plus a string-templated
// "do" command. Layer 1 defaults (bot_failed, my_review_resolved,
// etc.) come in Phase 4b as compiled-in rules using the same
// matcher.

import (
	"strings"
)

// Rule is one matcher + delivery instruction. Rules come from
// three sources (per docs/notifications.md three-layer model):
//
//   - Layer 1: compiled-in defaults (Phase 4b)
//   - Layer 2: project-shared, loaded from enju/conf.yaml (Phase 4d)
//   - Layer 3: per-user, loaded from ~/.enju/notify.yaml (Phase 4d)
//
// All three flow through Config.Rules; Run doesn't distinguish.
type Rule struct {
	// Name is a stable identifier used in logs and (Phase 4b)
	// for rate-limiting bookkeeping. Layer 1 defaults have
	// reserved names like "assigned_task_ready"; user rules
	// pick their own.
	Name string

	// When is the predicate. All non-empty fields must match
	// the event's corresponding field for the rule to fire.
	// Empty fields are wildcards.
	When Predicate

	// Kind selects the adapter. v1 ships "desktop"; "shell",
	// "slack", "ntfy", "email" come in 4c.
	Kind string

	// Message is a human-readable template — basic {{field}}
	// substitution (4b will harden the template language). For
	// Kind=desktop it becomes the popup body; for Kind=shell
	// it's piped to the command via stdin or env.
	Message string

	// Do is the shell command for Kind=shell rules. Ignored
	// for built-in adapter kinds.
	Do string
}

// Predicate is the When clause of a Rule. All non-empty fields
// AND together; the event must match all of them.
type Predicate struct {
	EventType string // exact match on event.type
	Subtype   string // exact match on event.subtype
	TaskID    string // exact match on event.task_id
	Citizen   string // exact match on event.citizen — supports the literal "{{me}}"
	// Future fields here as Layer 1 default predicates need
	// them (assign_to, project_owner, parent_of, etc.). Keep
	// the predicate flat; nested boolean logic is over-design
	// for v1.
}

// matchRules returns the subset of cfg.Rules that match ev.
// Doesn't include Layer 1 defaults — use matchRulesAgainst
// when defaults need to participate. Kept as a thin convenience
// for callers (mostly tests) that explicitly want user rules
// only.
func matchRules(ev Event, cfg Config) []Rule {
	return matchRulesAgainst(cfg.Rules, ev, cfg)
}

// matchRulesAgainst evaluates an arbitrary rule list against
// an event. Run uses this with the merged (defaults + user)
// list so both layers participate in dispatch.
func matchRulesAgainst(rules []Rule, ev Event, cfg Config) []Rule {
	var matched []Rule
	for _, rule := range rules {
		if predicateMatches(rule.When, ev, cfg) {
			matched = append(matched, rule)
		}
	}
	return matched
}

// predicateMatches returns true iff every non-empty field on p
// matches the event's value. The "{{me}}" sentinel on Citizen
// resolves to cfg.Username so user rules can express
// "events I emitted" without hardcoding their handle.
func predicateMatches(p Predicate, ev Event, cfg Config) bool {
	if p.EventType != "" && p.EventType != ev.Type {
		return false
	}
	if p.Subtype != "" && p.Subtype != ev.Subtype {
		return false
	}
	if p.TaskID != "" && p.TaskID != ev.TaskID {
		return false
	}
	if p.Citizen != "" {
		want := p.Citizen
		if want == "{{me}}" {
			want = cfg.Username
		}
		if want != ev.Citizen {
			return false
		}
	}
	return true
}

// renderTemplate does naive {{field}} substitution against the
// event. Supported tokens:
//
//   {{type}}, {{subtype}}, {{task_id}}, {{citizen}}, {{ts}}
//
// Phase 4b hardens this — proper Go-template-style with safe
// escaping and explicit field allowlist. v1 is intentionally
// minimal so the whole notify package stays tight.
func renderTemplate(tmpl string, ev Event) string {
	if tmpl == "" {
		return ""
	}
	replacements := []struct {
		token, value string
	}{
		{"{{type}}", ev.Type},
		{"{{subtype}}", ev.Subtype},
		{"{{task_id}}", ev.TaskID},
		{"{{citizen}}", ev.Citizen},
		{"{{ts}}", ev.Timestamp.UTC().Format("2006-01-02 15:04:05")},
	}
	out := tmpl
	for _, r := range replacements {
		out = strings.ReplaceAll(out, r.token, r.value)
	}
	return out
}
