package mcphandlers

// Per-MCP-session state for the fat-client tool handlers.
// apiClient is the handler-context shell — handlers close over
// it via the tool dispatcher. It owns exactly two things:
//
//   - session: the service.Session that holds the coord HTTP
//     client, workspace, model name, logger, and the cached
//     citizen profile. Every handler reaches the orchestration
//     layer through c.session.X(...).
//   - notifySess: the optional auto-subscribe notification
//     session (lifecycle is bound to the MCP transport, not the
//     service layer, so it stays here).
//
// The apiClient methods below (username, Token, get/post/put/delete,
// commitAuthor, citizenKind) are thin forwarders that compress
// hot-path call sites — `c.username()` reads better than
// `c.session.Username()` when sprinkled across many handlers.
// They do not duplicate state; every read goes through the
// session.

import (
	"context"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

type apiClient struct {
	session    *service.Session
	notifySess *notifySession
}

// username forwards to the session's coord client. Updated
// transparently by the coord client's auto-reregister flow —
// readers always see the current value.
func (c *apiClient) username() string { return c.session.Username() }

// Token forwards to the session's coord client. The notify
// supervisor uses this as a TokenFn at construction time so it
// picks up auto-reregister rotations live, without holding a
// stale copy.
func (c *apiClient) Token() string { return c.session.Coord().Token() }

// get/post/put/delete forward to the coord client. Kept as
// methods on apiClient so the handler call sites read
// `c.get(...)` (compact) without exposing handlers to the
// coord package directly. Every other write/read in handlers
// either uses these shims or reaches a service-layer method
// via c.session.
func (c *apiClient) get(ctx context.Context, path string) ([]byte, error) {
	return c.session.Coord().Get(ctx, path)
}

func (c *apiClient) post(ctx context.Context, path string, body interface{}) ([]byte, error) {
	return c.session.Coord().Post(ctx, path, body)
}

func (c *apiClient) put(ctx context.Context, path string, body interface{}) ([]byte, error) {
	return c.session.Coord().Put(ctx, path, body)
}

func (c *apiClient) delete(ctx context.Context, path string) ([]byte, error) {
	return c.session.Coord().Delete(ctx, path)
}

// commitAuthor forwards to the session's profile cache.
func (c *apiClient) commitAuthor(ctx context.Context) (name, email string) {
	return c.session.CommitAuthor(ctx)
}

// citizenKind forwards to the session's profile cache.
func (c *apiClient) citizenKind(ctx context.Context) string {
	return c.session.CitizenKind(ctx)
}
