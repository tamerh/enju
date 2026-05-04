package service

import (
	"log/slog"
	"sync"

	"github.com/enju-ai/enju/internal/coordinator/dagcache"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// Coordinator bundles the cross-cutting state that cascade
// operations need beyond the bare store.CoordinatorStore: the parsed-run
// cache and the per-project auto-triage mutex.
//
// Why a struct rather than free functions: the per-project
// triage mutex MUST be a single shared instance across every
// caller into the service layer — otherwise two concurrent
// requests racing on the same project could both pass
// FindOldestOpenIssue and both spawn a fix task, orphaning
// one. Plain free functions can't share that state.
//
// Construct one Coordinator at coordinator startup and pass
// it to api.Server (today's only transport). Future transports
// must share the same Coordinator instance to inherit the
// per-project triage mutex and parsed-run cache.
//
// Most service functions still take store.CoordinatorStore directly — the
// Coordinator only carries weight for cascade-touching code
// paths (invalidate/fail/tally/spawn). Read-only and simple-
// mutation handlers don't need it.
type Coordinator struct {
	Store  store.CoordinatorStore
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
func NewCoordinator(st store.CoordinatorStore, cache *dagcache.Cache, logger *slog.Logger) *Coordinator {
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
