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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/enju-ai/enju/internal/bots"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

// reconcileInterval is how often the background ticker sweeps each
// served project for opportunistic reconcile. 20s is conservative
const reconcileInterval = 20 * time.Second

type apiClient struct {
	fc         *service.FatClient
	notifySess *notifySession

	// supervisor is the long-lived bot daemon manager. Created
	// at first need (lazy) so a fatclient that never starts a
	// bot doesn't pay the os.UserHomeDir + path-resolution
	// cost. Concurrent MCP tool calls share a single
	// Supervisor instance so the in-memory tracking map is
	// the authoritative state for this fatclient session.
	//
	// supervisorMu serializes lazy construction so concurrent
	// first-callers can't each NewSupervisor + fire reconcile.
	// MCP tool dispatch is serialized at the transport layer
	// today, but the race window is real and would bite the
	// moment dispatch went concurrent. Tests pre-inject
	// c.supervisor at construction; the locked check at the
	// top of botSupervisor() honors that path without racing.
	supervisorMu sync.Mutex
	supervisor   *bots.Supervisor
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

// runReconcileTicker starts the background opportunistic reconcile
// loop. Tick every reconcileInterval. Scoped to CWD and the active
// notify session to match "the project(s) this MCP is serving"
// semantics. Only spawned when cfg.Notify is configured (i.e.,
// notify session is active).
func (c *apiClient) runReconcileTicker(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			c.fc.Logger().Error("reconcile ticker panicked", "recover", r)
		}
	}()

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Dynamic resolution per tick: check both the CWD (implicit
			// served project) and the notify session (explicitly
			// touched project).
			ids := make(map[int64]bool)

			if cwd, err := os.Getwd(); err == nil {
				if entry, _ := c.fc.ProjectRegistry().FindContaining(cwd); entry != nil {
					ids[entry.ID] = true
				}
			}

			if activeID := c.notifySess.ProjectID(); activeID != 0 {
				ids[activeID] = true
			}

			for id := range ids {
				// Only the elected reconcile owner for this project
				// does the git fetch/merge. Duplicate MCP servers
				// (piled up across /mcp reconnects) stand their
				// tickers down here instead of all contending on the
				// project write lock.
				if !c.fc.OwnsReconcile(id) {
					continue
				}
				c.reconcileActiveRuns(ctx, id)
			}
		}
	}
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
// Thread-safety: supervisorMu serializes lazy construction so
// concurrent first-callers can't each NewSupervisor + fire a
// duplicate reconcile goroutine. MCP tool dispatch is
// serialized at the transport layer today, but the race window
// is real and would bite the moment dispatch went concurrent.
//
// Test seam: when c.supervisor is pre-injected (tests build
// apiClient with a Supervisor pointing at a fake binary +
// tempdir PIDDir/LogDir), the locked nil-check returns the
// injected instance without calling NewSupervisor — otherwise
// the lazy ctor would overwrite the test's supervisor with
// one pointing at the operator's real ~/.enju/bots/pids.
func (c *apiClient) botSupervisor() (*bots.Supervisor, error) {
	c.supervisorMu.Lock()
	defer c.supervisorMu.Unlock()
	if c.supervisor != nil {
		return c.supervisor, nil
	}
	s, err := bots.NewSupervisor()
	if err != nil {
		return nil, err
	}
	c.supervisor = s
	// Reconcile stale auto_run_ids from a previous fatclient
	// session. Best-effort, fire-and-forget — if the coord is
	// unreachable we'd rather log and continue than block
	// supervisor construction. Stale refs that survive this
	// pass will be GC'd lazily by the next terminal event the
	// tailer observes for them.
	go func() {
		if err := s.Reconcile(context.Background(), c.isRunTerminal); err != nil {
			slog.Default().Warn("supervisor reconcile failed", "error", err)
		}
	}()
	return s, nil
}

// isRunTerminal implements bots.IsRunTerminal for the supervisor's
// startup reconcile. Returns terminal=true when the coord reports
// the run in {completed, failed, terminated} OR when the coord
// doesn't know the run (HTTP 404 → coord DB was wiped between
// fatclient sessions). The latter bias is intentional: a lingering
// auto-managed bot waiting on a run that no longer exists serves
// no purpose, and the operator can always restart it manually.
//
// "skipped" is NOT in the terminal set: it's a TASK state, never
// a RUN state — runs that ought to be "skipped" become "terminated"
// via the cascade in applyTerminateRun.
//
// Uses coord.Client.GetStatus to read the HTTP status without
// the historical Client.Get behavior of swallowing 4xx into a
// nil-error data return — string-matching the body for "404"
// would silently break the moment the coord changed its error
// format.
func (c *apiClient) isRunTerminal(ctx context.Context, projectID, runSeq int64) (bool, error) {
	data, status, err := c.fc.Coord().GetStatus(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runSeq))
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return true, nil
	}
	if status >= 400 {
		return false, fmt.Errorf("coord run lookup: HTTP %d: %s", status, truncateForLog(data, 512))
	}
	var resp map[string]any
	if jerr := json.Unmarshal(data, &resp); jerr != nil {
		return false, fmt.Errorf("decode run: %w", jerr)
	}
	state, _ := resp["state"].(string)
	switch state {
	case "completed", "failed", "terminated":
		return true, nil
	}
	return false, nil
}

// truncateForLog clips a byte slice for inclusion in an error
// message — coord error bodies can be HTML pages on
// proxy/middleware failures, and a 30KB error line is unhelpful.
func truncateForLog(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
