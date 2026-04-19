package mcpserver

// HTTP-transport layer for the MCP client. apiClient wraps the
// coordinator REST base URL with the citizen's identity +
// workspace handle, and exposes small get/post/put helpers that
// every tool handler uses. Also hosts the auto-reregister flow
// that recovers from coordinator DB wipes by re-POSTing
// /citizens/register with the cached identity on a 404/401
// "citizen not found / invalid token" response.

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

	"github.com/enju-ai/enju/internal/mcpgit"
)

type apiClient struct {
	baseURL      string
	username     string // caller's citizen username — stable across auto re-registers
	citizenName  string // display name, used when re-registering after a DB wipe
	citizenEmail string // optional, passed to the register endpoint
	modelName   string // LLM model for contribution tracking
	authToken   string // registration token for identity verification
	saveCreds    func(username, name, email, token string)
	workspace    *mcpgit.Workspace
	logger       *slog.Logger
	httpClient   *http.Client

	// reRegisterMu serializes re-registration attempts so concurrent
	// tool calls only trigger one refresh. Acquired by
	// ensureCitizenFresh before firing the register POST.
	reRegisterMu sync.Mutex

	// Cached citizen profile (name + email) used to populate git
	// commit author fields on the fat-client submit path. Fetched
	// lazily on first use and held for the life of the MCP client
	// process. Reasoning: citizen profile changes via
	// enju_update_profile are rare within a single session, and
	// paying one GET per submit just to avoid staleness is
	// wasteful. If a citizen does update their profile mid-session
	// the next process restart will pick up the new values.
	profileOnce  sync.Once
	profileName  string
	profileEmail string
}

func (c *apiClient) get(ctx context.Context, path string) ([]byte, error) {
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		if c.authToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.authToken)
		}
		return c.httpClient.Do(req)
	})
}

func (c *apiClient) put(ctx context.Context, path string, body interface{}) ([]byte, error) {
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
		if c.authToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.authToken)
		}
		return c.httpClient.Do(req)
	})
}

func (c *apiClient) delete(ctx context.Context, path string) ([]byte, error) {
	return c.doWithAutoReregister(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		if c.authToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.authToken)
		}
		return c.httpClient.Do(req)
	})
}

func (c *apiClient) post(ctx context.Context, path string, body interface{}) ([]byte, error) {
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
		if c.authToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.authToken)
		}
		return c.httpClient.Do(req)
	})
}

// doWithAutoReregister runs an HTTP request closure and, if the
// response body signals that the caller's citizen record no longer
// exists on the coordinator (typically: the server DB was wiped),
// re-registers with the same username + display name and replays
// the request once. Registering with a stable username is
// idempotent — the coordinator recreates a citizen with the same
// handle, so URLs embedding c.username and request bodies built
// from c.username stay valid across the retry.
//
// Only one retry is attempted. If the retry also fails (for any
// reason), the retry's response is returned as-is.
func (c *apiClient) doWithAutoReregister(ctx context.Context, do func() (*http.Response, error)) ([]byte, error) {
	resp, err := do()
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !isStaleCitizenResponse(resp.StatusCode, data) {
		return data, nil
	}
	if c.citizenName == "" {
		// No display name to re-register with — the caller
		// invoked `enju mcp` with only a username, which means
		// we can't recreate the record automatically. Return
		// the original response so the handler surfaces the
		// coordinator's error.
		c.logger.Warn("stale citizen detected but CitizenName is empty; cannot auto re-register",
			"username", c.username)
		return data, nil
	}
	if err := c.ensureCitizenFresh(ctx); err != nil {
		c.logger.Warn("auto re-register failed", "username", c.username, "error", err)
		return data, nil
	}
	c.logger.Info("auto re-registered stale citizen, retrying request", "username", c.username)
	resp2, err := do()
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable (after re-register): %w", err)
	}
	data2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	return data2, nil
}

// isStaleCitizenResponse tells whether the response body looks like
// a coordinator "citizen not found" error. Matches the two error
// message forms writeError currently emits from
// internal/api/router.go: `citizen "foo" not found` and the plain
// `citizen not found`. Only considers 404 responses to avoid
// misidentifying a 200 that happens to contain the phrase.
func isStaleCitizenResponse(status int, body []byte) bool {
	s := strings.ToLower(string(body))
	// 404 with "citizen not found" — DB wiped, citizen record gone.
	if status == http.StatusNotFound {
		return strings.Contains(s, "citizen") && strings.Contains(s, "not found")
	}
	// 401 with "invalid or expired token" — DB wiped, token
	// no longer valid. Re-register will get a fresh token.
	if status == http.StatusUnauthorized {
		return strings.Contains(s, "invalid") && strings.Contains(s, "token")
	}
	return false
}

