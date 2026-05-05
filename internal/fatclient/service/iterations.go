package service

// ListTaskIterations — fat-client view of one task's iteration
// history. Wraps GET /api/v1/tasks/{taskID}/iterations and
// decodes into the shared wire.Iteration shape. Used by the web
// UI's task-history panel: every claim attempt + verdict +
// commit, reverse-chronological so the user re-claiming after
// request_changes can see what previously got submitted and how
// each attempt was judged.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/enju-ai/enju/internal/common/wire"
)

// ListTaskIterations returns the iteration rows for one task,
// newest-first as the coordinator orders them. Empty slice when
// no iterations exist (task hasn't been claimed yet) — not an
// error.
//
// Each row carries the iteration's commit_sha; pair with
// ReadResultAtCommit (using the task's ResultDir from
// FetchTaskMeta) to read the prose body that iteration
// committed.
func (s *FatClient) ListTaskIterations(ctx context.Context, taskID string) ([]wire.Iteration, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}
	data, err := s.coord.Get(ctx, "/api/v1/tasks/"+taskID+"/iterations")
	if err != nil {
		return nil, err
	}
	var out []wire.Iteration
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode iterations: %w", err)
	}
	return out, nil
}
