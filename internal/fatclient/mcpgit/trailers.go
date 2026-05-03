package mcpgit

// Enju commit trailers. Every task-submit commit produced by the
// fat client carries a structured trailer section identifying the
// task and (for compute tasks) its exit + artifacts + duration.
// Trailers are the machine-parseable signal the coordinator and
// fetch-path scanner rely on to reconcile task state after a
// commit lands — "the commit IS the signal."
//
// Trailer format follows git's standard (`Key: value` lines in a
// trailer-only final paragraph, parsable by `git interpret-trailers`).
// We emit and parse our own because git's CLI isn't always on the
// machine running the fat client, and we need to handle messages
// produced by the shell-reference wrapper (planned for SLURM) too.

import (
	"strconv"
	"strings"
)

// Trailer keys. Kept as constants so the wrapper emitters, the
// coordinator's reconcile parser, and the fetch-path scanner
// agree on the exact strings. Changing any of these is a
// cross-component protocol bump.
const (
	TrailerTaskComplete    = "Enju-Task-Complete"
	TrailerExit        = "Enju-Exit"
	TrailerArtifacts      = "Enju-Artifacts"
	TrailerUntrackedArtifacts = "Enju-Untracked-Artifacts"
	TrailerDurationSeconds   = "Enju-Duration-Seconds"
	// verdict + iter-seq trailers carry per-
	// submission semantics into the commit message itself,
	// so a forensic `git log` over a project's history can
	// reconstruct review outcomes and iteration counters
	// without needing the events.db. Verdict mirrors the
	// review/vote decision string ("approve", "reject",
	// "request_changes", "comment"); IterSeq mirrors the
	// task_claims.iter_seq value for the iteration this
	// commit belongs to.
	TrailerVerdict = "Enju-Verdict"
	TrailerIterSeq = "Enju-Iter-Seq"
)

// EnjuTrailers carries the parsed Enju-* trailer values from a
// commit message. All fields are zero-valued when the trailer is
// absent; callers check `TaskID != ""` to decide whether this
// commit is a task-completion signal at all.
type EnjuTrailers struct {
	// TaskID — always present on a task-completion commit.
	// Example: "3:1:TP53:section_7". Unambiguous task
	// identifier (project:run:task[:instance]).
	TaskID string

	// ExitCode — compute tasks only. Absent ⇒ ExitSet == false.
	// Encoded as `Enju-Exit: 0` / `Enju-Exit: 137`.
	ExitCode int
	ExitSet bool

	// Artifacts — tracked artifact paths actually written
	// by the task AND included in this commit. Comma-
	// separated in the trailer. Non-compute commits may
	// omit or leave empty.
	//
	// The trailer's semantic contract is "what's in this
	// commit" — untracked artifacts (track:false) never
	// appear here. See UntrackedArtifacts below for the
	// parallel trailer that records produced-but-not-
	// committed files.
	Artifacts []string

	// UntrackedArtifacts — artifact paths declared track:false
	// that the wrapper confirmed on disk but deliberately kept
	// out of the commit. Recorded in a parallel trailer
	// `Enju-Untracked-Artifacts:` so the fetch-path scanner
	// can include them when reconciling async task completion
	// — the coordinator's artifact index upserts both kinds,
	// just with different commit_sha semantics (tracked → the
	// actual SHA; untracked → empty).
	//
	// Without this, async tasks' untracked writes never reach
	// the coordinator and downstream tasks reading them stay
	// blocked. Sync tasks were unaffected because the handler
	// POSTs /tasks/:id/result with the union directly.
	UntrackedArtifacts []string

	// DurationSeconds — wall-clock script runtime for compute
	// tasks. Zero when absent. Useful for the fetch-path
	// scanner's "task has been running for …" annotation.
	DurationSeconds int

	// Verdict — review/vote outcome carried into the commit
	// message ("approve", "reject", "request_changes",
	// "comment" for reviews; vote_choice id for votes).
	// . Empty for answer/compute submits.
	Verdict string

	// IterSeq — the task_claims.iter_seq value for the
	// iteration this commit belongs to. . Zero when
	// the commit isn't tied to a phase-6c claim row (vote/
	// review pre-6c paths, or future single-shot scripted
	// commits).
	IterSeq int
}

