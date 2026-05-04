package mcphandlers

// Artifact-read handlers. Writes happen through the submit
// path (artifacts_json on enju_submit_result); these three are
// the read-only surface: list every artifact in a project
// (optionally filtered by prefix), read one by path at the
// coordinator's current commit pointer, or walk its write
// history via git log. Workspace-touching bodies live in
// internal/fatclient/service/artifact_ops.go.

import (
	"context"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleListArtifacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path := fmt.Sprintf("/api/v1/projects/%d/artifacts", projectID)
	if prefix := req.GetString("prefix", ""); prefix != "" {
		path += "?prefix=" + prefix
	}
	data, err := c.get(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(format.ArtifactList(data, int64(projectID))), nil
}

// handleGetArtifact reads an artifact's current content from the
// client's local clone. The coordinator provides the provenance
// metadata (via its artifact index), the client reads the actual
// bytes.
func (c *apiClient) handleGetArtifact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	out, err := c.fc.GetArtifactContent(ctx, int64(projectID), path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(format.ArtifactDetail(out)), nil
}

// handleGetArtifactHistory walks the local clone's git log for a
// specific file, then enriches each commit with current-pointer
// and invalidation status by cross-referencing the coordinator's
// artifact index and the task state machine.
func (c *apiClient) handleGetArtifactHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	out, err := c.fc.GetArtifactHistory(ctx, int64(projectID), path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(format.ArtifactHistory(out)), nil
}

// handleListUntrackedArtifacts filters the coordinator's artifact
// index down to entries with tracked=false and reports their
// local-workspace visibility. Intended as a debugging aid for
// "cannot claim — task reads untracked artifact(s) not in your
// workspace" errors, and as a quick audit of which outputs this
// project keeps out of git.
//
// For each untracked entry, the service layer runs
// EnsureSharedSymlink (best-effort, materializes the link if
// $ENJU_SHARED_ROOT is configured and the workspace path isn't
// a live symlink yet) before stat'ing — so calling this tool
// can fix downstream claim errors in-place when shared storage
// is available.
func (c *apiClient) handleListUntrackedArtifacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	branch := req.GetString("branch", "")
	report, err := c.fc.ListUntrackedArtifacts(ctx, int64(projectID), branch)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Untracked artifacts in project %d", projectID)
	if report.ResolvedBranch != "" {
		fmt.Fprintf(&b, " (branch: %s)", report.ResolvedBranch)
	}
	b.WriteString("\n\n")
	if len(report.Rows) == 0 {
		b.WriteString("(none — no artifacts flagged track:false in this project)\n")
		return mcp.NewToolResultText(b.String()), nil
	}
	for _, u := range report.Rows {
		marker := "?"
		switch u.LocalState {
		case "present":
			marker = "✓"
		case "missing":
			marker = "✗"
		}
		fmt.Fprintf(&b, "%s %s\n", marker, u.Path)
		if u.Producer != "" {
			fmt.Fprintf(&b, "   Producer: %s\n", u.Producer)
		}
		fmt.Fprintf(&b, "   Local: %s", u.LocalState)
		if u.Target != "" {
			fmt.Fprintf(&b, " (symlink → %s)", u.Target)
		}
		b.WriteByte('\n')
		b.WriteByte('\n')
	}
	if report.SharedRoot == "" {
		b.WriteString("(ENJU_SHARED_ROOT not configured — missing entries can be fixed by re-running the producer task locally, or by pointing $ENJU_SHARED_ROOT at a mount the producer wrote to.)\n")
	} else {
		fmt.Fprintf(&b, "(ENJU_SHARED_ROOT=%s — missing entries mean the producer never wrote to this mount, or the mount is unavailable.)\n", report.SharedRoot)
	}
	return mcp.NewToolResultText(b.String()), nil
}
