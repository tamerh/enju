package enjugit

import (
	"fmt"
	"strings"
)

// composeCommitMessage builds a commit message in canonical
// Enju shape:
//
//	<subject>
//
//	<optional body>
//
//	Enju-Task-ID: 7:1:dev_a
//	Enju-Iter-Seq: 2
//	Enju-Verdict: approve
//	AI-Model: claude-3.5-sonnet
//	Co-Authored-By: Claude <noreply@anthropic.com>
//
// Trailer order is Conventions.TrailerOrder. Unknown trailer
// names (caller-provided custom ones) append after the canonical
// list in iteration order.
//
// Caller passes raw values; this helper concatenates and skips
// empty values (no "AI-Model: " line when ModelName is "").
func composeCommitMessage(convs Conventions, subject, body string, trailers map[string]string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(subject))
	b.WriteString("\n")
	if body = strings.TrimSpace(body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	if len(trailers) > 0 {
		b.WriteString("\n")
		emitted := make(map[string]bool, len(trailers))
		// Canonical order first.
		for _, name := range convs.TrailerOrder {
			if val, ok := trailers[name]; ok && val != "" {
				fmt.Fprintf(&b, "%s: %s\n", name, val)
				emitted[name] = true
			}
		}
		// Anything not in the canonical list, in caller-provided
		// order (we iterate the map; Go maps are unordered, so
		// for true determinism use only canonical names).
		for name, val := range trailers {
			if emitted[name] || val == "" {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", name, val)
		}
	}
	return b.String()
}

// aiCoAuthorTrailer returns the "Co-Authored-By: <model>"
// trailer value for AI-attributed commits. Returns "" when
// modelName is empty so composeCommitMessage skips the line.
func aiCoAuthorTrailer(modelName string) string {
	if modelName == "" {
		return ""
	}
	// Map the model name to a canonical Co-Authored-By string.
	// We use the same shape Anthropic suggests for Claude commits,
	// preserving the model identity in parens — without it,
	// every Claude variant collapses to a single "Claude" line in
	// contributor graphs and audit timelines, losing per-model
	// attribution that downstream review tooling relies on.
	// Other providers follow the same "Vendor (model) <addr>"
	// pattern so the trailer reads consistently across vendors.
	switch {
	case strings.HasPrefix(modelName, "claude"):
		return "Claude (" + modelName + ") <noreply@anthropic.com>"
	case strings.HasPrefix(modelName, "gpt"):
		return "OpenAI (" + modelName + ") <noreply@openai.com>"
	default:
		return modelName + " <noreply@unknown>"
	}
}

// buildSubmitTrailers composes the trailer map for a
// SubmitTaskResult commit. Uses the canonical TrailerXxx
// constants (defined in parsed_trailers.go) so the shape the
// SCANNER expects exactly matches what the WRITER produces —
// the round-trip protocol stays one-source-of-truth.
//
// Scanner-readable keys (matched in ParseEnjuTrailers):
//   - Enju-Task-Complete  (the scan key)
//   - Enju-Iter-Seq
//   - Enju-Verdict
//
// Plus AI attribution (matches existing fat-client convention):
//   - AI-Model
//   - Co-Authored-By (Claude/GPT/etc. per model)
//
// Custom trailers (e.g. Enju-Untracked-Artifacts the scanner
// also reads) flow through req.CustomTrailers so the verb
// stays generic.
func buildSubmitTrailers(req SubmitRequest) map[string]string {
	tr := map[string]string{}
	if req.TaskID != "" {
		tr[TrailerTaskComplete] = req.TaskID
	}
	if req.IterSeq > 0 {
		tr[TrailerIterSeq] = fmt.Sprintf("%d", req.IterSeq)
	}
	if req.Verdict != "" {
		tr[TrailerVerdict] = req.Verdict
	}
	if len(req.ArtifactPaths) > 0 {
		// Enju-Artifacts carries the tracked-artifact paths the
		// scanner reconciles against the artifact index. Tracked
		// ⇒ committed in this commit's tree; untracked paths flow
		// via Enju-Untracked-Artifacts (caller's
		// CustomTrailers["Enju-Untracked-Artifacts"]).
		tr[TrailerArtifacts] = strings.Join(req.ArtifactPaths, ", ")
	}
	if req.ModelName != "" {
		tr["AI-Model"] = req.ModelName
		if co := aiCoAuthorTrailer(req.ModelName); co != "" {
			tr["Co-Authored-By"] = co
		}
	}
	for k, v := range req.CustomTrailers {
		tr[k] = v
	}
	return tr
}

// buildMergeTrailers composes the trailer map for an auto-merge
// commit per spec.
func buildMergeTrailers(author MergeAuthor) map[string]string {
	tr := map[string]string{}
	if author.AutoOrManual != "" {
		tr["Enju-Merge"] = author.AutoOrManual
	}
	if author.TaskID != "" {
		tr["Enju-Triggered-By"] = author.TaskID
	}
	return tr
}

// buildTemplateSnapshotTrailers composes the trailer map for a
// CommitTemplateBundle commit.
func buildTemplateSnapshotTrailers(runSeq int) map[string]string {
	return map[string]string{
		"Enju-Template-Snapshot": fmt.Sprintf("%d", runSeq),
	}
}
