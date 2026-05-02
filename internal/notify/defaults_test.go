package notify

import (
	"testing"
)

// TestAssignedTaskReadyDefault pins the 10th compiled-in default:
// `assigned_task_ready` fires when EventType=task_ready AND
// AssignTo matches the current user. The "{{me}}" sentinel on
// the predicate's AssignTo resolves to cfg.Username at match time.
func TestAssignedTaskReadyDefault(t *testing.T) {
	cfg := Config{Username: "tamer"}

	var rule *Rule
	for _, r := range compiledDefaults() {
		if r.Name == "assigned_task_ready" {
			cp := r
			rule = &cp
			break
		}
	}
	if rule == nil {
		t.Fatal("assigned_task_ready not in compiledDefaults()")
	}

	// Positive: task_ready event assigned to me → matches.
	ev := Event{Type: "task_ready", TaskID: "5:1:review", AssignTo: "tamer"}
	if !predicateMatches(rule.When, ev, cfg) {
		t.Errorf("expected match for assigned_to=tamer, got no match")
	}

	// Negative: task_ready event assigned to someone else.
	ev2 := Event{Type: "task_ready", TaskID: "5:1:review", AssignTo: "alice"}
	if predicateMatches(rule.When, ev2, cfg) {
		t.Errorf("expected no match for assigned_to=alice (I am tamer)")
	}

	// Negative: task_ready event with no assignee.
	ev3 := Event{Type: "task_ready", TaskID: "5:1:t"}
	if predicateMatches(rule.When, ev3, cfg) {
		t.Errorf("expected no match for unassigned task_ready")
	}

	// Negative: different event type, even if assigned to me.
	ev4 := Event{Type: "task_completed", TaskID: "5:1:t", AssignTo: "tamer"}
	if predicateMatches(rule.When, ev4, cfg) {
		t.Errorf("expected no match for task_completed (rule scopes to task_ready)")
	}
}

// TestPredicateAssignToLiteral pins that a non-{{me}} literal
// AssignTo on a custom predicate matches exactly. Future custom
// rules might say "assigned to alice"; the matcher needs to
// respect the literal without treating it as a sentinel.
func TestPredicateAssignToLiteral(t *testing.T) {
	cfg := Config{Username: "tamer"}
	p := Predicate{EventType: "task_ready", AssignTo: "alice"}

	if !predicateMatches(p, Event{Type: "task_ready", AssignTo: "alice"}, cfg) {
		t.Error("literal AssignTo=alice should match event with AssignTo=alice")
	}
	if predicateMatches(p, Event{Type: "task_ready", AssignTo: "tamer"}, cfg) {
		t.Error("literal AssignTo=alice should NOT match event with AssignTo=tamer")
	}
}

// TestCompiledDefaultsCount pins the size — adding a default
// without updating documentation in defaults.go's package comment
// is a known footgun; this counts as a tripwire.
func TestCompiledDefaultsCount(t *testing.T) {
	got := len(compiledDefaults())
	want := 10
	if got != want {
		t.Errorf("compiledDefaults() length = %d, want %d (update defaults.go header comment if intentional)", got, want)
	}
}
