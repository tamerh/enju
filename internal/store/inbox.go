package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// InboxItem is one task waiting on the requesting citizen's
// action. Returned by ListInbox to feed the enju_inbox MCP tool
// and the `enju inbox` CLI.
//
// The shape is "everything the reviewer needs to read the work
// without claiming first": the task's own metadata + the
// upstream submission(s) that fed into it. Without the upstream
// content, the inbox is just a list of names — the reviewer
// claims, reads, decides, and may release if it wasn't ready.
// Inlining the content cuts that loop.
type InboxItem struct {
	TaskID          string               // full ID, e.g. "5:1:review"
	Action          string               // task action: review, vote, answer, etc.
	Prompt          string               // rendered prompt, possibly truncated
	PromptTruncated bool                 // true when Prompt was clipped at inboxPromptCap
	Upstream        []UpstreamSubmission // newest-submission per declared parent
}

// UpstreamSubmission is one parent task's most recent submission.
// Reviewers read this alongside the task's prompt to evaluate the
// work in-place.
//
// Known v1 limitation — Content is empty for compute and vote
// parents. task_submissions.content is populated by submitters
// that pass prose (answer / contribute / review verdict text);
// compute tasks write their output to git artifacts (the
// commit_sha in this struct points at it), and vote tasks pass
// the chosen option in a separate column. Reviewing a
// compute/vote parent therefore still requires claiming the
// reviewer task and pulling from git. A future addition can
// fetch artifact content from git into the inbox payload —
// straightforward once a real workflow needs it.
type UpstreamSubmission struct {
	TaskID    string
	Action    string
	CommitSHA string
	Content   string
}

// inboxPromptCap is the byte ceiling on the rendered prompt
// returned in InboxItem. Big prompts (4-8KB compute scripts,
// long-form essays) bloat the inbox response and the
// fat-client's MCP message; truncating with a suffix lets the
// caller follow up with enju_get_task for the full text. 2KB
// is generous for review prompts (typical: 200-500 bytes) and
// keeps the typical inbox payload under 50KB even with a dozen
// pending items + their upstreams.
const inboxPromptCap = 2048

// ListInbox returns all ready tasks in the given project that
// are assigned to the named user, with each task's upstream
// submissions inlined. Empty username matches nothing —
// callers must resolve the bearer token to a username before
// calling.
//
// SQL note: tasks.assign_to is a JSON-encoded array
// (`["alice","bob"]`); membership uses json_each (JSON1
// extension, available in modernc.org/sqlite).
func (s *Store) ListInbox(projectID int64, username string) ([]InboxItem, error) {
	if username == "" {
		return nil, nil
	}
	// Phase 1: drain the candidate task list into a slice, then
	// close the rows. We can't issue follow-up queries on the
	// same connection while a Rows is open (single-statement
	// limit on the SQLite pool connection); pulling everything
	// into memory first is cheap for inbox sizes (typically
	// single-digit items, capped by ready-task assignment).
	type pending struct {
		taskID, action, prompt, dependsOn string
	}
	var todo []pending
	{
		rows, err := s.db.Query(
			`SELECT t.id, t.action, t.prompt, COALESCE(t.depends_on, '')
			 FROM tasks t
			 JOIN runs r ON t.run_id = r.id
			 WHERE r.project_id = ?
			   AND t.state = 'ready'
			   AND t.assign_to <> ''
			   AND EXISTS (SELECT 1 FROM json_each(t.assign_to) WHERE value = ?)
			 ORDER BY t.created_at`,
			projectID, username,
		)
		if err != nil {
			return nil, fmt.Errorf("listing inbox: %w", err)
		}
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.taskID, &p.action, &p.prompt, &p.dependsOn); err != nil {
				rows.Close()
				return nil, err
			}
			todo = append(todo, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	// Phase 2: enrich each item with upstream submission(s).
	var items []InboxItem
	for _, p := range todo {
		it := InboxItem{TaskID: p.taskID, Action: p.action, Prompt: p.prompt}
		if len(it.Prompt) > inboxPromptCap {
			it.Prompt = it.Prompt[:inboxPromptCap] + "...(truncated)"
			it.PromptTruncated = true
		}
		if p.dependsOn != "" {
			for _, dep := range strings.Split(p.dependsOn, ",") {
				dep = strings.TrimSpace(dep)
				if dep == "" {
					continue
				}
				up, err := loadUpstreamSubmission(s.db, dep)
				if err != nil {
					return nil, fmt.Errorf("upstream %s for %s: %w", dep, it.TaskID, err)
				}
				if up != nil {
					it.Upstream = append(it.Upstream, *up)
				}
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// loadUpstreamSubmission pulls the latest submission for one
// parent task, joining task_submissions through task_claims.
// Returns nil if the upstream has no submissions yet (e.g. it's
// a gate or skipped task) — that's a normal "nothing to inline"
// case, not an error.
func loadUpstreamSubmission(q dbExecQueryer, taskID string) (*UpstreamSubmission, error) {
	var action string
	if err := q.QueryRow(`SELECT action FROM tasks WHERE id = ?`, taskID).Scan(&action); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	up := &UpstreamSubmission{TaskID: taskID, Action: action}
	err := q.QueryRow(
		`SELECT COALESCE(ts.commit_sha, ''), COALESCE(ts.content, '')
		 FROM task_submissions ts
		 JOIN task_claims tc ON ts.claim_id = tc.id
		 WHERE tc.task_id = ?
		 ORDER BY ts.submitted_at DESC LIMIT 1`,
		taskID,
	).Scan(&up.CommitSHA, &up.Content)
	if err == sql.ErrNoRows {
		// Upstream has no submission (gate, skipped, etc.) —
		// return the bare metadata so callers see the parent
		// existed even when there's nothing to read.
		return up, nil
	}
	if err != nil {
		return nil, err
	}
	return up, nil
}
