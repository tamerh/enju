package yaml

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVoteOption_JSONShapeIsLowercase pins the load-bearing wire
// contract for tasks.vote_options. The coord side stores it via
// json.Marshal([]VoteOption{...}); the bot daemon
// (parseVoteOptions), the submit handler, and tally all decode it
// keyed on lowercase "id". Before json: tags were added,
// json.Marshal fell back to the exported Go field names ("ID"),
// which none of those decoders recognized → empty options → vote
// responses shipped unparsed and coord rejected the submit. This
// test fails loudly if the tags regress.
func TestVoteOption_JSONShapeIsLowercase(t *testing.T) {
	b, err := json.Marshal([]VoteOption{
		{ID: "terse", Label: "Terse one-block report"},
		{ID: "detailed", Label: "Detailed report", Activates: []string{"stats"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)

	want := `[{"id":"terse","label":"Terse one-block report"},` +
		`{"id":"detailed","label":"Detailed report","activates":["stats"]}]`
	if got != want {
		t.Errorf("VoteOption JSON shape drifted:\n got: %s\nwant: %s", got, want)
	}
	// Explicit guard against the exact regression (Go field-name
	// fallback) so the failure message names the cause.
	for _, bad := range []string{`"ID"`, `"Label"`, `"Activates"`} {
		if strings.Contains(got, bad) {
			t.Errorf("found Go field-name key %s — json: tags missing/regressed on VoteOption", bad)
		}
	}

	// Round-trips back through a lowercase-keyed decoder (the
	// shape parseVoteOptions / the submit handler expect).
	var rt []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rt) != 2 || rt[0].ID != "terse" || rt[1].ID != "detailed" {
		t.Errorf("round-trip ids wrong: %+v", rt)
	}
}
