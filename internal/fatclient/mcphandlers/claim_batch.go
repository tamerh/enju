package mcphandlers

// Bulk-claim-by-filter handler. Orchestration moved to
// internal/fatclient/service/claim_batch.go — this file is
// now: parse args → call service → render the result via
// the formatters that still live here.
//
// The formatters consume service.BatchClaimEntry directly so
// the handler doesn't have to copy or convert.

import (
	"context"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	defaultClaimSelectorLimit = 50
	maxClaimSelectorLimit     = 500
)

func (c *apiClient) handleClaimReadyMatching(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	actionFilter := strings.TrimSpace(req.GetString("action", ""))
	includeContext := req.GetBool("include_context", false)
	limit := req.GetInt("limit", defaultClaimSelectorLimit)
	if limit <= 0 {
		limit = defaultClaimSelectorLimit
	}
	if limit > maxClaimSelectorLimit {
		return mcp.NewToolResultError(fmt.Sprintf(
			"limit %d exceeds hard cap %d — lower it, or split the bulk claim across calls",
			limit, maxClaimSelectorLimit)), nil
	}

	result, err := c.session.ClaimReadyMatching(ctx, service.ClaimMatchingParams{
		ProjectID:      int64(projectID),
		RunID:          int64(runID),
		ActionFilter:   actionFilter,
		IncludeContext: includeContext,
		Limit:          limit,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatClaimMatchingSummary(result.Entries, result.CandidatesScanned, result.ActionFilter)), nil
}

// formatClaimMatchingSummary renders the whole selector
// response: a header with N claimed / M skipped / E errored
// + optional action-filter echo, followed by per-entry
// lines. Humans read the header, agents parse the entries.
func formatClaimMatchingSummary(results []service.BatchClaimEntry, candidatesScanned int, actionFilter string) string {
	var b strings.Builder
	var claimed, skipped, errored int
	for _, r := range results {
		switch r.Status {
		case "claimed":
			claimed++
		case "skipped":
			skipped++
		default:
			errored++
		}
	}
	if len(results) == 0 {
		if actionFilter != "" {
			b.WriteString(fmt.Sprintf("No ready tasks match action=%q (scanned %d ready task(s))\n", actionFilter, candidatesScanned))
		} else {
			b.WriteString(fmt.Sprintf("No ready tasks to claim (scanned %d)\n", candidatesScanned))
		}
		return b.String()
	}
	if errored == 0 && skipped == 0 {
		b.WriteString(fmt.Sprintf("✓ Claimed %d task(s)\n\n", claimed))
	} else {
		b.WriteString(fmt.Sprintf("Claim summary: %d claimed, %d skipped, %d errored\n\n", claimed, skipped, errored))
	}
	for _, r := range results {
		writeClaimEntryLine(&b, r)
	}
	return b.String()
}

// writeClaimEntryLine renders one row of the selector's
// per-entry section. Split out of formatClaimMatchingSummary
// because the claim row has three rendering shapes —
// single-line lean, multi-line full-context block, and
// reason-only (skipped/errored) — and inlining the picker
// made the loop hard to follow.
func writeClaimEntryLine(b *strings.Builder, r service.BatchClaimEntry) {
	b.WriteString(claimStatusPrefix(r.Status))
	b.WriteString(" ")
	b.WriteString(r.TaskID)

	if r.Status == "claimed" && r.Summary != "" {
		if isSingleLineClaimSummary(r.Summary) {
			// Lean response: everything fits on the header
			// line. Append in parentheses so the task id
			// stays visually anchored.
			b.WriteString("  (" + r.Summary + ")\n")
			return
		}
		// include_context=true response: the summary is a
		// full format.ClaimResult render. Put it on its own
		// indented block underneath the header row.
		b.WriteString("\n")
		for _, line := range strings.Split(r.Summary, "\n") {
			if line != "" {
				b.WriteString("    " + line + "\n")
			}
		}
		return
	}

	if r.Reason != "" {
		b.WriteString("  — " + r.Reason)
	}
	b.WriteString("\n")
}

// claimStatusPrefix picks the leading glyph for a
// per-entry line. ✓ = claimed this call; — = skipped
// (already claimed by us, race, terminal); ✗ = error.
func claimStatusPrefix(status string) string {
	switch status {
	case "claimed":
		return "✓"
	case "skipped":
		return "—"
	case "error":
		return "✗"
	}
	return "?"
}

// isSingleLineClaimSummary reports whether a summary string
// is the lean one-line form (no embedded newlines). Used to
// pick the inline-append vs indented-block render path.
func isSingleLineClaimSummary(s string) bool {
	return !strings.ContainsRune(s, '\n')
}
