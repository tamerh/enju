package gitv6

import "testing"

// TestRemoveOrigin pins the idempotent-delete contract:
// remove-when-present, no-op-when-absent. Used by
// project.Clone.SetRemote("") to turn a remote-backed clone
// local-only without callers needing to special-case the
// "already absent" path.
func TestRemoveOrigin(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Sanity: origin is present after clone.
	if _, err := c.repo.Remote("origin"); err != nil {
		t.Fatalf("origin should exist after clone: %v", err)
	}

	// First call removes it.
	if err := c.RemoveOrigin(); err != nil {
		t.Fatalf("RemoveOrigin: %v", err)
	}
	if _, err := c.repo.Remote("origin"); err == nil {
		t.Errorf("origin should be gone after RemoveOrigin, but it's still there")
	}
	if c.remoteURL != "" {
		t.Errorf("c.remoteURL should be cleared, got %q", c.remoteURL)
	}

	// Second call is a no-op — caller doesn't need to know
	// origin's current state.
	if err := c.RemoveOrigin(); err != nil {
		t.Errorf("RemoveOrigin should be idempotent on absent origin, got: %v", err)
	}
}
