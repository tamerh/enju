package notify

// Rule matching. Simple field-equality predicates on
// event_type / subtype / task_id / citizen. Used by the 9
// compiled-in Layer 1 default rules (defaults.go) when the
// enju_notifications MCP tool filters live.jsonl.

import (
	"strings"
)

// Rule is one matcher + rendered message template. Rules in v1
// are compiled-in Layer 1 defaults — the filter that decides
// "is this event worth showing as a notification." User rules
// (Layer 3 customization) deferred post-launch; the simpler
// "9 hardcoded notification types, opt-out via disable_defaults"
// model is what v1 ships.
type Rule struct {
	// Name is a stable identifier used in logs and for opt-out
	// via disable_defaults in enju/notify.yaml.
	Name string

	// When is the predicate. All non-empty fields must match
	// the event's corresponding field for the rule to fire.
	// Empty fields are wildcards.
	When Predicate

	// Message is the rendered notification text shown to the
	// user. Supports basic {{field}} substitution against the
	// event — see renderTemplate for the supported tokens.
	Message string
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

// PredicateMatches is the exported entry point for
// predicateMatches — used by the enju_notifications MCP tool
// to filter live.jsonl events through the same predicate logic
// the poll loop would have used.
func PredicateMatches(p Predicate, ev Event, cfg Config) bool {
	return predicateMatches(p, ev, cfg)
}

// RenderTemplate is the exported entry point for renderTemplate.
func RenderTemplate(tmpl string, ev Event) string {
	return renderTemplate(tmpl, ev)
}

// matchRulesAgainst evaluates an arbitrary rule list against
// an event. Used by enju_notifications to filter live.jsonl
// through the Layer 1 default rules.
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
// v1 is intentionally minimal — when custom user rules land
// (roadmap item #3) this should grow proper Go-template-style
// rendering with explicit field allowlist.
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
