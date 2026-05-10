package webui

import (
	"fmt"
	"net/http"
	"time"
)

// logRequest logs one line per request at info level: method,
// path, status, duration. Cheap; lets operators answer "did the
// browser hit the server" without enabling chi's verbose
// logger middleware.
func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)
		s.logger.Info("ui request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusRecorder wraps http.ResponseWriter to capture the
// status code for logging. Same trick chi uses internally;
// duplicated here to avoid pulling middleware.WrapResponseWriter
// for one field.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// requireSameOriginForWrites is the CSRF guard for state-
// changing requests. Same-origin policy is the entire auth
// story for `enju ui`: the binary binds 127.0.0.1 only, the
// bearer token sits in-process, and only requests originating
// from this same UI may mutate state.
//
// Without this, a malicious page on evil.com could fire a
// fetch() at http://127.0.0.1:8484/actions/claim and the
// browser would happily send it. The Origin header is set by
// the browser and cannot be forged from JavaScript, so
// checking it here closes the gap.
//
// GET / HEAD / OPTIONS are passed through (read-only requests
// are not the CSRF risk). All other methods MUST carry
// Origin, and Origin MUST match one of the allowed values
// (http://127.0.0.1:PORT or http://localhost:PORT). Missing
// or mismatched Origin → 403.
//
// Same-origin GET-from-link navigation does NOT include
// Origin in many browsers — that's why we exempt safe methods
// rather than require Origin everywhere.
func (s *Server) requireSameOriginForWrites(next http.Handler) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("http://127.0.0.1:%d", s.port): true,
		fmt.Sprintf("http://localhost:%d", s.port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" || !allowed[origin] {
			s.logger.Warn("origin check rejected request",
				"method", r.Method, "path", r.URL.Path,
				"origin", origin)
			http.Error(w, "forbidden: cross-origin write", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
