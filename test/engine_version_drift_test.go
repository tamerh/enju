package test

import (
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestEngineVersionsMatch pins store.TestEngineVersion (the
// duplicated constant the store package's test helpers use,
// because store can't import engine without a cycle) to
// engine.EngineVersion. If you bump engine.EngineVersion,
// also bump TestEngineVersion in
// internal/coordinator/store/helpers_test.go.
//
// This test lives in test/ because it's the natural seam
// where both packages can be imported without cycles.
func TestEngineVersionsMatch(t *testing.T) {
	if engine.EngineVersion != store.TestEngineVersion {
		t.Fatalf("engine.EngineVersion (%q) and store.TestEngineVersion (%q) have drifted.\n"+
			"Bump store.TestEngineVersion in internal/coordinator/store/helpers_test.go.",
			engine.EngineVersion, store.TestEngineVersion)
	}
}
