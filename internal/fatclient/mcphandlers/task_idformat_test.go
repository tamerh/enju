package mcphandlers

import "testing"

// L6: validTaskIDFormat distinguishes a malformed task_id from a
// well-formed-but-missing one, so enju_get_task can return "invalid
// task_id format" instead of the same generic "not found".
func TestValidTaskIDFormat(t *testing.T) {
	valid := []string{
		"26:1:analyze",
		"1:1:a:review", // task name may itself contain colons
		"100:42:fix_ISSUE_001_abc",
	}
	for _, id := range valid {
		if !validTaskIDFormat(id) {
			t.Errorf("expected %q to be a valid task_id format", id)
		}
	}
	invalid := []string{
		"malformed-id",
		"26:1",          // missing task name
		"26:1:",         // empty task name
		"a:1:x",         // non-numeric project
		"26:b:x",        // non-numeric run
		"",
	}
	for _, id := range invalid {
		if validTaskIDFormat(id) {
			t.Errorf("expected %q to be rejected as malformed", id)
		}
	}
}
