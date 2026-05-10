package format

// Phase 8.5 renderer tests. RenderBlockedBy is the surface-side
// counterpart to store.computeBlockedBy: each kind's JSON
// becomes a one-line "Blocked by:" string for run_status.
// These tests pin one input/output pair per kind so a future
// renderer tweak can't drift the format silently.

import (
	"strings"
	"testing"
	"time"
)

func TestRenderBlockedBy_Review(t *testing.T) {
	since := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	raw := `{"kind":"review","task":"1:1:gate","assignee":"alice","since":"` + since + `"}`
	got := RenderBlockedBy(raw)
	wantParts := []string{"Blocked by: review", "1:1:gate", "alice", "h ago"}
	for _, w := range wantParts {
		if !strings.Contains(got, w) {
			t.Errorf("RenderBlockedBy(review) = %q; want substring %q", got, w)
		}
	}
}

func TestRenderBlockedBy_HumanClaim(t *testing.T) {
	raw := `{"kind":"human_claim","task":"1:1:edit","assignee":"bob"}`
	got := RenderBlockedBy(raw)
	if !strings.Contains(got, "Blocked by: human claim") {
		t.Errorf("missing prefix in %q", got)
	}
	if !strings.Contains(got, "1:1:edit") || !strings.Contains(got, "bob") {
		t.Errorf("missing fields in %q", got)
	}
}

func TestRenderBlockedBy_Artifact(t *testing.T) {
	raw := `{"kind":"artifact","task":"1:1:reader","awaiting_path":"out/data.json"}`
	got := RenderBlockedBy(raw)
	if !strings.Contains(got, "artifact") {
		t.Errorf("missing 'artifact' in %q", got)
	}
	if !strings.Contains(got, "1:1:reader") || !strings.Contains(got, "out/data.json") {
		t.Errorf("missing fields in %q", got)
	}
}

func TestRenderBlockedBy_Stuck(t *testing.T) {
	raw := `{"kind":"stuck","detail":"no actionable blocker identified"}`
	got := RenderBlockedBy(raw)
	if !strings.HasPrefix(got, "Blocked by: stuck") {
		t.Errorf("expected 'Blocked by: stuck' prefix, got %q", got)
	}
	if !strings.Contains(got, "no actionable") {
		t.Errorf("detail missing in %q", got)
	}
}

func TestRenderBlockedBy_EmptyAndMalformed(t *testing.T) {
	cases := []string{"", "  ", "not-json", `{"kind":"unknown"}`, `{"kind":""}`}
	for _, raw := range cases {
		if got := RenderBlockedBy(raw); got != "" {
			t.Errorf("RenderBlockedBy(%q) = %q; want empty", raw, got)
		}
	}
}

func TestHumanizeSince(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		offset time.Duration
		want   string
	}{
		{30 * time.Second, "s ago"},
		{5 * time.Minute, "m ago"},
		{2 * time.Hour, "h ago"},
		{3 * 24 * time.Hour, "d ago"},
	}
	for _, c := range cases {
		ts := now.Add(-c.offset).Format(time.RFC3339)
		got := humanizeSince(ts)
		if !strings.HasSuffix(got, c.want) {
			t.Errorf("humanizeSince(now-%v) = %q; want suffix %q", c.offset, got, c.want)
		}
	}
}
