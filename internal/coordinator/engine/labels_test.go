package engine

import (
	"testing"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// TestStateLabel_FailedRetryable pins the new state's wire value
// and human label. The wire value "failed_retryable" is durable
// (it lands in the DB and crosses the API); a rename is a
// migration, so this guards an accidental edit. The label must
// be visibly distinct from terminal "failed" so an operator
// reading run status can tell "errored, retry it" from "dead".
func TestStateLabel_FailedRetryable(t *testing.T) {
	if store.TaskFailedRetryable != "failed_retryable" {
		t.Fatalf("wire value drifted: %q (DB/API-durable; a change is a migration)", store.TaskFailedRetryable)
	}
	if got := StateLabel(store.TaskFailedRetryable); got != "failed (retryable)" {
		t.Errorf("StateLabel(failed_retryable) = %q, want %q", got, "failed (retryable)")
	}
	// Must not collide with terminal failed's label.
	if StateLabel(store.TaskFailedRetryable) == StateLabel(store.TaskFailed) {
		t.Error("failed_retryable and failed must render distinct labels")
	}
}
