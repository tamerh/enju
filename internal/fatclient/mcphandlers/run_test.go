package mcphandlers

import (
	"testing"
)

func TestIsReconcilableRunState(t *testing.T) {
	// Cases where reconcile should proceed
	for _, state := range []string{"active", "waiting", "idle"} {
		if !isReconcilableRunState(state) {
			t.Errorf("state %q should be reconcilable", state)
		}
	}

	// Cases where reconcile should be skipped
	for _, state := range []string{"completed", "failed", "aborted", "terminated", "paused"} {
		if isReconcilableRunState(state) {
			t.Errorf("state %q should NOT be reconcilable", state)
		}
	}
}
