package mcphandlers

// Agent registration handlers. Thin MCP tools that wire to the
// corresponding API endpoints — each handler is a marshal-call-
// format shim. Heavy logic stays in the API layer where the auth
// context is; this file translates MCP arg shapes to JSON request
// bodies. A model is not a citizen and has no registration tool —
// it is a label stamped on the work at submit time.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/mark3labs/mcp-go/mcp"
)

// errorFromResponse forwards to coord.ExtractError. Kept as a
// package-local name because every call site already uses the
// short form; the implementation lives in coord so service-layer
// code shares the same envelope-extraction logic.
func errorFromResponse(data []byte) string {
	return coord.ExtractError(data)
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
	data, err := c.post(ctx, "/api/v1/citizens/me/agents", body)
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
	fmt.Fprintf(&b, "✓ Agent registered: @%s (%s)\n", resp["username"], resp["name"])
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
	data, err := c.get(ctx, "/api/v1/citizens/me/agents")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Check for an error response BEFORE handing to the
	// formatter — the auth middleware returns 401 with
	// {"error": "..."} which would otherwise decode as an
	// empty Bots slice and silently mask auth failures.
	if msg := errorFromResponse(data); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(format.BotList(data)), nil
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

