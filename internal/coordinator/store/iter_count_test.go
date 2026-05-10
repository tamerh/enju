package store

// Phase 8.6 — pin that ListTaskIterations now surfaces the
// per-claim iter_seq value so callers (ToTaskResponse) can
// compute iter_count = COUNT(DISTINCT iter_seq) without a
// second query. Multi-citizen tasks have N claim rows per
// accept-cycle, so a naive len(iters) would overcount; the
// distinct-iter_seq projection is the correct shape.

import (
	"testing"
	"time"
)

// TestListTaskIterations_SurfacesIterSeq checks the column
// is read out of task_claims, not silently dropped to zero.
// One claim with iter_seq=3 (set directly so we don't depend
// on the apply-time stamping rules) → IterSeq==3 in the
// projection.
func TestListTaskIterations_SurfacesIterSeq(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-itseq")
	now := time.Now()

	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:dev", RunID: runID, Seq: 1, TaskDefID: "dev",
		Action: "answer", ResultType: "text", State: TaskReady,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := helperClaimTask(s, "1:1:dev", alice, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Stamp iter_seq directly — applySetClaim's auto-stamp
	// logic uses MAX+1, so for a fresh task it's 1; we want
	// to verify the projection surfaces whatever value the
	// column carries, including non-default values.
	if _, err := s.db.Exec(`UPDATE task_claims SET iter_seq = 3 WHERE task_id = ?`, "1:1:dev"); err != nil {
		t.Fatal(err)
	}

	iters, err := s.ListTaskIterations("1:1:dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(iters) != 1 {
		t.Fatalf("len = %d, want 1", len(iters))
	}
	if iters[0].IterSeq != 3 {
		t.Errorf("IterSeq = %d, want 3 (column is being silently dropped)", iters[0].IterSeq)
	}
}

// TestListTaskIterations_DistinctIterSeqAcrossMultiCitizen
// pins the multi-citizen case: three citizens each claim the
// SAME iter_seq=1 cycle. ListTaskIterations returns three
// rows (one per claim) but COUNT(DISTINCT IterSeq) is 1 —
// so iter_count should be 1, not 3. Callers (ToTaskResponse)
// gate iterations[] on iter_count > 1 so a fresh
// multi-citizen task without a re-iteration cycle shouldn't
// surface a noisy "Iterations (3×)" block.
func TestListTaskIterations_DistinctIterSeqAcrossMultiCitizen(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	now := time.Now()
	alice := createTestCitizen(t, s, "alice", "tok-mcr-a")
	bob := createTestCitizen(t, s, "bob", "tok-mcr-b")
	carol := createTestCitizen(t, s, "carol", "tok-mcr-c")

	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:vote", RunID: runID, Seq: 1, TaskDefID: "vote",
		Action: "vote", ResultType: "text", State: TaskReady,
		Citizens: 3, MinQuorum: 3,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []int64{alice, bob, carol} {
		if err := helperClaimTask(s, "1:1:vote", c, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// All three claim rows should be at iter_seq=1.
	iters, err := s.ListTaskIterations("1:1:vote")
	if err != nil {
		t.Fatal(err)
	}
	if len(iters) != 3 {
		t.Fatalf("expected 3 claim rows, got %d", len(iters))
	}
	distinct := map[int]bool{}
	for _, it := range iters {
		distinct[it.IterSeq] = true
	}
	if len(distinct) != 1 {
		t.Errorf("expected 1 distinct iter_seq across multi-citizen claims, got %d (rows: %+v)",
			len(distinct), iters)
	}
}
