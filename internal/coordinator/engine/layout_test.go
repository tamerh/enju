package engine

import (
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

func TestComputeResultDirSingleton(t *testing.T) {
	got := ComputeResultDirForInstance(3, "gwas", "analyze", nil)
	want := "enju/runs/3-gwas/analyze"
	if got != want {
		t.Errorf("singleton: got %q want %q", got, want)
	}
}

func TestComputeResultDirSingleForEach(t *testing.T) {
	got := ComputeResultDirForInstance(3, "gwas", "align", map[string]string{"sample": "S1"})
	want := "enju/runs/3-gwas/align/sample=S1"
	if got != want {
		t.Errorf("single for_each: got %q want %q", got, want)
	}
}

func TestComputeResultDirNestedForEachAlphaSorted(t *testing.T) {
	// gene + tissue → alphabetical: gene first.
	got := ComputeResultDirForInstance(5, "gwas", "analyze", map[string]string{
		"tissue": "breast",
		"gene":   "BRCA1",
	})
	want := "enju/runs/5-gwas/analyze/gene=BRCA1/tissue=breast"
	if got != want {
		t.Errorf("nested alpha-sort: got %q want %q", got, want)
	}
}

// TestComputeResultDirSlugsUnsafeValues — for_each values with
// filesystem-unsafe characters get slugged the same way the
// instance-key already is, so `sample/A:B` becomes
// `sample=sample_A_B` (no path-separator leaks, no `:` ambiguity).
func TestComputeResultDirSlugsUnsafeValues(t *testing.T) {
	got := ComputeResultDirForInstance(1, "gwas", "t", map[string]string{"sample": "a/b c"})
	// "/" and " " both become "_"; adjacent runs collapse.
	want := "enju/runs/1-gwas/t/sample=a_b_c"
	if got != want {
		t.Errorf("slugging: got %q want %q", got, want)
	}
}

// TestComputeResultDirEmptyParamsMap — an empty-but-non-nil
// params map (e.g. a run with no for_each at all) collapses
// to the singleton layout. No trailing slash, no spurious
// `=` segment.
func TestComputeResultDirEmptyParamsMap(t *testing.T) {
	got := ComputeResultDirForInstance(2, "gwas", "answer", map[string]string{})
	want := "enju/runs/2-gwas/answer"
	if got != want {
		t.Errorf("empty map: got %q want %q", got, want)
	}
}

// TestComputeResultDirEmptySlugFallback — an empty slug (e.g.
// a legacy run row predating the slug column) renders as
// "run" so the path is always well-formed. This is the
// fallback every helper relies on.
func TestComputeResultDirEmptySlugFallback(t *testing.T) {
	got := ComputeResultDirForInstance(4, "", "t", nil)
	want := "enju/runs/4-run/t"
	if got != want {
		t.Errorf("empty slug fallback: got %q want %q", got, want)
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
		RunSlug:        "gwas",
	}
	if got := ComputeResultDir(t1); got != "enju/runs/3-gwas/align" {
		t.Errorf("singleton via TaskRecord: got %q", got)
	}

	t2 := &store.TaskRecord{
		ID:             "7:3:sample_S1:align",
		TaskDefID:      "align",
		InstanceParams: `{"sample":"S1"}`,
		RunSlug:        "gwas",
	}
	if got := ComputeResultDir(t2); got != "enju/runs/3-gwas/align/sample=S1" {
		t.Errorf("for_each via TaskRecord: got %q", got)
	}

	t3 := &store.TaskRecord{
		ID:             "7:5:BRCA1_breast:analyze",
		TaskDefID:      "analyze",
		InstanceParams: `{"gene":"BRCA1","tissue":"breast"}`,
		RunSlug:        "gwas",
	}
	if got := ComputeResultDir(t3); got != "enju/runs/5-gwas/analyze/gene=BRCA1/tissue=breast" {
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
		RunSlug:        "gwas",
	}
	got := ComputeResultDir(t1)
	want := "enju/runs/2-gwas/weird"
	if got != want {
		t.Errorf("malformed params fallback: got %q want %q", got, want)
	}
}

// TestComputeResultDirUsesVisibleRoot — pins the headline
// pre-launch change: no dot prefix. Separate assertion so a
// future accidental revert is loud.
func TestComputeResultDirUsesVisibleRoot(t *testing.T) {
	got := ComputeResultDirForInstance(1, "demo", "t", nil)
	if len(got) < len("enju/") || got[:len("enju/")] != "enju/" {
		t.Errorf("path should start with visible 'enju/', got %q", got)
	}
	if got[0] == '.' {
		t.Errorf("path must not start with dot, got %q", got)
	}
}

// TestRunTemplateSnapshotDir locks in the snapshot layout —
// sibling of the result dirs, under the slugged run dir.
// Drift here would silently break executor script resolution
// because task.go looks up exactly this path.
func TestRunTemplateSnapshotDir(t *testing.T) {
	got := RunTemplateSnapshotDir(3, "variant-calling")
	want := "enju/runs/3-variant-calling/template-snapshot"
	if got != want {
		t.Errorf("snapshot dir: got %q want %q", got, want)
	}
	// Empty slug → "run" fallback, same as the result-dir
	// variant. Keeps the path well-formed for legacy rows.
	if got := RunTemplateSnapshotDir(5, ""); got != "enju/runs/5-run/template-snapshot" {
		t.Errorf("empty slug fallback: got %q", got)
	}
}

// TestComputeRunSlug covers the slug-derivation rule used by
// the server at create_run time. Precedence: bundle dir →
// run name → "run" fallback. Every branch is asserted here
// because a silent drift between client-side and server-side
// slug computation would corrupt the layout (e.g. snapshot
// dir lands at a different path than result dirs).
func TestComputeRunSlug(t *testing.T) {
	cases := []struct {
		name     string
		srcPath  string
		runName  string
		want     string
	}{
		{"template bundle wins", "enju/templates/variant-calling", "Ignored", "variant-calling"},
		{"nested bundle path", "workflows/gwas-analysis", "", "gwas-analysis"},
		{"inline with name", "", "My Smoke Test", "my-smoke-test"},
		{"slug fallback on empty", "", "", "run"},
		{"name with unsafe chars", "", "Run: A/B", "run-a-b"},
		// Template bundle already lowercase-hyphenated — kebab
		// slugger is idempotent, no drift.
		{"bundle stays idempotent", "enju/templates/variant-calling", "", "variant-calling"},
		// Case normalization: mixed-case bundle becomes lower.
		// Previously the loader accepted any dir name; now we
		// normalize so on-disk and git paths agree with the
		// display form.
		{"uppercase bundle normalized", "enju/templates/MyBundle", "", "mybundle"},
		// Only whitespace/punctuation → "run" fallback (trim
		// of dashes leaves nothing).
		{"all-punctuation falls back", "", "!!!", "run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeRunSlug(c.srcPath, c.runName)
			if got != c.want {
				t.Errorf("ComputeRunSlug(%q, %q) = %q, want %q", c.srcPath, c.runName, got, c.want)
			}
		})
	}
}
