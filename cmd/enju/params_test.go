package main

import (
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

func TestCoerceCLIParams_StringPassthrough(t *testing.T) {
	declared := []enjuYaml.ParamDef{{Name: "dataset", Type: "string"}}
	got, err := coerceCLIParams(map[string]interface{}{"dataset": "tp53"}, declared)
	if err != nil {
		t.Fatal(err)
	}
	if got["dataset"] != "tp53" {
		t.Errorf("string passthrough: got %v", got["dataset"])
	}
}

func TestCoerceCLIParams_IntConversion(t *testing.T) {
	declared := []enjuYaml.ParamDef{{Name: "n", Type: "int"}}
	got, err := coerceCLIParams(map[string]interface{}{"n": "42"}, declared)
	if err != nil {
		t.Fatal(err)
	}
	if got["n"] != int64(42) {
		t.Errorf("int coerce: got %v (%T)", got["n"], got["n"])
	}
}

func TestCoerceCLIParams_IntInvalid(t *testing.T) {
	declared := []enjuYaml.ParamDef{{Name: "n", Type: "int"}}
	if _, err := coerceCLIParams(map[string]interface{}{"n": "not-a-number"}, declared); err == nil {
		t.Error("expected error for non-numeric int value")
	}
	if _, err := coerceCLIParams(map[string]interface{}{"n": "1.5"}, declared); err == nil {
		t.Error("expected error for decimal in int slot (strict parser)")
	}
}

func TestCoerceCLIParams_BoolForms(t *testing.T) {
	declared := []enjuYaml.ParamDef{{Name: "f", Type: "bool"}}
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true}, {"TRUE", true}, {"yes", true}, {"1", true},
		{"false", false}, {"no", false}, {"0", false},
	}
	for _, c := range cases {
		got, err := coerceCLIParams(map[string]interface{}{"f": c.in}, declared)
		if err != nil {
			t.Errorf("bool %q: %v", c.in, err)
			continue
		}
		if got["f"] != c.want {
			t.Errorf("bool %q: got %v, want %v", c.in, got["f"], c.want)
		}
	}
}

func TestCoerceCLIParams_BoolInvalid(t *testing.T) {
	declared := []enjuYaml.ParamDef{{Name: "f", Type: "bool"}}
	if _, err := coerceCLIParams(map[string]interface{}{"f": "maybe"}, declared); err == nil {
		t.Error("expected error for non-bool string")
	}
}

func TestCoerceCLIParams_ListSplits(t *testing.T) {
	declared := []enjuYaml.ParamDef{{Name: "xs", Type: "list<string>"}}
	got, err := coerceCLIParams(map[string]interface{}{"xs": "a | b|c"}, declared)
	if err != nil {
		t.Fatal(err)
	}
	xs, ok := got["xs"].([]interface{})
	if !ok || len(xs) != 3 {
		t.Fatalf("list split: got %#v", got["xs"])
	}
	if xs[0] != "a" || xs[1] != "b" || xs[2] != "c" {
		t.Errorf("list values trimmed wrong: %v", xs)
	}
}

func TestCoerceCLIParams_UnknownKeyPassesThrough(t *testing.T) {
	// Validator catches unknown keys later with a better message;
	// the coercer shouldn't reject them at the boundary.
	got, err := coerceCLIParams(map[string]interface{}{"ghost": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["ghost"] != "x" {
		t.Errorf("unknown key: got %v", got["ghost"])
	}
}

func TestCoerceCLIParams_NonStringPassthrough(t *testing.T) {
	// A future caller might supply already-typed values (the
	// MCP path's JSON decoder produces float64 / bool / etc).
	// Those should ride through untouched.
	declared := []enjuYaml.ParamDef{{Name: "n", Type: "int"}}
	got, err := coerceCLIParams(map[string]interface{}{"n": int64(7)}, declared)
	if err != nil {
		t.Fatal(err)
	}
	if got["n"] != int64(7) {
		t.Errorf("typed passthrough: got %v", got["n"])
	}
}
