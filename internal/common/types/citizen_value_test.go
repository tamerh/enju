package types

// Pins the on-the-wire CitizenKind values. These are a product/API
// surface (spec-bot-to-agent §4): the Go identifier CitizenKindBot
// deliberately keeps its historical name while its VALUE is "agent".
// Anyone who "fixes" the name≠value mismatch by editing the value
// trips this test — that is the point. It is the LOUD guard that
// makes the single-source flip safe.

import "testing"

func TestCitizenKindWireValues(t *testing.T) {
	cases := []struct {
		got  CitizenKind
		want string
	}{
		{CitizenKindHuman, "human"},
		{CitizenKindBot, "agent"}, // name keeps "Bot", wire value is "agent"
		{CitizenKindModel, "model"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("CitizenKind wire value = %q, want %q (changing this is an API break — see spec-bot-to-agent §4)", c.got, c.want)
		}
	}

	if IsValidCitizenKind("bot") {
		t.Error(`IsValidCitizenKind("bot") = true; the legacy literal must no longer validate after the agent flip`)
	}
	if !IsValidCitizenKind("agent") {
		t.Error(`IsValidCitizenKind("agent") = false; the new wire value must validate`)
	}
}
