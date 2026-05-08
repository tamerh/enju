// Package oplog provides structured per-step traces for multi-step
// operations and a per-project append-only file sink that hosts
// them.
//
// The shape Enju keeps reaching for:
//
//   - A verb does N steps in sequence, with fallbacks. When something
//     goes wrong, "which step failed?" is the load-bearing question.
//   - The trace shape is uniform across verbs: a list of (name, status,
//     detail) records with verb-level context.
//   - Diagnostic output goes to slog (live tail) AND a per-project
//     file (audit trail). The file is gitignored, opened append-only,
//     safe under concurrent writers.
//
// Two consumers today:
//
//   - internal/fatclient/enjugit — git-verb traces (SubmitTaskResult,
//     MergeAcceptedTopic, EnsureRunBranch, ...). Each verb opens a
//     trace at entry, records steps as it runs, and `defer trace.Emit`s
//     once. On failure it returns *OpError carrying the trace; on
//     success it just emits.
//   - internal/fatclient/notify — uses OpenProjectLogFile to materialize
//     the events log dir + .gitignore. Doesn't use Trace; events have
//     a separate JSONL shape.
//
// The package boundary captures the genuinely shared bit (file sink
// + structured trace shape) without forcing every consumer to adopt
// the trace concept.
package oplog
