package mcphandlers

// Forwarders to the service-layer reconciliation primitives.
// The substantive bodies — PullBranchWithReconcile,
// ReconcileRunBranch, ReapWrapperFailures, BuildReconcileBody,
// StateDir — live in internal/fatclient/service/reconcile.go.
// Handlers and tests reach them through these short shims so
// call sites stay compact and don't need to import service
// directly for one line.

import (
	"context"

	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// buildReconcileBody forwards to the service layer. Kept as a
// package-local function so existing call sites + tests don't
// need to import service to construct reconcile bodies.
func buildReconcileBody(trailers []workspace.CommitTrailer) map[string]interface{} {
	return service.BuildReconcileBody(trailers)
}

// stateDir forwards to the service layer.
func (c *apiClient) stateDir() string {
	return c.fc.StateDir()
}

// pullBranchWithReconcile forwards to the service layer.
func (c *apiClient) pullBranchWithReconcile(ctx context.Context, proj *workspace.Project, projectID int64, branch string) error {
	return c.fc.PullBranchWithReconcile(ctx, proj, projectID, branch)
}
