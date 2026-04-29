package mcpserver

// Bot + model registration handlers (operator/model design).
// Five MCP tools that wire to the corresponding API endpoints —
// each handler is a thin marshal-call-format shim. Heavy logic
// stays in the API layer where the auth context is, with this
// file translating MCP arg shapes to JSON request bodies.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// errorFromResponse decodes a JSON body and returns the value of
// its "error" field, or "" if absent. Used by list-style handlers
// that decode into typed structs and would otherwise silently
// mask auth failures (the auth middleware returns 401 with
// {"error": "..."} which is structurally compatible with most
// list response shapes).
func errorFromResponse(data []byte) string {
	var probe map[string]interface{}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	if msg, ok := probe["error"].(string); ok {
		return msg
	}
	return ""
}

func (c *apiClient) handleRegisterBot(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	body := map[string]interface{}{}
	for _, k := range []string{"name", "username", "role", "label"} {
		if v, ok := args[k]; ok {
			if s, _ := v.(string); s != "" {
				body[k] = s
			}
		}
	}
	if body["name"] == nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	data, err := c.post(ctx, "/api/v1/citizens/me/bots", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decode response: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok {
		return mcp.NewToolResultError(errMsg), nil
	}

	// Format the response with the token prominently shown — this
	// is the ONE TIME the caller sees it, so we make sure they
	// can't miss it.
	var b strings.Builder
	fmt.Fprintf(&b, "✓ Bot registered: @%s (%s)\n", resp["username"], resp["name"])
	fmt.Fprintf(&b, "  Owned by: @%s\n", resp["parent_name"])
	if label, _ := resp["label"].(string); label != "" {
		fmt.Fprintf(&b, "  Label:    %s\n", label)
	}
	fmt.Fprintf(&b, "\n  TOKEN (stash this NOW — cannot be retrieved later):\n  %s\n", resp["token"])
	fmt.Fprintf(&b, "\n  To use: drop it into the bot's launcher as the Bearer token.\n")
	fmt.Fprintf(&b, "  To revoke: enju_revoke_token token=%s\n", resp["token"])
	return mcp.NewToolResultText(b.String()), nil
}

func (c *apiClient) handleListMyBots(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/citizens/me/bots")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Check for an error response BEFORE decoding into the
	// typed struct. The auth middleware returns 401 with
	// {"error": "..."} which would otherwise decode as an
	// empty Bots slice and silently mask auth failures.
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	var resp struct {
		Bots []struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
			Role     string `json:"role"`
			Tokens   []struct {
				ID        int64  `json:"id"`
				Label     string `json:"label"`
				IssuedAt  string `json:"issued_at"`
				RevokedAt string `json:"revoked_at,omitempty"`
			} `json:"tokens"`
		} `json:"bots"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decode: " + err.Error()), nil
	}
	if len(resp.Bots) == 0 {
		return mcp.NewToolResultText("You don't own any bots yet. Use enju_register_bot to create one."), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your bots (%d):\n", len(resp.Bots))
	for _, bot := range resp.Bots {
		fmt.Fprintf(&b, "\n@%s — %s (role: %s)\n", bot.Username, bot.Name, bot.Role)
		if len(bot.Tokens) == 0 {
			b.WriteString("  (no tokens)\n")
			continue
		}
		for _, t := range bot.Tokens {
			label := t.Label
			if label == "" {
				label = "(no label)"
			}
			status := "active"
			if t.RevokedAt != "" {
				status = "revoked " + t.RevokedAt
			}
			fmt.Fprintf(&b, "  token #%d  %s  issued %s  [%s]\n", t.ID, label, t.IssuedAt, status)
		}
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (c *apiClient) handleRevokeToken(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	body := map[string]interface{}{}
	if v, ok := args["token"]; ok {
		if s, _ := v.(string); s != "" {
			body["token"] = s
		}
	}
	if v, ok := args["token_id"]; ok {
		// MCP numbers come through as float64.
		if f, ok := v.(float64); ok && f != 0 {
			body["token_id"] = int64(f)
		}
	}
	if len(body) == 0 {
		return mcp.NewToolResultError("either token or token_id is required"), nil
	}
	data, err := c.post(ctx, "/api/v1/tokens/revoke", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decode: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText("✓ Token revoked. It will no longer authenticate."), nil
}

func (c *apiClient) handleListModels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/models")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	var resp struct {
		Models []struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decode: " + err.Error()), nil
	}
	if len(resp.Models) == 0 {
		return mcp.NewToolResultText("Catalog is empty. (Unexpected — the migration seeds 10 popular models. Check coordinator logs.)"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Model catalog (%d):\n", len(resp.Models))
	for _, m := range resp.Models {
		fmt.Fprintf(&b, "  %-30s  %s\n", m.Username, m.Name)
	}
	b.WriteString("\nUse the username (left column) as the -model flag value.\n")
	return mcp.NewToolResultText(b.String()), nil
}

func (c *apiClient) handleRegisterModel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	body := map[string]interface{}{}
	if v, ok := args["username"]; ok {
		if s, _ := v.(string); s != "" {
			body["username"] = s
		}
	}
	if v, ok := args["display_name"]; ok {
		if s, _ := v.(string); s != "" {
			body["display_name"] = s
		}
	}
	if body["username"] == nil {
		return mcp.NewToolResultError("username is required"), nil
	}
	data, err := c.post(ctx, "/api/v1/models", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("decode: " + err.Error()), nil
	}
	if errMsg, ok := resp["error"].(string); ok {
		return mcp.NewToolResultError(errMsg), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Model registered: %s (%s)", resp["username"], resp["display_name"])), nil
}
