package compute

import (
	"strings"
	"testing"
)

func TestStderrTail(t *testing.T) {
	t.Run("short string returned verbatim", func(t *testing.T) {
		s := "boom\n"
		if got := StderrTail(s, 1000); got != s {
			t.Errorf("got %q, want unchanged %q", got, s)
		}
	})

	t.Run("keeps the tail, drops the head", func(t *testing.T) {
		// The real error is the LAST line; the head is noise.
		var b strings.Builder
		for i := 0; i < 500; i++ {
			b.WriteString("noise progress line that is just chatter\n")
		}
		b.WriteString("PANIC: the actual root cause\n")
		got := StderrTail(b.String(), 200)
		if !strings.Contains(got, "PANIC: the actual root cause") {
			t.Errorf("tail must retain the final error line, got:\n%s", got)
		}
		if strings.Contains(got, "noise progress line") &&
			!strings.HasPrefix(got, "...(truncated)") {
			t.Errorf("head noise leaked without truncation marker:\n%s", got)
		}
		if !strings.HasPrefix(got, "...(truncated)\n") {
			t.Errorf("truncated output must be marked, got prefix %q", got[:20])
		}
		if len(got) > len("...(truncated)\n")+200 {
			t.Errorf("result exceeds cap: %d bytes", len(got))
		}
	})

	t.Run("snaps to a line boundary (no dangling half-line)", func(t *testing.T) {
		s := "aaaaaaaaaa\nbbbbbbbbbb\ncccccccccc\n"
		got := StderrTail(s, 15) // mid-"bbb..." window
		body := strings.TrimPrefix(got, "...(truncated)\n")
		// Every retained line must be whole.
		for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			if ln != "" && ln != "bbbbbbbbbb" && ln != "cccccccccc" {
				t.Errorf("partial line leaked: %q (full: %q)", ln, got)
			}
		}
	})

	t.Run("non-positive cap is a no-op", func(t *testing.T) {
		if got := StderrTail("anything", 0); got != "anything" {
			t.Errorf("cap 0 must pass through, got %q", got)
		}
	})
}
