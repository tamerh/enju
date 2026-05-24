package service

import (
	"path/filepath"
	"testing"
)

// TestReconcileOwners_SingleOwnerReproduces the duplicate-ticker
// contention scenario at the election layer: several processes (here,
// several reconcileOwners on one machine, standing in for the MCP
// servers that pile up across /mcp reconnects) all try to own the
// reconcile for ONE project. Exactly one must win; the rest stand
// down — that is what stops N tickers from all grabbing the project
// write lock.
//
// Each reconcileOwners is a distinct value with its own flock handle,
// which mirrors distinct processes: gofrs/flock's exclusion is per
// open file description, so two handles on the same path contend even
// within one test binary.
func TestReconcileOwners_SingleOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".enju", "locks", "reconcile.lock")

	const servers = 4
	owns := make([]*reconcileOwners, servers)
	wonBy := -1
	winners := 0
	for i := range owns {
		owns[i] = &reconcileOwners{}
		if owns[i].own(lockPath) {
			winners++
			wonBy = i
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one reconcile owner among %d servers, got %d", servers, winners)
	}

	// The winner is sticky: re-electing returns true without handing
	// ownership around.
	if !owns[wonBy].own(lockPath) {
		t.Errorf("owner should keep ownership on re-check")
	}
	// A non-owner still stands down.
	loser := (wonBy + 1) % servers
	if owns[loser].own(lockPath) {
		t.Errorf("non-owner should not acquire while the owner holds the lock")
	}
}

// TestReconcileOwners_TakeoverAfterOwnerExit pins the
// auto-recovery: when the owning process dies, the OS releases its
// flock, so a surviving server can take over reconciliation on its
// next election — no heartbeat, no stale-file sweep. We simulate the
// owner's exit by unlocking its flock (what the kernel does on
// process death).
func TestReconcileOwners_TakeoverAfterOwnerExit(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".enju", "locks", "reconcile.lock")

	owner := &reconcileOwners{}
	standby := &reconcileOwners{}

	if !owner.own(lockPath) {
		t.Fatal("first server should win reconcile ownership")
	}
	if standby.own(lockPath) {
		t.Fatal("second server should stand down while the owner is alive")
	}

	// Owner "exits": release its held flock (the OS does this on
	// process death).
	owner.mu.Lock()
	fl := owner.held[lockPath]
	owner.mu.Unlock()
	if fl == nil {
		t.Fatal("owner should be holding a flock")
	}
	if err := fl.Unlock(); err != nil {
		t.Fatalf("release owner flock: %v", err)
	}

	if !standby.own(lockPath) {
		t.Error("standby should take over reconciliation after the owner exits")
	}
}
