package service

import (
	"context"
	"testing"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
)

// TestResolveAutoBranch pins the shared <slug>-N allocator that
// Finding F extracted out of mcphandlers so the MCP create_run
// handler and `enju go` share one implementation. Uses the
// views_test fakeCoord/newViewClient harness: the FatClient has a
// real coord pointed at canned JSON but NO workspace, so
// OpenWorkflow errors and the allocator falls back to the coord
// run list alone (the graceful-degradation path is itself under
// test here). The invariant: a branch already present in the
// coord's run list must be skipped, and bad YAML must fail before
// any coord/git work.
func TestResolveAutoBranch(t *testing.T) {
	const yaml = "name: Showcase Auto\nversion: 1\ntasks:\n  - id: t\n    action: compute\n    assign_to: tamer\n    script: t.sh\n    prompt: \"x\"\n"
	slug := corelayout.ComputeRunSlug("", "Showcase Auto")
	if slug == "" {
		t.Fatal("precondition: ComputeRunSlug returned empty for a named workflow")
	}

	// Coord already has <slug>-1; the allocator must skip it.
	srv := fakeCoord(t, map[string]any{
		"/api/v1/projects/7/runs": []map[string]any{
			{
				"id":         int64(1),
				"project_id": int64(7),
				"seq":        1,
				"branch":     slug + "-1",
				"state":      "completed",
				"created_at": "2026-05-01T10:00:00Z",
			},
		},
	})
	defer srv.Close()
	fc := newViewClient(t, srv.URL)

	got, err := fc.ResolveAutoBranch(context.Background(), 7, yaml, "")
	if err != nil {
		t.Fatalf("ResolveAutoBranch: %v", err)
	}
	if want := slug + "-2"; got != want {
		t.Fatalf("got %q, want %q (must skip the coord-used %s-1)", got, want, slug)
	}

	// Invalid YAML → error before any coord/workspace work.
	if _, err := fc.ResolveAutoBranch(context.Background(), 7, "\tnot: [valid", ""); err == nil {
		t.Fatal("invalid YAML must produce an error")
	}
}
