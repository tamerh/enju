// Package inbox is the shared core of the inbox view. It's a
// fat-client projection over the local live.jsonl event stream:
// for each task, the latest event determines the current state,
// and tasks whose latest event is task_ready+me end up in the
// inbox. Each parent's commit_sha + result_dir are embedded in
// the task_ready event itself (snapshotted at cascade time), so
// upstream content renders from git with zero coordinator calls.
//
// Two surfaces consume this package: enju_inbox MCP tool and the
// `enju inbox` CLI subcommand. Each provides a Deps implementation
// (just the git read); projection logic lives here so both
// surfaces stay in lockstep.
//
// Architectural framing: inbox is a special-case rendering of
// the assigned_task_ready notification — same substrate, with
// state-replay (latest-event-wins) layered on top so stale ready
// events from already-completed work don't show up. See
// docs/inbox-and-review.md.
package inbox

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// InboxRow is one ready task surfaced to the assignee, with each
// upstream's most-recent submission inlined for in-place reading.
type InboxRow struct {
	TaskID   string             `json:"task_id"`
	Action   string             `json:"action"`
	Upstream []InboxUpstreamRow `json:"upstream"`
}

// InboxUpstreamRow is one parent task's submission, read from git
// at the recorded commit_sha. Content is empty for compute / vote
// parents (their work is in artifacts or the option column, not a
// result.md the inbox can render) or for skipped parents.
type InboxUpstreamRow struct {
	TaskID    string `json:"task_id"`
	Action    string `json:"action"`
	CommitSHA string `json:"commit_sha"`
	Content   string `json:"content"`
}

// Deps is the dependency surface the inbox builder needs. The
// pure-event-replay design means the only outside thing this
// package touches is git — for parent result.md content. No HTTP,
// no coordinator, no state.db.
type Deps interface {
	// ReadFileAtCommit reads a file from the project clone at
	// the given commit SHA. Returns (data, true) on hit,
	// (nil, false) on miss. Implementations typically wrap
	// workspace.Project.ReadFileAtCommit.
	ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error)
}

// MaxCandidates bounds the rows returned. Distinct task_ids
// passed beyond this point in the backward scan are ignored.
// 200 covers any realistic active-work window.
const MaxCandidates = 200

// inboxEvent is the subset of one live.jsonl line the inbox
// projection reads. Subtype carries the task action; AssignTo
// is the hoisted top-level field; Metadata carries parents.
type inboxEvent struct {
	Type     string          `json:"type"`
	Subtype  string          `json:"subtype"`
	TaskID   string          `json:"task_id"`
	Citizen  string          `json:"citizen"`
	AssignTo string          `json:"assign_to"`
	Metadata json.RawMessage `json:"metadata"`
}

// readyMetadata is the parsed payload of a task_ready event's
// metadata field. Only `parents` is consumed here; assign_to is
// already hoisted to the top-level event field.
type readyMetadata struct {
	Parents []readyParent `json:"parents"`
}

type readyParent struct {
	TaskID    string `json:"task_id"`
	Action    string `json:"action"`
	CommitSHA string `json:"commit_sha"`
	ResultDir string `json:"result_dir"`
}