// RenderEnjuTrailers produces the trailer block that goes at the
// very end of a commit message. Callers splice this onto the
// existing message with a blank line separator. Returns an empty
// string when the trailers struct has no task id — no task, no
// trailers.
//
// Note the ordering: TaskComplete first (what readers scan for),
// Exit/Duration next (compute metadata), Artifacts last (often
// long). Keeping the order stable makes commit messages diff
// cleanly between runs that touched the same task.
func RenderEnjuTrailers(t EnjuTrailers) string {
	if t.TaskID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(TrailerTaskComplete)
	b.WriteString(": ")
	b.WriteString(t.TaskID)
	if t.ExitSet {
		b.WriteString("\n")
		b.WriteString(TrailerExit)
		b.WriteString(": ")
		b.WriteString(strconv.Itoa(t.ExitCode))
	}
	if t.DurationSeconds > 0 {
		b.WriteString("\n")
		b.WriteString(TrailerDurationSeconds)
		b.WriteString(": ")
		b.WriteString(strconv.Itoa(t.DurationSeconds))
	}
	if len(t.Artifacts) > 0 {
		b.WriteString("\n")
		b.WriteString(TrailerArtifacts)
		b.WriteString(": ")
		b.WriteString(strings.Join(t.Artifacts, ", "))
	}
	if len(t.UntrackedArtifacts) > 0 {
		b.WriteString("\n")
		b.WriteString(TrailerUntrackedArtifacts)
		b.WriteString(": ")
		b.WriteString(strings.Join(t.UntrackedArtifacts, ", "))
	}
	if t.Verdict != "" {
		b.WriteString("\n")
		b.WriteString(TrailerVerdict)
		b.WriteString(": ")
		b.WriteString(t.Verdict)
	}
	if t.IterSeq > 0 {
		b.WriteString("\n")
		b.WriteString(TrailerIterSeq)
		b.WriteString(": ")
		b.WriteString(strconv.Itoa(t.IterSeq))
	}
	return b.String()
}

// ParseEnjuTrailers extracts the Enju-* trailer values from a
// commit message. Accepts the full message (subject + body +
// trailers); scans any trailer-shaped line in the final paragraph.
// Lines that aren't `Key: value` shape are ignored, as are
// non-Enju keys — the commit may also carry `Co-Authored-By` and
// `AI-Model` trailers we don't care about here.
//
// Returned `TaskID == ""` means "not a task-completion commit" —
// a scanner should skip and move on. Any parse error on numeric
// fields (Exit, Duration-Seconds) falls back to the zero value
// rather than returning an error; this keeps the scanner
// tolerant of malformed trailers without losing the identifier
// we actually care about.
func ParseEnjuTrailers(msg string) EnjuTrailers {
	var t EnjuTrailers
	// Trailers live in the final paragraph (everything after
	// the last blank line). Split on the last "\n\n" and scan
	// just that tail. This matches git's trailer rules: earlier
	// paragraphs aren't considered trailer zones even if they
	// happen to contain `Key: value` lines.
	msg = strings.TrimRight(msg, "\n")
	tail := msg
	if idx := strings.LastIndex(msg, "\n\n"); idx >= 0 {
		tail = msg[idx+2:]
	}
	for _, line := range strings.Split(tail, "\n") {
		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		switch key {
		case TrailerTaskComplete:
			t.TaskID = val
		case TrailerExit:
			if n, err := strconv.Atoi(val); err == nil {
				t.ExitCode = n
				t.ExitSet = true
			}
		case TrailerDurationSeconds:
			if n, err := strconv.Atoi(val); err == nil {
				t.DurationSeconds = n
			}
		case TrailerArtifacts:
			for _, p := range strings.Split(val, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					t.Artifacts = append(t.Artifacts, p)
				}
			}
		case TrailerUntrackedArtifacts:
			for _, p := range strings.Split(val, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					t.UntrackedArtifacts = append(t.UntrackedArtifacts, p)
				}
			}
		case TrailerVerdict:
			t.Verdict = val
		case TrailerIterSeq:
			if n, err := strconv.Atoi(val); err == nil {
				t.IterSeq = n
			}
		}
	}
	return t
}