// ensureCitizenFresh POSTs /citizens/register with the client's
// cached username + display name. Used by the auto-reregister flow
// to recreate a citizen record after a coordinator DB wipe.
// Serialized by reRegisterMu so concurrent tool calls only fire
// one register.
func (c *apiClient) ensureCitizenFresh(ctx context.Context) error {
	c.reRegisterMu.Lock()
	defer c.reRegisterMu.Unlock()
	body := map[string]string{"name": c.citizenName}
	if c.username != "" {
		body["username"] = c.username
	}
	if c.citizenEmail != "" {
		body["email"] = c.citizenEmail
	}
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v1/citizens/register", bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
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
	c.username = got
	if gotToken != "" {
		c.authToken = gotToken
	}
	if c.saveCreds != nil {
		c.saveCreds(got, c.citizenName, c.citizenEmail, gotToken)
	}
	return nil
}

// --- Project + profile helpers shared across tool handlers ---

// commitAuthor returns the `name email` pair to use as git commit
// author for submits made on this citizen's behalf. Fetches the
// citizen profile from the coordinator once and caches it for the
// life of the MCP client process. Falls back to the configured
// display name (from `enju mcp -name`) when no profile is
// available, and to a synthetic `{username}@enju.local` address
// when the citizen hasn't set a real email.
//
// Real email addresses attribute commits to the right GitHub user
// when they match the citizen's GitHub email; synthetic ones at
// least make different citizens' commits distinguishable in
// contributor graphs instead of collapsing to one bot identity.
func (c *apiClient) commitAuthor(ctx context.Context) (name, email string) {
	c.profileOnce.Do(func() {
		// Default values — used if the fetch fails.
		c.profileName = c.username
		c.profileEmail = c.username + "@enju.local"

		data, err := c.get(ctx, "/api/v1/citizens/by-username/"+c.username)
		if err != nil {
			c.logger.Warn("commitAuthor: failed to fetch profile, using defaults",
				"username", c.username, "error", err)
			return
		}
		var p map[string]interface{}
		if err := json.Unmarshal(data, &p); err != nil {
			return
		}
		if n, ok := p["name"].(string); ok && n != "" {
			c.profileName = n
		}
		if e, ok := p["email"].(string); ok && e != "" {
			c.profileEmail = e
		}
	})
	return c.profileName, c.profileEmail
}

// fetchProjectMeta reads a project's metadata from the coordinator.
// Used by the client-side project_remote_status / project_sync /
// get_artifact / get_artifact_history / set_project_remote handlers
// that need the project's remote_url to open the local clone.
func (c *apiClient) fetchProjectMeta(ctx context.Context, projectID int64) (remoteURL string, err error) {
	remoteURL, _, err = c.fetchProjectMetaFull(ctx, projectID)
	return
}

// fetchProjectMetaFull is like fetchProjectMeta but also returns the
// human-readable project name for workspace directory naming.
func (c *apiClient) fetchProjectMetaFull(ctx context.Context, projectID int64) (remoteURL, name string, err error) {
	remoteURL, name, _, err = c.fetchProjectMetaExpanded(ctx, projectID)
	return
}

// openProject fetches project metadata, opens the workspace
// clone, AND wires the project's default_branch into the
// Project so Pull/Push fallback paths (enju_list_templates,
// enju_get_artifact, enju_project_sync) target the right ref.
// The tester-reported bug was exactly that: default_branch was
// plumbed through the API but discarded at the workspace layer,
// so every fallback silently used "main" regardless of what the
// project was configured with. Every call site that pairs
// fetchProjectMetaFull + workspace.ForProject should use this
// helper instead to get the wiring for free.
func (c *apiClient) openProject(ctx context.Context, projectID int64) (proj *mcpgit.Project, remoteURL, projName, defaultBranch string, err error) {
	if c.workspace == nil {
		return nil, "", "", "", fmt.Errorf("no workspace configured")
	}
	remoteURL, projName, defaultBranch, err = c.fetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return nil, "", "", "", err
	}
	proj, err = c.workspace.ForProject(projectID, remoteURL, projName)
	if err != nil {
		return nil, remoteURL, projName, defaultBranch, err
	}
	proj.SetDefaultBranch(defaultBranch)
	return proj, remoteURL, projName, defaultBranch, nil
}

// fetchProjectMetaExpanded returns remote_url + name +
// default_branch. Called from paths that need the branch name
// to configure the workspace (submit / claim / execute) so
// Pull/Push target the right ref.
func (c *apiClient) fetchProjectMetaExpanded(ctx context.Context, projectID int64) (remoteURL, name, defaultBranch string, err error) {
	data, err := c.get(ctx, fmt.Sprintf("/api/v1/projects/%d", projectID))
	if err != nil {
		return "", "", "", err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", "", fmt.Errorf("parsing project: %w", err)
	}
	if errMsg, ok := raw["error"].(string); ok {
		return "", "", "", fmt.Errorf("%s", errMsg)
	}
	if v, ok := raw["remote_url"].(string); ok {
		remoteURL = v
	}
	if v, ok := raw["name"].(string); ok {
		name = v
	}
	if v, ok := raw["default_branch"].(string); ok {
		defaultBranch = v
	}
	return remoteURL, name, defaultBranch, nil
}
