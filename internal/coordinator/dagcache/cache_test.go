package dagcache

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/common/dag"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

func ptrOf(p *enjuYaml.ParsedRun) uintptr {
	return uintptr(unsafe.Pointer(p))
}

// minimalRunYAML is a parsable run YAML — just enough that
// enjuYaml.Parse succeeds. The exact tasks don't matter; the
// cache only cares that ParsedRun + DAG come back non-nil.
const minimalRunYAML = "name: test\nversion: 1\ntasks:\n  - id: t1\n    action: answer\n    prompt: hi\n"

func newCacheStore(t *testing.T) *store.Store {
	t.Helper()
	// Use an on-disk SQLite file rather than ":memory:" — the
	// in-memory DSN is per-connection, so concurrent goroutines
	// sharing a *Store land on fresh empty schemas and the test
	// hits "no such table: runs". A temp-dir file gives us one
	// physical DB that all connections see.
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	es, err := store.NewSQLiteEventStore(filepath.Join(dir, "events.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.AttachEventStore(es)
	t.Cleanup(func() {
		s.Close()
		es.Close()
	})
	return s
}

func createRun(t *testing.T, s *store.Store, yaml string) int64 {
	t.Helper()
	now := time.Now()
	createRes, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateProject{Project: store.ProjectRecord{
				Name: "p", CreatedAt: now, UpdatedAt: now,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pid := createRes.ProjectID
	id, _, err := s.CreateRun(&store.RunRecord{
		ProjectID: pid,
		Name:   "r",
		YAMLData: yaml,
		State:   store.RunActive,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGetParsedRunLazyLoadAndCache(t *testing.T) {
	s := newCacheStore(t)
	runID := createRun(t, s, minimalRunYAML)
	c := New(s)

	first, err := c.GetParsedRun(runID)
	if err != nil {
		t.Fatalf("first GetParsedRun: %v", err)
	}
	if first == nil || first.DAG == nil {
		t.Fatal("expected non-nil parsed run + DAG")
	}
	second, err := c.GetParsedRun(runID)
	if err != nil {
		t.Fatalf("second GetParsedRun: %v", err)
	}
	if second != first {
		t.Error("expected cache hit to return same *ParsedRun, got fresh parse")
	}
}

func TestGetDAGMatchesParsedRunDAG(t *testing.T) {
	s := newCacheStore(t)
	runID := createRun(t, s, minimalRunYAML)
	c := New(s)

	d, err := c.GetDAG(runID)
	if err != nil {
		t.Fatalf("GetDAG: %v", err)
	}
	pr, _ := c.GetParsedRun(runID)
	if d != pr.DAG {
		t.Error("GetDAG returned a different *DAG than the cached ParsedRun")
	}
}

func TestInvalidateForcesReparse(t *testing.T) {
	s := newCacheStore(t)
	runID := createRun(t, s, minimalRunYAML)
	c := New(s)

	first, _ := c.GetParsedRun(runID)
	c.Invalidate(runID)
	second, _ := c.GetParsedRun(runID)
	if second == first {
		t.Error("expected fresh ParsedRun after Invalidate, got cached pointer")
	}
}

func TestGetParsedRunMissingRun(t *testing.T) {
	s := newCacheStore(t)
	c := New(s)
	if _, err := c.GetParsedRun(9999); err == nil {
		t.Error("expected error for unknown run")
	}
}

func TestMutateDAGNoEntry(t *testing.T) {
	s := newCacheStore(t)
	c := New(s)
	err := c.MutateDAG(123, func(_ *dag.DAG) {})
	if err == nil {
		t.Error("expected error when mutating uncached run")
	}
}

func TestMutateDAGRunsUnderLock(t *testing.T) {
	s := newCacheStore(t)
	runID := createRun(t, s, minimalRunYAML)
	c := New(s)

	if _, err := c.GetParsedRun(runID); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := c.MutateDAG(runID, func(d *dag.DAG) {
		if d == nil {
			t.Error("expected non-nil DAG")
		}
		called = true
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("expected fn to be invoked")
	}
}

func TestConcurrentLazyLoadStableIdentity(t *testing.T) {
	// Race: many goroutines call GetParsedRun on a cold cache.
	// Property: every caller observes the same *ParsedRun
	// pointer. Without the re-check inside the write lock, two
	// concurrent fillers could install different pointers and
	// later readers would see an unstable identity.
	s := newCacheStore(t)
	runID := createRun(t, s, minimalRunYAML)
	c := New(s)

	const N = 32
	var wg sync.WaitGroup
	results := make([]uintptr, N)
	errs := make([]error, N)
	var fails atomic.Int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pr, err := c.GetParsedRun(runID)
			if err != nil {
				errs[idx] = err
				fails.Add(1)
				return
			}
			results[idx] = ptrOf(pr)
		}(i)
	}
	wg.Wait()
	if fails.Load() > 0 {
		for i, e := range errs {
			if e != nil {
				t.Logf("goroutine %d: %v", i, e)
			}
		}
		t.Fatalf("%d concurrent GetParsedRun calls errored", fails.Load())
	}
	want := results[0]
	for i, got := range results {
		if got != want {
			t.Fatalf("goroutine %d saw a different *ParsedRun pointer (%v vs %v) — re-check inside write lock didn't hold", i, got, want)
		}
	}
}
