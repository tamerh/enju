// Package coord is the fat-client's HTTP client to the Enju
// coordinator REST API. Owns the bearer token (with auto re-
// register on stale-citizen 404/401), the citizen identity
// triplet (username + display name + email) the re-register
// flow needs, and the shared http.Client.
//
// The single Client instance is constructed once at MCP server
// boot and shared across every handler that needs to talk to
// the coordinator. Methods are safe for concurrent use; the
// auth-token field is atomic and re-register is mutex-
// serialized so concurrent calls only fire one refresh.
//
// This package is the single chokepoint for fat-client →
// coordinator HTTP. Handlers (mcphandlers/*), the orchestration
// layer (service/*), and subsystem packages (notify, compute,
// etc.) all reach the coordinator through here. Wire-shape
// types are decoded by callers — coord doesn't know about
// claim/submit/etc., it just moves bytes.
package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Client is the per-MCP-session HTTP client to the coordinator.
// Holds the bearer token, identity for re-register, and the
// shared http.Client. Construct via New; mutate the token via
// SetToken (or let auto-reregister do it); read identity via
// Username/CitizenName/CitizenEmail accessors.
type Client struct {
	baseURL string

	// identity fields — needed by ensureCitizenFresh to
	// re-register after a coordinator DB wipe. username can
	// change on re-register (rare; only if the coordinator
	// allocates a different one); accessed via Username() to
	// stay race-free.
	identityMu  sync.RWMutex
	username   string
	citizenName string
	citizenEmail string

	// authToken: bearer the client sends on every request.
	// Mutated by doWithAutoReregister on a successful refresh,
	// read from many goroutines. atomic.Value keeps it race-
	// free without per-request locking. Always holds a string
	// (possibly empty); never nil.
	authToken atomic.Value

	saveCreds func(username, name, email, token string)
	logger   *slog.Logger
	http    *http.Client

	// reRegisterMu serializes refresh attempts so concurrent
	// tool calls only trigger one re-register.
	reRegisterMu sync.Mutex
}

// Config is the constructor input for New. All fields except
// AuthToken are required; an empty AuthToken is allowed (the
// first request will fail 401 → auto re-register).
type Config struct {
	BaseURL     string
	Username    string
	CitizenName  string
	CitizenEmail  string
	AuthToken    string
	SaveCredentials func(username, name, email, token string)
	Logger     *slog.Logger
	HTTPClient   *http.Client // optional; default http.DefaultClient
}

// New constructs a Client. Fields that aren't immediately
// usable (HTTPClient, Logger) get sensible defaults.
func New(cfg Config) *Client {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	c := &Client{
		baseURL:    cfg.BaseURL,
		username:    cfg.Username,
		citizenName:  cfg.CitizenName,
		citizenEmail: cfg.CitizenEmail,
		saveCreds:   cfg.SaveCredentials,
		logger:    logger,
		http:     httpClient,
	}
	c.authToken.Store(cfg.AuthToken)
	return c
}

// BaseURL returns the configured coordinator base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// Username returns the current citizen username. Safe for
// concurrent use; reflects auto-reregister rotations.
func (c *Client) Username() string {
	c.identityMu.RLock()
	defer c.identityMu.RUnlock()
	return c.username
}

// CitizenName returns the cached display name.
func (c *Client) CitizenName() string {
	c.identityMu.RLock()
	defer c.identityMu.RUnlock()
	return c.citizenName
}

// CitizenEmail returns the cached email (may be empty).
func (c *Client) CitizenEmail() string {
	c.identityMu.RLock()
	defer c.identityMu.RUnlock()
	return c.citizenEmail
}

// Token returns the current bearer token. Safe to call from
// any goroutine; readers see whatever the most recent SetToken
// completed. Returns "" before initial SetToken.
func (c *Client) Token() string {
	v := c.authToken.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// SetToken atomically replaces the bearer.
func (c *Client) SetToken(tok string) {
	c.authToken.Store(tok)
}

// Get issues a GET against c.baseURL+path. Wraps doWithAutoReregister
// so a stale-citizen 404/401 triggers a transparent re-register +
// retry once.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		c.attachAuth(req)
		return c.http.Do(req)
	})
}

