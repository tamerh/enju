package engine

import "github.com/enju-ai/enju/internal/store"

// StateLabel returns the user-facing label for a task
// state. Used in error messages so citizens see consistent
// vocabulary across the MCP formatter and engine errors.
//
// Internal state → user-facing label:
//   ready      → available
//   pending    → blocked
//   claimed    → in progress
//   running    → in progress
//   accepted   → completed
//   collecting → collecting
//   skipped    → skipped
func StateLabel(state store.TaskState) string {
	switch state {
	case store.TaskReady:
		return "available"
	case store.TaskPending:
		return "blocked"
	case store.TaskClaimed:
		return "in progress"
	case store.TaskAccepted:
		return "completed"
	case store.TaskCollecting:
		return "collecting"
	case store.TaskSkipped:
		return "skipped"
	case store.TaskFailed:
		return "failed"
	case store.TaskParked:
		return "parked"
	}
	return string(state)
}
