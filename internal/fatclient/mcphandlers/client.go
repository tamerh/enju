package mcphandlers

// Per-MCP-session state for the fat-client tool handlers.
// apiClient is the handler-context shell — handlers close over
// it via the tool dispatcher. It owns exactly two things:
//
//   - fc: the service.FatClient that holds the coord HTTP
//     client, workspace, model name, logger, and the cached
//     citizen profile. Every handler reaches the orchestration
//     layer through c.fc.X(...).
//   - notifySess: the optional auto-subscribe notification
//     session (lifecycle is bound to the MCP transport, not the
//     service layer, so it stays here).
//
// The apiClient methods below (username, Token, get/post/put/delete,
// commitAuthor, citizenKind) are thin forwarders that compress
// hot-path call sites — `c.username()` reads better than
// `c.fc.Username()` when sprinkled across many handlers.
// They do not duplicate state; every read goes through the
// FatClient handle.

import (
	"context"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

type apiClient struct {
	fc         *service.FatClient
	notifySess *notifySession

	// supervisor is the long-lived bot daemon manager. Created
	// at first need (lazy) so a fatclient that never starts a
	// bot doesn't pay the os.UserHomeDir + path-resolution
	// cost. Concurrent MCP tool calls share a single
	// Supervisor instance so the in-memory tracking map is
	// the authoritative state for this fatclient session.
	supervisor *bots.Supervisor
}

// username forwards to the FatClient's coord client. Updated
// transparently by the coord client's auto-reregister flow —
// readers always see the current value.
func (c *apiClient) username() string { return c.fc.Username() }

// Token forwards to the FatClient's coord client. The notify
// supervisor uses this as a TokenFn at construction time so it
// picks up auto-reregister rotations live, without holding a
// stale copy.
func (c *apiClient) Token() string { return c.fc.Coord().Token() }

// get/post/put/delete forward to the coord client. Kept as
// methods on apiClient so the handler call sites read
// `c.get(...)` (compact) without exposing handlers to the
// coord package directly. Every other write/read in handlers
// either uses these shims or reaches a service-layer method
// via c.fc.
func (c *apiClient) get(ctx context.Context, path string) ([]byte, error) {
	return c.fc.Coord().Get(ctx, path)
}

func (c *apiClient) post(ctx context.Context, path string, body interface{}) ([]byte, error) {
	return c.fc.Coord().Post(ctx, path, body)
}

func (c *apiClient) put(ctx context.Context, path string, body interface{}) ([]byte, error) {
	return c.fc.Coord().Put(ctx, path, body)
}

func (c *apiClient) delete(ctx context.Context, path string) ([]byte, error) {
	return c.fc.Coord().Delete(ctx, path)
}

// commitAuthor forwards to the FatClient's profile cache.
func (c *apiClient) commitAuthor(ctx context.Context) (name, email string) {
	return c.fc.CommitAuthor(ctx)
}

// citizenKind forwards to the FatClient's profile cache.
func (c *apiClient) citizenKind(ctx context.Context) string {
	return c.fc.CitizenKind(ctx)
}

// botSupervisor returns the lazily-constructed bot supervisor.
// First call resolves ~/.enju/bots/{pids,logs} and the enju
// binary path; subsequent calls return the cached instance.
// Errors from NewSupervisor (no $HOME, no os.Executable) are
// surfaced once at the call site so the MCP tool can return a
// friendly message instead of panicking.
//
// Thread-safety note: this is a "first-use init" pattern not
// guarded by a mutex. Concurrent first calls would each
// construct a Supervisor and the last one written wins. In
// practice MCP tool dispatch is serialized at the transport
// layer (mcp-go's stdio handler is single-threaded per
// connection), so the race window doesn't fire. If we ever
// see concurrent MCP dispatch the obvious fix is sync.Once.
func (c *apiClient) botSupervisor() (*bots.Supervisor, error) {
	if c.supervisor != nil {
		return c.supervisor, nil
	}
	s, err := bots.NewSupervisor()
	if err != nil {
		return nil, err
	}
	c.supervisor = s
	return s, nil
}
