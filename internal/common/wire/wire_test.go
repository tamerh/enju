package wire

import "testing"

func TestIsTerminalRunState(t *testing.T) {
	for _, s := range []string{"completed", "failed", "aborted", "terminated"} {
		if !IsTerminalRunState(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []string{"active", "waiting", "idle", "paused"} {
		if IsTerminalRunState(s) {
			t.Errorf("%q should not be terminal", s)
		}
	}
}
