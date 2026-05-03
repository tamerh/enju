package mcptools

import "testing"

// TestRegistryNoDuplicates pins the "every tool listed once"
// invariant. Two entries with the same name = a programmer
// error in the registry that would surface as silent duplicate
// tool registration on the wire.
func TestRegistryNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Registry {
		name := e.Tool.Name
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

// TestRegistryCoversBothSides smoke-checks that the
// classification didn't accidentally collapse to one side.
// Catches the regression where a refactor moves all entries
// to one side and we lose the architectural intent.
func TestRegistryCoversBothSides(t *testing.T) {
	if len(Coordinator()) == 0 {
		t.Error("Coordinator() returned no tools — registry classification missing")
	}
	if len(FatClient()) == 0 {
		t.Error("FatClient() returned no tools — registry classification missing")
	}
}

// TestByName_RoundTrip pins the lookup helper. ByName must
// find every registered tool by its exact name.
func TestByName_RoundTrip(t *testing.T) {
	for _, e := range Registry {
		got, ok := ByName(e.Tool.Name)
		if !ok {
			t.Errorf("ByName(%q) returned ok=false for a registered tool", e.Tool.Name)
			continue
		}
		if got.Tool.Name != e.Tool.Name {
			t.Errorf("ByName(%q) returned wrong tool: %q", e.Tool.Name, got.Tool.Name)
		}
		if got.Side != e.Side {
			t.Errorf("ByName(%q) returned wrong side: got %v want %v", e.Tool.Name, got.Side, e.Side)
		}
	}
	if _, ok := ByName("enju_does_not_exist"); ok {
		t.Error("ByName(unknown) returned ok=true; expected false")
	}
}
