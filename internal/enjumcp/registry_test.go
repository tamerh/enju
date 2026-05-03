package enjumcp

import "testing"

// TestRegistryNoDuplicates pins the "every tool listed once"
// invariant. Two entries with the same name = a programmer
// error in the registry that would surface as silent duplicate
// tool registration on the wire.
func TestRegistryNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Registry {
		name := e.Name
		if name == "" {
			t.Errorf("registry entry has empty tool name: %+v", e)
			continue
		}
		if seen[name] {
			t.Errorf("registry has duplicate tool name: %s", name)
		}
		seen[name] = true
	}
}

// TestByName_RoundTrip pins the lookup helper. ByName must
// find every registered tool by its exact name.
func TestByName_RoundTrip(t *testing.T) {
	for _, e := range Registry {
		got, ok := ByName(e.Name)
		if !ok {
			t.Errorf("ByName(%q) returned ok=false for a registered tool", e.Name)
			continue
		}
		if got.Name != e.Name {
			t.Errorf("ByName(%q) returned wrong tool: %q", e.Name, got.Name)
		}
	}
	if _, ok := ByName("enju_does_not_exist"); ok {
		t.Error("ByName(unknown) returned ok=true; expected false")
	}
}
