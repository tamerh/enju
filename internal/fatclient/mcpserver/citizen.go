package mcpserver

// Citizen-profile handlers. enju_my_profile renders the
// current citizen's identity + contribution rollup;
// enju_my_dashboard adds active + recent tasks;
// enju_update_profile merges display-name / email changes
// back to the coordinator.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (c *apiClient) handleUpdateProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Merge semantics: only include fields the caller actually
	// provided in the request. Omitted fields stay untouched on
	// both the server and in credentials.json. This prevents
	// update_profile(name="X") from silently clearing email.
	args := req.GetArguments()
	body := map[string]interface{}{}
	var providedName, providedEmail string
	var haveName, haveEmail bool
	if v, ok := args["name"]; ok {
		s, _ := v.(string)
		if s == "" {
			return mcp.NewToolResultError("name cannot be empty"), nil
		}
		body["name"] = s
		providedName = s
		haveName = true
	}
	if v, ok := args["email"]; ok {
		s, _ := v.(string)
		body["email"] = s
		providedEmail = s
		haveEmail = true
	}
	if len(body) == 0 {
		return mcp.NewToolResultError("at least one of name or email must be provided"), nil
	}

	data, err := c.put(ctx, "/api/v1/citizens/by-username/"+c.username+"/profile", body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var result map[string]interface{}
	if json.Unmarshal(data, &result) == nil {
		if errMsg, ok := result["error"].(string); ok {
			return mcp.NewToolResultError(errMsg), nil
		}
	}

	// Local credentials file: same merge semantics — only touch
	// fields the caller actually provided.
	updateLocalCredentials(haveName, providedName, haveEmail, providedEmail)

	// Show the authoritative current display name in the response.
	// When the caller provided a name, we echo theirs; when they
	// only changed email, we re-fetch the profile so the user
	// sees their existing display name instead of the username
	// handle.
	label := providedName
	if !haveName {
		if profileData, perr := c.get(ctx, "/api/v1/citizens/by-username/"+c.username); perr == nil {
			var prof map[string]interface{}
			if json.Unmarshal(profileData, &prof) == nil {
				if n, _ := prof["name"].(string); n != "" {
					label = n
				}
			}
		}
		if label == "" {
			label = c.username
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("✓ Profile updated: %s", label)), nil
}
func (c *apiClient) handleMyProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/citizens/by-username/"+c.username)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Inject model name from client config so the profile
	// display shows which model this session is using.
	if c.modelName != "" {
		var profileMap map[string]interface{}
		if json.Unmarshal(data, &profileMap) == nil {
			profileMap["model"] = c.modelName
			data, _ = json.Marshal(profileMap)
		}
	}
	// Fetch contribution summary for the enriched profile.
	contribData, contribErr := c.get(ctx, "/api/v1/citizens/by-username/"+c.username+"/contributions")
	if contribErr != nil {
		// Contributions are best-effort — show the basic
		// profile if contributions endpoint fails.
		contribData = nil
	}
	return mcp.NewToolResultText(formatProfile(data, contribData)), nil
}
func (c *apiClient) handleMyDashboard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := c.get(ctx, "/api/v1/citizens/by-username/"+c.username+"/dashboard")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatDashboard(data)), nil
}
