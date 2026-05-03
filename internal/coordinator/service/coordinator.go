package service

import (
	"log/slog"
	"sync"

	"github.com/enju-ai/enju/internal/coordinator/dagcache"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// Coordinator bundles the cross-cutting state that cascade
// operations need beyond the bare *store.Store: the parsed-run
// cache and the per-project auto-triage mutex.
//
// Why a struct rather than free functions: the per-project
// triage mutex MUST be a single shared instance across both the
// REST and MCP transports — otherwise two concurrent calls (one
// REST claim, one MCP invalidate) racing on the same project
// could both pass FindOldestOpenIssue and both spawn a fix task,
// orphaning one. Plain free functions can't share that state.
//
// Construct one Coordinator at coordinator startup, share it
// with both api.Server (so its existing handlers can call
// through) and mcphandlers.Config (so native MCP handlers can
// call the same service path). The two transports thereby
// converge on identical cascade behavior + identical
// concurrency semantics.
//
// Most service functions still take *store.Store directly — the
// Coordinator only carries weight for cascade-touching code
// paths (invalidate/fail/tally/spawn). Read-only and simple-
// mutation handlers don't need it.
type Coordinator struct {
	Store  *store.Store
	Cache  *dagcache.Cache
	Logger *slog.Logger

	// triageMu serializes maybeAutoTriage per project. Closes
	// the bounded race where two concurrent submits both pass
	// the open-issue check, both spawn a fix task, and one
	// becomes an orphan. One entry per active project, keyed
	// by projectID(int64).
	triageMu sync.Map
}

// NewCoordinator constructs the cross-cutting Coordinator state.
// Pass the cache from dagcache.New(store) so api.Server and the
// MCP handlers share the same parsed-run cache.
func NewCoordinator(st *store.Store, cache *dagcache.Cache, logger *slog.Logger) *Coordinator {
	return &Coordinator{
		Store:  st,
		Cache:  cache,
		Logger: logger,
	}
}

// projectTriageMutex returns the per-project mutex used by
// maybeAutoTriage. Idempotent: first caller for a projectID
// allocates, subsequent callers find the same mutex.
func (c *Coordinator) projectTriageMutex(projectID int64) *sync.Mutex {
	if m, ok := c.triageMu.Load(projectID); ok {
		return m.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := c.triageMu.LoadOrStore(projectID, m)
	return actual.(*sync.Mutex)
}