// Post issues a POST with a JSON-marshalled body.
func (c *Client) Post(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.attachAuth(req)
		return c.http.Do(req)
	})
}

// Put issues a PUT with a JSON-marshalled body.
func (c *Client) Put(ctx context.Context, path string, body interface{}) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+path, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.attachAuth(req)
		return c.http.Do(req)
	})
}

// Delete issues a DELETE.
func (c *Client) Delete(ctx context.Context, path string) ([]byte, error) {
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		c.attachAuth(req)
		return c.http.Do(req)
	})
}

func (c *Client) attachAuth(req *http.Request) {
	if tok := c.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// doWithAutoReregister runs an HTTP request closure and, if
// the response signals that the caller's citizen record no
// longer exists on the coordinator (typically: server DB was
// wiped), re-registers with the cached identity and replays
// the request once. Registering with a stable username is
// idempotent — the coordinator recreates a citizen with the
// same handle so URLs and bodies built from username stay
// valid across the retry.
//
// Only one retry is attempted. If the retry also fails (for
// any reason), the retry's response is returned as-is.
func (c *Client) doWithAutoReregister(ctx context.Context, do func() (*http.Response, error)) ([]byte, error) {
	resp, err := do()
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !isStaleCitizenResponse(resp.StatusCode, data) {
		return data, nil
	}
	if c.CitizenName() == "" {
		// No display name to re-register with — caller invoked
		// `enju mcp` with only a username, so we can't recreate
		// the record automatically. Surface the original error.
		c.logger.Warn("stale citizen detected but CitizenName is empty; cannot auto re-register",
			"username", c.Username())
		return data, nil
	}
	if err := c.EnsureCitizenFresh(ctx); err != nil {
		c.logger.Warn("auto re-register failed", "username", c.Username(), "error", err)
		return data, nil
	}
	c.logger.Info("auto re-registered stale citizen, retrying request", "username", c.Username())
	resp2, err := do()
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable (after re-register): %w", err)
	}
	data2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	return data2, nil
}

// isStaleCitizenResponse tells whether the response body looks
// like a coordinator "citizen not found / invalid token" error.
// Matches the two error message forms writeError emits in
// internal/coordinator/api/. Only considers 404 / 401 to avoid
// misidentifying a 200 that happens to contain the phrase.
func isStaleCitizenResponse(status int, body []byte) bool {
	s := strings.ToLower(string(body))
	if status == http.StatusNotFound {
		return strings.Contains(s, "citizen") && strings.Contains(s, "not found")
	}
	if status == http.StatusUnauthorized {
		return strings.Contains(s, "invalid") && strings.Contains(s, "token")
	}
	return false
}

// EnsureCitizenFresh POSTs /citizens/register with the cached
// identity. Used by auto-reregister to recreate a citizen
// record after a coordinator DB wipe. Serialized so concurrent
// tool calls only fire one refresh.
//
// Updates the in-memory username + token on success and
// invokes the SaveCredentials callback if configured.
func (c *Client) EnsureCitizenFresh(ctx context.Context) error {
	c.reRegisterMu.Lock()
	defer c.reRegisterMu.Unlock()

	username := c.Username()
	name := c.CitizenName()
	email := c.CitizenEmail()

	body := map[string]string{"name": name}
	if username != "" {
		body["username"] = username
	}
	if email != "" {
		body["email"] = email
	}
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v1/citizens/register", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("coordinator unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("register returned %d: %s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decoding register response: %w", err)
	}
	if errMsg, ok := result["error"].(string); ok && errMsg != "" {
		return fmt.Errorf("%s", errMsg)
	}
	got, _ := result["username"].(string)
	if got == "" {
		return fmt.Errorf("register response missing username")
	}
	gotToken, _ := result["token"].(string)

	c.identityMu.Lock()
	c.username = got
	c.identityMu.Unlock()
	if gotToken != "" {
		c.SetToken(gotToken)
	}
	if c.saveCreds != nil {
		c.saveCreds(got, name, email, gotToken)
	}
	return nil
}
