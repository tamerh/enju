// Package dagcache holds the coordinator's in-memory cache of
// parsed run YAML and the derived DAG, keyed by run ID.
//
// Why a dedicated package: the cache used to live as two raw maps
// on api.Server (s.dags, s.runs) accessed directly from cascade
// handlers. Lifting it out lets the service layer reach the same
// cache without an api dependency, which is what unblocks the
// coord-side ports of invalidate/fail/tally/spawn handlers.
//
// Lifecycle: the coordinator binary constructs one Cache at
// startup and shares it with the api Server and any service
// callers that need parsed DAGs. The cache is restart-safe — it
// rebuilds lazily from the run's stored YAML on first access
// after a process restart, so process restarts don't corrupt
// in-flight cascades.
//
// Concurrency: the previous direct-map version was unsynchronized
// — concurrent claim/submit traffic could race the lazy fill.
// This Cache holds an RWMutex around the maps. The DAG itself
// (returned by GetDAG / read off ParsedRun) still has the same
// race surface for incremental AddNode/AddEdge mutations during
// materialization; that's a known caveat documented on
// MutateDAG, and unchanged from the prior behavior.
package dagcache

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/enju-ai/enju/internal/common/dag"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// Cache is the coordinator's parsed-run cache. Construct via New.
// Methods are safe for concurrent use.
type Cache struct {
	store store.CoordinatorStore

	mu  sync.RWMutex
	dags map[int64]*dag.DAG
	runs map[int64]*enjuYaml.ParsedRun
}

// New returns a fresh empty cache backed by the given store.
// The store handle is used to load + parse YAML on cache misses.
func New(s store.CoordinatorStore) *Cache {
	return &Cache{
		store: s,
		dags: make(map[int64]*dag.DAG),
		runs: make(map[int64]*enjuYaml.ParsedRun),
	}
}

// GetDAG returns the cached DAG for runID, parsing the run's
// stored YAML on first access. Subsequent calls hit the cache.
func (c *Cache) GetDAG(runID int64) (*dag.DAG, error) {
	c.mu.RLock()
	if d, ok := c.dags[runID]; ok {
		c.mu.RUnlock()
		return d, nil
	}
	c.mu.RUnlock()
	parsed, err := c.GetParsedRun(runID)
	if err != nil {
		return nil, err
	}
	return parsed.DAG, nil
}

// GetParsedRun returns the cached ParsedRun for runID, parsing
// the run's stored YAML on first access. Callers that need the
// raw task defs, warnings, or deferred metadata reach for this
// instead of GetDAG.
func (c *Cache) GetParsedRun(runID int64) (*enjuYaml.ParsedRun, error) {
	c.mu.RLock()
	if p, ok := c.runs[runID]; ok {
		c.mu.RUnlock()
		return p, nil
	}
	c.mu.RUnlock()

	run, err := c.store.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("loading run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %d not found", runID)
	}
	// Re-parse with the run's stored params so any `{{param}}`
	// references in `for_each:` resolve to their concrete
	// values. Without this, a YAML like
	//
	//   for_each: {item: "{{items}}"}
	//
	// re-parses with `{{items}}` left as a literal — the
	// expander treats the for_each variable as a single-element
	// list of the literal string and materializes nodes under
	// the wrong keys. The DAG ends up without the per-instance
	// nodes the live state actually uses (e.g. "a:process_docker"),
	// so any cascade walk (invalidate, fail-cascade) that calls
	// d.Descendants("a:process_docker") returns empty and
	// silently fails to propagate.
	//
	// Load-bearing for post-restart recovery: a coord restart
	// wipes the in-memory cache; the next cascade-touching
	// operation lands here on a cache miss. Pre-fix, the user
	// saw "1 task(s) changed state (target)" with zero
	// descendants — the structural DAG mismatch ate the cascade.
	var parsed *enjuYaml.ParsedRun
	var err2 error
	if run.Params != "" {
		var paramValues map[string]any
		if jerr := json.Unmarshal([]byte(run.Params), &paramValues); jerr == nil {
			parsed, err2 = enjuYaml.ParseWithParams([]byte(run.YAMLData), paramValues)
		}
	}
	if parsed == nil {
		// No stored params (legacy runs) or params couldn't
		// unmarshal — fall through to plain Parse. Preserves
		// pre-fix behavior for runs that genuinely don't carry
		// params.
		parsed, err2 = enjuYaml.Parse([]byte(run.YAMLData))
	}
	if err2 != nil {
		return nil, fmt.Errorf("parsing stored YAML for run %d: %w", runID, err2)
	}

	c.mu.Lock()
	// Re-check after acquiring the write lock — a concurrent
	// caller may have populated the entry while we were parsing.
	// Prefer the existing entry to keep object identity stable
	// for callers that already hold a reference.
	if existing, ok := c.runs[runID]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.dags[runID] = parsed.DAG
	c.runs[runID] = parsed
	c.mu.Unlock()
	return parsed, nil
}

// Put inserts a freshly-parsed run into the cache. Used by the
// create_run path so the first cascade access doesn't have to
// reparse the YAML it just built.
func (c *Cache) Put(runID int64, parsed *enjuYaml.ParsedRun) {
	if parsed == nil {
		return
	}
	c.mu.Lock()
	c.dags[runID] = parsed.DAG
	c.runs[runID] = parsed
	c.mu.Unlock()
}

// Invalidate evicts the entry for runID. Called by cascade
// handlers after structural mutations (delete or singleton
// re-open) where surgically updating the cached DAG is harder
// than letting the next access reparse from scratch.
func (c *Cache) Invalidate(runID int64) {
	c.mu.Lock()
	delete(c.dags, runID)
	delete(c.runs, runID)
	c.mu.Unlock()
}

// MutateDAG runs fn against the cached DAG under the cache's
// write lock, ensuring no concurrent reparse races the
// mutation. Used by the materialize fast path that incrementally
// adds nodes/edges instead of evicting.
//
// Caveat: the mutation is on the same *dag.DAG that prior
// readers may already hold a reference to — this is unchanged
// from the pre-extract behavior. Readers that walk the DAG while
// MutateDAG is running observe a partially-updated graph. The
// pragmatic mitigation today is that materialize callers run
// after their upstream commit lands, and concurrent walk-callers
// (run_status, cascade) tolerate "missing edge that's about to
// appear" the same way they tolerate any unobserved-yet write.
// Tightening this requires DAG-internal locking, which is out of
// scope for the cache extraction.
func (c *Cache) MutateDAG(runID int64, fn func(*dag.DAG)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.dags[runID]
	if !ok {
		return fmt.Errorf("dagcache: no cached DAG for run %d", runID)
	}
	fn(d)
	return nil
}