// BuildInbox replays live.jsonl backward and returns the rows
// for tasks currently in READY state with username in their
// assign_to set. Latest-event-wins per task: if the most recent
// task-scoped state-leaving event for a task precedes its latest
// task_ready+me, the task isn't in the inbox.
//
// For each surviving task, parents are read from the task_ready
// event's own metadata (snapshot at cascade time) and each
// parent's result.md is pulled from git via deps.ReadFileAtCommit.
//
// Event categorization (matters for multi-citizen tasks):
//
//   - **Citizen-scoped** (iteration_started, iteration_completed,
//     task_submitted) describe one citizen's progress. They
//     terminate MY view of the task only if citizen == me — for a
//     co-assignee's claim/submit on a multi-reviewer task, I keep
//     scanning back to find my own task_ready event.
//   - **Task-scoped** (task_completed, task_invalidated, task_failed,
//     task_skipped, task_parked, task_request_changes, branch_merged)
//     describe the task as a whole. They terminate every assignee's
//     view — once the task is done/invalidated/etc, nobody's inbox
//     should show it.
//
// Anything else is ignored (cascade_fired and the like don't change
// the task's effective ready-or-not state from any one user's
// perspective).
func BuildInbox(livePath, username string, deps Deps) ([]InboxRow, error) {
	if username == "" {
		return nil, nil
	}
	decided := map[string]bool{}
	// Runs retired by a coarse run-scoped terminal. enju_terminate_run
	// deliberately emits ONE run_terminated event (with skipped-task
	// counts in metadata) instead of N per-task task_skipped events
	// — see store/apply.go. Without honoring it here, a task that
	// was task_ready+me and then run-terminate-skipped keeps
	// task_ready as its newest task-scoped event and wrongly lingers
	// in the inbox. The scan is newest→oldest, so a run_terminated
	// is always seen before the older task_ready it retires.
	terminatedRuns := map[string]bool{}
	var rows []InboxRow

	err := tailJSONL(livePath, func(line []byte) bool {
		var ev inboxEvent
		if json.Unmarshal(line, &ev) != nil {
			return false
		}
		// Run-scoped terminal: no task_id, so it must be handled
		// before the per-task short-circuit below.
		if ev.Type == "run_terminated" {
			if seq := runSeqFromMetadata(ev.Metadata); seq != "" {
				terminatedRuns[seq] = true
			}
			return false
		}
		if ev.TaskID == "" || decided[ev.TaskID] {
			return false
		}
		switch ev.Type {
		case "task_ready":
			// task_ready events fan out per assignee, so an event
			// for someone else doesn't disqualify us — keep
			// scanning back for our own ready.
			if ev.AssignTo != username {
				return false
			}
			// The task's run was terminated after this ready (we
			// saw run_terminated earlier in the newest-first
			// scan): the task is skipped, not actionable. Decide
			// it so no older event re-adds it.
			if terminatedRuns[runSeqOfTask(ev.TaskID)] {
				decided[ev.TaskID] = true
				return false
			}
			decided[ev.TaskID] = true
			rows = append(rows, buildRow(&ev, deps))
			return len(rows) >= MaxCandidates
		case "iteration_started", "iteration_completed", "task_submitted":
			// Citizen-scoped — only terminates my view if I'm
			// the citizen. Co-assignee progress on a multi-
			// citizen task doesn't hide it from me.
			if ev.Citizen == username {
				decided[ev.TaskID] = true
			}
			return false
		case "task_completed", "task_invalidated", "task_failed",
			"task_skipped", "task_parked", "task_request_changes",
			"branch_merged":
			// Task-scoped terminal — gone from everyone's inbox.
			decided[ev.TaskID] = true
			return false
		}
		// Anything else (cascade_fired, model registrations, etc.)
		// doesn't affect ready/not-ready from a per-user perspective.
		return false
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// runSeqOfTask extracts the run-seq segment from a task ID.
// Task IDs are "{project}:{run}:{taskdef}"; the middle segment
// is the per-project run seq. Returns "" on an unexpected shape
// (then it simply won't match any terminated run — safe).
func runSeqOfTask(taskID string) string {
	parts := strings.Split(taskID, ":")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// runSeqFromMetadata pulls run_seq out of a run_terminated
// event's metadata. json.Number so an integer seq renders as
// "2", not "2" vs 2.0 — matched against the task ID's string
// segment. Returns "" when absent/unparseable.
func runSeqFromMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m struct {
		RunSeq json.Number `json:"run_seq"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m.RunSeq.String()
}

// buildRow turns one task_ready event into an InboxRow with each
// parent's result.md pulled from git. Best-effort: parent fetches
// that miss (no commit, missing result.md, transient git error)
// surface as upstream rows with empty Content rather than failing
// the whole inbox.
func buildRow(ev *inboxEvent, deps Deps) InboxRow {
	row := InboxRow{TaskID: ev.TaskID, Action: ev.Subtype}
	if len(ev.Metadata) == 0 {
		return row
	}
	var meta readyMetadata
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		return row
	}
	for _, p := range meta.Parents {
		up := InboxUpstreamRow{
			TaskID:    p.TaskID,
			Action:    p.Action,
			CommitSHA: p.CommitSHA,
		}
		if p.CommitSHA != "" && p.ResultDir != "" {
			resultPath := filepath.Join(p.ResultDir, "result.md")
			if data, ok, _ := deps.ReadFileAtCommit(p.CommitSHA, resultPath); ok {
				up.Content = string(data)
			}
		}
		row.Upstream = append(row.Upstream, up)
	}
	return row
}

// FormatInbox renders the inbox response as readable plain text.
// One section per task with each upstream submission inlined
// below. Empty inbox renders as a single line so pattern-matching
// consumers can detect it. Both the MCP tool and the CLI render
// via this function so their output stays textually identical.
func FormatInbox(rows []InboxRow) string {
	if len(rows) == 0 {
		return "(no tasks waiting on you)"
	}
	var b strings.Builder
	b.WriteString("Inbox: ")
	b.WriteString(itoa(len(rows)))
	b.WriteString(" task(s) waiting on you.\n")
	for _, r := range rows {
		b.WriteString("\n[")
		b.WriteString(r.TaskID)
		b.WriteString("] ")
		b.WriteString(r.Action)
		b.WriteString("\n")
		if len(r.Upstream) == 0 {
			b.WriteString("  (no upstream submissions)\n")
			continue
		}
		for _, up := range r.Upstream {
			b.WriteString("\n  Upstream [")
			b.WriteString(up.TaskID)
			b.WriteString("] ")
			b.WriteString(up.Action)
			if up.CommitSHA != "" {
				b.WriteString(" (commit ")
				b.WriteString(up.CommitSHA)
				b.WriteString(")")
			}
			b.WriteString(":\n")
			if up.Content == "" {
				b.WriteString("  (no inlined content — likely a compute or vote parent; pull from git via the commit_sha)\n")
				continue
			}
			for _, line := range strings.Split(strings.TrimRight(up.Content, "\n"), "\n") {
				b.WriteString("  ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// itoa renders a small int without dragging in fmt or strconv
// just for the "Inbox: N task(s)" header. Clamp to a reasonable
// upper bound (we cap rows at MaxCandidates anyway).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [16]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
