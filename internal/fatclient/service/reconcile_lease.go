package service

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/gofrs/flock"
)

// reconcileOwners elects, per project, the single process that runs
// the periodic reconcile (git fetch/merge). It holds a non-blocking
// flock on the project's reconcile.lock for as long as this process
// owns reconciliation; the OS releases the flock when the process
// exits, so a dead owner's lease frees automatically and another
// server can take over — no heartbeat timestamps, no stale-file
// sweeping.
//
// This is the fix for duplicate MCP servers (piled up across /mcp
// reconnects) each running their own reconcile ticker and contending
// on the project write lock: only the flock holder reconciles; the
// rest stand their tickers down.
type reconcileOwners struct {
	mu   sync.Mutex
	held map[string]*flock.Flock // reconcile-lock path -> held lock
}

// own reports whether this process owns reconciliation for the
// project whose reconcile lock is at lockPath. It returns true when
// we already hold the lock or can acquire it now (non-blocking), and
// false when another live process holds it. Once acquired, the lock
// is kept for the process lifetime — re-electing on every tick would
// just hand ownership back and forth.
func (o *reconcileOwners) own(lockPath string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.held == nil {
		o.held = make(map[string]*flock.Flock)
	}
	if _, ok := o.held[lockPath]; ok {
		return true // already ours
	}
	// gofrs/flock won't create the parent dir; the project's
	// .enju/locks/ usually exists (project.lock lives there) but
	// MkdirAll keeps a first-reconcile-before-first-commit safe.
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return false
	}
	fl := flock.New(lockPath)
	ok, err := fl.TryLock()
	if err != nil || !ok {
		return false // another live process owns reconciliation
	}
	o.held[lockPath] = fl
	return true
}

// OwnsReconcile reports whether this fat client should run the
// periodic reconcile for projectID. Exactly one process per project
// (per machine) returns true; the others stand their reconcile
// tickers down, eliminating the needless project-lock contention
// that duplicate MCP servers otherwise create.
//
// Fail-open: if there's no registry or the project's path can't be
// resolved, it returns true rather than silently never reconciling —
// the single-process case (no contention to avoid) keeps its old
// behavior.
func (s *FatClient) OwnsReconcile(projectID int64) bool {
	if s.projectRegistry == nil {
		return true
	}
	entry, err := s.projectRegistry.Get(projectID)
	if err != nil || entry == nil || entry.LocalPath == "" {
		return true
	}
	return s.reconcileOwn.own(enjugit.ReconcileLockPathFor(entry.LocalPath))
}
