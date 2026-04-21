package engine

import (
	"testing"

	"github.com/enju-ai/enju/internal/store"
)

func TestComputeResultDirSingleton(t *testing.T) {
	got := ComputeResultDirForInstance(3, "analyze", nil)
	want := "enju/runs/3/analyze"
	if got != want {
		t.Errorf("singleton: got %q want %q", got, want)
	}
}

func TestComputeResultDirSingleForEach(t *testing.T) {
	got := ComputeResultDirForInstance(3, "align", map[string]string{"sample": "S1"})
	want := "enju/runs/3/align/sample=S1"
	if got != want {
		t.Errorf("single for_each: got %q want %q", got, want)
	}
}

func TestComputeResultDirNestedForEachAlphaSorted(t *testing.T) {
	// gene + tissue → alphabetical: gene first.
	got := ComputeResultDirForInstance(5, "analyze", map[string]string{
		"tissue": "breast",
		"gene":   "BRCA1",
	})
	want := "enju/runs/5/analyze/gene=BRCA1/tissue=breast"
	if got != want {
		t.Errorf("nested alpha-sort: got %q want %q", got, want)
	}
}

// TestComputeResultDirSlugsUnsafeValues — for_each values with
// filesystem-unsafe characters get slugged the same way the
// instance-key already is, so `sample/A:B` becomes
// `sample=sample_A_B` (no path-separator leaks, no `:` ambiguity).
func TestComputeResultDirSlugsUnsafeValues(t *testing.T) {
	got := ComputeResultDirForInstance(1, "t", map[string]string{"sample": "a/b c"})
	// "/" and " " both become "_"; adjacent runs collapse.
	want := "enju/runs/1/t/sample=a_b_c"
	if got != want {
		t.Errorf("slugging: got %q want %q", got, want)
	}
}

// TestComputeResultDirEmptyParamsMap — an empty-but-non-nil
// params map (e.g. a run with no for_each at all) collapses
// to the singleton layout. No trailing slash, no spurious
// `=` segment.
func TestComputeResultDirEmptyParamsMap(t *testing.T) {
	got := ComputeResultDirForInstance(2, "answer", map[string]string{})
	want := "enju/runs/2/answer"
	if got != want {
		t.Errorf("empty map: got %q want %q", got, want)
	}
}

// TestComputeResultDirFromTaskRecord — the DB-row variant
// must produce the identical path as the instance variant
// given equivalent inputs, so persisting + re-serving
// doesn't drift.
func TestComputeResultDirFromTaskRecord(t *testing.T) {
	t1 := &store.TaskRecord{
		ID:             "7:3:align",
		TaskDefID:      "align",
		InstanceParams: "",
	}
	if got := ComputeResultDir(t1); got != "enju/runs/3/align" {
		t.Errorf("singleton via TaskRecord: got %q", got)
	}

	t2 := &store.TaskRecord{
		ID:             "7:3:sample_S1:align",
		TaskDefID:      "align",
		InstanceParams: `{"sample":"S1"}`,
	}
	if got := ComputeResultDir(t2); got != "enju/runs/3/align/sample=S1" {
		t.Errorf("for_each via TaskRecord: got %q", got)
	}

	t3 := &store.TaskRecord{
		ID:             "7:5:BRCA1_breast:analyze",
		TaskDefID:      "analyze",
		InstanceParams: `{"gene":"BRCA1","tissue":"breast"}`,
	}
	if got := ComputeResultDir(t3); got != "enju/runs/5/analyze/gene=BRCA1/tissue=breast" {
		t.Errorf("nested via TaskRecord: got %q", got)
	}
}

// TestComputeResultDirMalformedInstanceParams — a corrupted
// JSON blob in the DB shouldn't crash or return nothing;
// falls back to the singleton layout so the submit can still
// route somewhere recoverable.
func TestComputeResultDirMalformedInstanceParams(t *testing.T) {
	t1 := &store.TaskRecord{
		ID:             "1:2:weird",
		TaskDefID:      "weird",
		InstanceParams: `{not valid json`,
	}
	got := ComputeResultDir(t1)
	want := "enju/runs/2/weird"
	if got != want {
		t.Errorf("malformed params fallback: got %q want %q", got, want)
	}
}

// TestComputeResultDirUsesVisibleRoot — pins the headline
// pre-launch change: no dot prefix. Separate assertion so a
// future accidental revert is loud.
func TestComputeResultDirUsesVisibleRoot(t *testing.T) {
	got := ComputeResultDirForInstance(1, "t", nil)
	if len(got) < len("enju/") || got[:len("enju/")] != "enju/" {
		t.Errorf("path should start with visible 'enju/', got %q", got)
	}
	if got[0] == '.' {
		t.Errorf("path must not start with dot, got %q", got)
	}
}
