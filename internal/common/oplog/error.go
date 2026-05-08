package oplog

import (
	"sort"
	"strings"
)

// OpError is the structured failure type produced by Trace.Failed
// and Trace.WrapTerminal. It wraps the typed sentinel cause so
// `errors.Is(err, sentinel)` keeps working for caller routing,
// and carries the step trace so failure messages pinpoint where
// the operation broke.
//
// Verb-specific operands (branch names, task IDs, run seqs) live
// in Context (via Trace.Ctx). The Error() string renders Context
// in sorted order for stable diffs across log/test output.
type OpError struct {
	Op      string
	Cause   error
	Steps   []Step
	Context map[string]string
}

func (e *OpError) Error() string {
	var b strings.Builder
	b.WriteString(e.Op)
	b.WriteString(" failed")
	if len(e.Context) > 0 {
		keys := make([]string, 0, len(e.Context))
		for k := range e.Context {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" (")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(e.Context[k])
		}
		b.WriteString(")")
	}
	for _, s := range e.Steps {
		b.WriteString("\n  ")
		b.WriteString(s.Name)
		b.WriteString(": ")
		b.WriteString(s.Status)
		if s.Detail != "" {
			b.WriteString(" — ")
			b.WriteString(s.Detail)
		}
	}
	return b.String()
}

// Unwrap exposes the typed Cause for `errors.Is` / `errors.As`
// routing.
func (e *OpError) Unwrap() error { return e.Cause }
