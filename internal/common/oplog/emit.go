package oplog

import (
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

// Emit writes a one-line structured summary of the trace to the
// parent logger AND, when traceFile is non-nil, appends the same
// information to it. The file is per-process (see
// ProcessTraceFilename) so within-process goroutines write
// safely (slog and *os.File both have internal mutexes); no
// cross-process coordination needed because each process owns
// its own file.
//
// Callers should `defer trace.Emit(logger, traceFile)` right
// after Start so every invocation — success OR failure — leaves
// a queryable record.
//
// Without persistent emission, traces on the success path are
// thrown away, leaving "verb returned nil but didn't actually do
// the work" cases impossible to diagnose post-hoc.
//
// Severity rule: info when every step ran cleanly (ok/skipped),
// warn when any step failed (so log filters surface the bad runs
// without manual scanning). Either logger or traceFile may be nil
// — Emit no-ops the missing destination. Both nil = silent.
//
// Structured fields written:
//   - op: trace's Op name
//   - steps: "name1=status1,name2=status2,..." compact line
//   - one field per Ctx() entry (branch, task_id, etc.)
//   - fail_detail: the last failed step's error text, when present
func (t *Trace) Emit(logger *slog.Logger, traceFile *os.File) {
	if t == nil {
		return
	}
	stepsLine := renderStepsLine(t.Steps)
	failDetail := lastFailedDetail(t.Steps)
	keys := sortedKeys(t.Context)
	failed := failDetail != ""

	if logger != nil {
		attrs := []any{"op", t.Op, "steps", stepsLine}
		for _, k := range keys {
			attrs = append(attrs, k, t.Context[k])
		}
		if failed {
			attrs = append(attrs, "fail_detail", failDetail)
			logger.Warn("oplog verb completed", attrs...)
		} else {
			logger.Info("oplog verb completed", attrs...)
		}
	}

	if traceFile != nil {
		level := "INFO"
		if failed {
			level = "WARN"
		}
		var line strings.Builder
		line.WriteString(time.Now().UTC().Format(time.RFC3339))
		line.WriteString(" ")
		line.WriteString(level)
		line.WriteString(" op=")
		line.WriteString(t.Op)
		line.WriteString(" steps=")
		line.WriteString(stepsLine)
		for _, k := range keys {
			line.WriteString(" ")
			line.WriteString(k)
			line.WriteString("=")
			line.WriteString(t.Context[k])
		}
		if failed {
			line.WriteString(" fail_detail=")
			line.WriteString(quoteForLog(failDetail))
		}
		line.WriteString("\n")
		// Best-effort write. A failure here (file rotated or
		// removed) shouldn't crash the verb path — slog stays
		// as the backup destination.
		_, _ = traceFile.WriteString(line.String())
	}
}

func renderStepsLine(steps []Step) string {
	var b strings.Builder
	for i, s := range steps {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(s.Name)
		b.WriteString("=")
		b.WriteString(s.Status)
	}
	return b.String()
}

func lastFailedDetail(steps []Step) string {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Status == StatusFailed {
			return steps[i].Detail
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// quoteForLog wraps a string in double quotes and escapes embedded
// quotes / newlines / backslashes. Inlined (vs strconv.Quote) to
// keep the file-format human-grep-friendly rather than Go-literal-
// aware tooling.
func quoteForLog(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
	)
	return "\"" + r.Replace(s) + "\""
}
