package api

import (
	"context"
	"net/http"
	"strings"
)

// authMiddleware validates the Bearer token on every request.
// Hard-enforced: missing OR invalid token → 401. The only
// un-authenticated endpoint is /citizens/register (bootstrap),
// which is explicitly whitelisted below.
//
// Prior iterations let missing-token requests fall through for
// backwards-compat. That was removed after Phase J: a coordinator
// that silently ignores "no auth header" leaks project data to
// anyone who just forgets to send one, and the coordinator
// already rejects INVALID tokens — the asymmetry was a pure
// footgun.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bootstrap exception: /citizens/register is the only
		// endpoint that legitimately has no token — that's
		// how a new citizen gets one.
		if r.URL.Path == "/api/v1/citizens/register" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeError(w, http.StatusUnauthorized, "Authorization header is required — send 'Bearer <token>' from your registered citizen")
			return
		}
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "invalid Authorization header — expected 'Bearer <token>'")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		citizen, err := s.store.GetCitizenByToken(token)
		if err != nil || citizen == nil {
			s.logger.Warn("auth: invalid token rejected",
				"method", r.Method, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "invalid or expired token — delete ~/.enju/credentials.json and re-register")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyCitizen, citizen)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
