package service

import "testing"

// TestFormatIterationLabel_HidesRecordEnvKeys pins that the
// list<record> env-expansion keys (`<var>__<field>`, which become
// ENJU_PARAM_<var>__<field>) are NOT shown in the iteration
// label — the bare `<var>` key already identifies the instance,
// and dumping every record field would make run-status output
// unreadable.
func TestFormatIterationLabel_HidesRecordEnvKeys(t *testing.T) {
	cases := []struct {
		name           string
		instanceParams string
		instanceKey    string
		want           string
	}{
		{
			name:           "record instance: only the bare var shows",
			instanceParams: `{"gene":"tp53","gene__name":"TP53","gene__slug":"tp53","gene__hits":"7"}`,
			instanceKey:    "tp53",
			want:           "gene=tp53",
		},
		{
			name:           "list<string> instance unaffected",
			instanceParams: `{"item":"a"}`,
			instanceKey:    "a",
			want:           "item=a",
		},
		{
			name:           "multi-var list<string> still joins all non-__ keys",
			instanceParams: `{"region":"us","tier":"gold"}`,
			instanceKey:    "us_gold",
			want:           "region=us, tier=gold",
		},
		{
			name:           "all-__ params (defensive): fall back to instanceKey",
			instanceParams: `{"gene__name":"TP53"}`,
			instanceKey:    "tp53",
			want:           "tp53",
		},
		{
			name:           "empty params: instanceKey",
			instanceParams: "",
			instanceKey:    "k",
			want:           "k",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatIterationLabel(tc.instanceParams, tc.instanceKey)
			if got != tc.want {
				t.Errorf("FormatIterationLabel(%q, %q) = %q, want %q",
					tc.instanceParams, tc.instanceKey, got, tc.want)
			}
		})
	}
}
