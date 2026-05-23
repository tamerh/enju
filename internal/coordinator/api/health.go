package api

import "net/http"

// storageChecker is the optional capability a store may expose to
// report whether its on-disk file is still intact (not unlinked or
// replaced underneath the running process). The concrete
// *store.Store implements it; mocks that don't simply skip the
// check, so this stays off the CoordinatorStore interface.
type storageChecker interface {
	StorageIntact() error
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Ghost-DB guard: if the state database file was wiped or
	// swapped while the coordinator is running (e.g. ~/.enju/
	// cleared under a live process), the server keeps serving
	// stale state from a deleted file descriptor with no other
	// signal. Surface it here so a health probe — or an operator
	// curling /health after a dev wipe — sees it as degraded
	// rather than a misleading "ok".
	if c, ok := s.store.(storageChecker); ok {
		if err := c.StorageIntact(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "degraded",
				"error":  err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
