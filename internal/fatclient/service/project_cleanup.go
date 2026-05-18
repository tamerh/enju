package service

// project_cleanup.go — on-demand cleanup of merged run and iter
// branches, exposed as the service layer behind the
// enju_project_sync cleanup param.

import (
	"context"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// CleanupRunBranches fetches the project's terminal runs from the
// coordinator, opens the local workflow, and delegates to
// Workflow.CleanupRunBranches. Returns (nil, nil) when mode is
// "none" or the project has no local workspace (remote-only setup).
//
// Errors from individual branches are non-fatal and collected in
// BranchCleanupResult.Errors; a returned error means a structural
// prerequisite failed (coord fetch, workspace open, baseBranch
// resolution).
func (s *FatClient) CleanupRunBranches(ctx context.Context, projectID int64, mode enjugit.CleanupMode) (*enjugit.BranchCleanupResult, error) {
	if mode == enjugit.CleanupModeNone {
		return nil, nil
	}
	if s.enjugit == nil {
		return nil, fmt.Errorf("branch cleanup is only available in MCP client mode")
	}

	runs, err := s.ListRuns(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("cleanup: list runs: %w", err)
	}

	wf, _, _, _, err := s.OpenWorkflow(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("cleanup: open workflow: %w", err)
	}

	return wf.CleanupRunBranches(runs, mode)
}
