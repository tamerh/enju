package compute

import "strings"

// StderrTail returns at most maxBytes from the END of s.
//
// A failed script's stderr says *why* at the bottom — the panic,
// the traceback, the last error line. The top is usually noise
// (startup banners, progress, deprecation warnings). Capping with
// s[:maxBytes] (what the fail-reason builders did before) threw
// away exactly the part an operator needs and kept the part they
// don't. This keeps the tail instead.
//
// When s is longer than maxBytes the cut is snapped forward to the
// next newline so the result starts on a clean line (no dangling
// half-line), and a "...(truncated)" marker is prefixed so the
// reader knows there was more above.
func StderrTail(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	tail := s[len(s)-maxBytes:]
	// Drop a leading partial line so we begin at a real line
	// boundary. Guard i+1 so a tail that is exactly "...\n" (the
	// newline is the last byte) doesn't collapse to empty.
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	return "...(truncated)\n" + tail
}
