package types

// Pins the on-the-wire CitizenKind values — an API/DB contract.
// "human" and "agent" are persisted and cross the wire; changing
// either string is a breaking change, so this LOUD guard trips if
// someone edits a value. The legacy literal "bot" and the removed
// "model" kind must NOT validate: there are exactly two kinds, and
// a model is NOT one (it has no identity; it's a label on work).

import "testing"

func TestCitizenKindWireValues(t *testing.T) {
	cases := []struct {
		got  CitizenKind
		want string
	}{
		{CitizenKindHuman, "human"},
		{CitizenKindAgent, "agent"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("CitizenKind wire value = %q, want %q (changing this is an API break)", c.got, c.want)
		}
	}

	if IsValidCitizenKind("bot") {
		t.Error(`IsValidCitizenKind("bot") = true; the legacy literal must no longer validate after the agent flip`)
	}
	if !IsValidCitizenKind("agent") {
		t.Error(`IsValidCitizenKind("agent") = false; the new wire value must validate`)
	}
	// A model is not a citizen kind anymore — it has no identity.
	if IsValidCitizenKind("model") {
		t.Error(`IsValidCitizenKind("model") = true; "model" is no longer a citizen kind (a model is a label on the work, not a citizen)`)
	}
}
