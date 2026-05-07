package enjugit

import (
	"strconv"
	"strings"
)

// parsed_trailers.go — Enju commit-trailer schema and parser.
//
// Every task-submit commit produced by the fat client carries a
// structured trailer section identifying the task and (for compute
// tasks) its exit / artifacts / duration. These trailers are the
// machine-parseable signal the coordinator and fetch-path scanner
// rely on to reconcile task state after a commit lands — "the
// commit IS the signal."
//
// Note: this is the *parsed-trailer* shape (read side). For composing
// trailers on outbound commits, see trailers.go's composeCommitMessage
// helpers which live alongside the producing path. The two shapes
// stay separate because reading is value-preserving (parse exact
// integers, structured artifact lists) while composing is
// string-formatting (write the integers back as decimal text in the
// canonical order).

// Trailer keys. Kept as constants so wrapper emitters, the
// coordinator's reconcile parser, and the fetch-path scanner agree
// on the exact strings. Changing any of these is a cross-component
// protocol bump.
const (
	TrailerTaskComplete       = "Enju-Task-Complete"
	TrailerExit               = "Enju-Exit"
	TrailerArtifacts          = "Enju-Artifacts"
	TrailerUntrackedArtifacts = "Enju-Untracked-Artifacts"
	TrailerDurationSeconds    = "Enju-Duration-Seconds"
	// Verdict + iter-seq trailers carry per-submission semantics
	// into the commit message itself, so a forensic `git log` can
	// reconstruct review outcomes and iteration counters without
	// the events.db. Verdict mirrors the review/vote decision
	// string ("approve" / "reject" / "request_changes" /
	// "comment" for reviews; vote_choice id for votes); IterSeq
	// mirrors the task_claims.iter_seq value.
	TrailerVerdict = "Enju-Verdict"
	TrailerIterSeq = "Enju-Iter-Seq"
)

// EnjuTrailers carries the parsed Enju-* trailer values from a
// commit message. All fields are zero-valued when the trailer is
// absent; callers check `TaskID != ""` to decide whether this
// commit is a task-completion signal at all.
type EnjuTrailers struct {
	TaskID string

	ExitCode int
	ExitSet  bool

	Artifacts          []string
	UntrackedArtifacts []string

	DurationSeconds int

	Verdict string
	IterSeq int
}

// RenderEnjuTrailers produces the trailer block that goes at the
// very end of a commit message. Callers splice this onto the
// existing message with a blank line separator. Returns "" when
// there's no TaskID — no task, no trailers.
//
// Ordering: TaskComplete first (the scan-key), Exit/Duration next
// (compute metadata), Artifacts last (often long). Stable so
// commit messages diff cleanly between runs.
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

// ParseEnjuTrailers extracts Enju-* trailer values from a commit
// message. Accepts the full message (subject + body + trailers);
// scans any trailer-shaped line in the final paragraph. Lines that
// aren't `Key: value` shape are ignored, as are non-Enju keys.
//
// Returned `TaskID == ""` means "not a task-completion commit" —
// a scanner should skip and move on. Numeric-parse failures fall
// back to the zero value rather than returning an error: scanner
// tolerance over strict validation, so a malformed trailer doesn't
// drop the task identifier we actually care about.
func ParseEnjuTrailers(msg string) EnjuTrailers {
	var t EnjuTrailers
	// Trailers live in the final paragraph (everything after the
	// last blank line). Split on the last "\n\n" and scan just
	// that tail — git's trailer rules in spirit.
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
