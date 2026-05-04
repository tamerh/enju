package mcphandlers

// Task-metadata forwarders. The real implementation lives in
// internal/fatclient/service — TaskMeta + FetchTaskMeta +
// UseFatClient are session methods now. The alias + apiClient
// shims here let handlers keep saying `*taskMeta` and
// `c.fc.FetchTaskMeta(...)` without per-call edits while they
// gradually migrate to call service.FatClient directly.

import (
	"context"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// taskMeta is a package-local alias for the service-layer type.
// Handlers use `*taskMeta` everywhere; aliasing (vs a wrapper
// struct) keeps that valid without copy-conversion at the
// boundary.
type taskMeta = service.TaskMeta

// fetchTaskMeta forwards to the service layer.
func (c *apiClient) fetchTaskMeta(ctx context.Context, taskID string) (*taskMeta, error) {
	return c.fc.FetchTaskMeta(ctx, taskID)
}

// useFatClient forwards to the service layer.
func (c *apiClient) useFatClient(meta *taskMeta) bool {
	return c.fc.UseFatClient(meta)
}
