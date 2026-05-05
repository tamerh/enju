// HTTP-backed CoordClient — the production implementation of
// the runner's coord interface. Wraps the fatclient's coord HTTP
// client with the bot-specific filtering + submit shape that
// the runner expects.
//
// Walking-skeleton scope: action=review and action=vote only.
// These are the LLM-friendly text-output cases that don't need
// the git/commit machinery action=answer/contribute/compute
// require. Phase 2.4+ will add a workspace-aware path for the
// commit-bearing actions.

package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// HTTPCoordClient implements CoordClient against a real
// coordinator over HTTP. Holds onto a coord.Client (which
// owns the auth token + auto-reregister logic) and translates
// between the Runner's narrow CoordClient interface and the
// coord's REST surface.
type HTTPCoordClient struct {
	C *coord.Client
}

// ListReadyForBot fetches /api/v1/tasks/ready and filters to
// tasks whose assign_to includes the bot's username. The coord
// doesn't filter assign_to server-side today — the bot pays
// the cost of the extra rows but the trade-off is fine for the
// task volumes a single bot sees.
//
// projectID > 0 narrows the query at the coord; projectID == 0
// returns ready tasks across every project the bot is a member of.
func (h *HTTPCoordClient) ListReadyForBot(ctx context.Context, projectID int64, botUsername string) ([]TaskInfo, error) {
	path := "/api/v1/tasks/ready"
	if projectID > 0 {
		path = fmt.Sprintf("%s?project_id=%d", path, projectID)
	}
	data, err := h.C.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("list ready tasks: %w", err)
	}
	// Decode just the fields the runner needs — the coord's
	// TaskResponse has 50+ fields, most irrelevant here.
	var raw []struct {
		ID         string   `json:"id"`
		Action     string   `json:"action"`
		Prompt     string   `json:"prompt"`
		UserPrompt string   `json:"user_prompt"`
		AssignTo   []string `json:"assign_to"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode ready tasks: %w", err)
	}
	out := make([]TaskInfo, 0, len(raw))
	for _, t := range raw {
		// Filter: this bot must be in assign_to. By design —
		// an empty assign_to means "any project member," and
		// we don't want bots racing humans for unowned tasks
		// (humans pick freely; bots only pick up what's
		// explicitly addressed to them, including the
		// reviewer-bot / tester-bot patterns where the YAML
		// says assign_to: reviewer-bot). Operators who want
		// a bot to pick up open work should add the bot's
		// username to the task's assign_to list.
		//
		// TODO(server-side-filter): the coord doesn't filter
		// assign_to today, so we ship the entire ready list
		// over the wire and filter client-side. Fine at the
		// task volumes a single bot sees; at scale, push the
		// filter into the /tasks/ready handler.
		if !contains(t.AssignTo, botUsername) {
			continue
		}
		out = append(out, TaskInfo{
			ID:         t.ID,
			Action:     t.Action,
			Prompt:     t.Prompt,
			UserPrompt: t.UserPrompt,
			AssignTo:   t.AssignTo,
		})
	}
	return out, nil
}

// Claim posts to /api/v1/tasks/{id}/claim. Translates the
// "already claimed" 4xx into ErrClaimRace so the runner can
// gracefully skip and try the next task.
//
// Wire-error detection note: coord.Client.Post returns the
// body even for 4xx — Go errors only fire on transport
// failure. So we have to inspect the response body's "error"
// field to detect application-level failures like the race.
func (h *HTTPCoordClient) Claim(ctx context.Context, taskID, botUsername, model string) error {
	body := map[string]string{
		"username": botUsername,
		"model":    model,
	}
	data, err := h.C.Post(ctx, "/api/v1/tasks/"+taskID+"/claim", body)
	if err != nil {
		return fmt.Errorf("coord claim: %w", err)
	}
	if msg := errorFromResponse(data); msg != "" {
		if isClaimRaceMessage(msg) {
			return ErrClaimRace
		}
		return fmt.Errorf("coord claim: %s", msg)
	}
	return nil
}

// Submit translates the LLM's text response into the
// per-action submit body and POSTs it.
//
// Walking-skeleton scope:
//   - action=review: parse the verdict from the response's first
//     line (approve/reject), use the rest as the comment text.
//     If unparseable, default to "reject" — bots should NEVER
//     silently approve work they couldn't grade.
//   - action=vote: the response IS the vote choice. The runner's
//     system prompt is responsible for instructing the LLM to
//     return exactly one vote option.
//   - any other action: returns an error directing the operator
//     to assign the task to a human until the git-aware path
//     lands (Phase 2.4+).
//
// commit_sha is omitted — review and vote submissions are
// commit-less per the coord's per-action contract (see
// submit.go's "vote and review are decisions" comment).
func (h *HTTPCoordClient) Submit(ctx context.Context, task TaskInfo, response, model string) error {
	body := map[string]interface{}{
		"model":    model,
		"username": h.C.Username(),
	}
	switch task.Action {
	case "review":
		decision, comment := parseReviewResponse(response)
		body["decision"] = decision
		body["content"] = comment
	case "vote":
		body["option"] = strings.TrimSpace(response)
	default:
		return fmt.Errorf("bot daemon doesn't support action=%q yet — assign to a human or wait for git-aware bot support (Phase 2.4+)", task.Action)
	}
	data, err := h.C.Post(ctx, "/api/v1/tasks/"+task.ID+"/result", body)
	if err != nil {
		return fmt.Errorf("coord submit: %w", err)
	}
	if msg := errorFromResponse(data); msg != "" {
		return fmt.Errorf("coord submit: %s", msg)
	}
	return nil
}

// errorFromResponse pulls the "error" field out of a coord
// JSON response body. Returns "" when the body has no error
// field (success path). Mirrors the helper in mcphandlers but
// duplicated here so the bots package doesn't drag the MCP
// handler dependencies in.
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

// parseReviewResponse pulls the verdict out of the LLM's
// review response. Convention: the first non-empty line starts
// with one of the four verdict keywords (approve, reject,
// request_changes, comment). Anything after that line is the
// reviewer's free text. Unparseable input defaults to
// "request_changes" — the safest reaction to "I can't grade
// this": ask for revisions rather than silently approve OR
// hard-reject (which kills the cascade).
//
// The four verdicts are the coord's full review surface:
//   - approve: ship it; cascade proceeds
//   - request_changes: send back for revision; target re-opens
//   - reject: terminal failure; cascade dies on the target's branch
//   - comment: non-blocking annotation; cascade unaffected
//
// Examples:
//
//	"approve\nthe code looks correct"   → ("approve", "the code looks correct")
//	"REJECT — missing tests"            → ("reject", "missing tests")
//	"request_changes\nFix bug in X"     → ("request_changes", "Fix bug in X")
//	"comment: nice naming"              → ("comment", "nice naming")
//	"hmm not sure"                      → ("request_changes", "hmm not sure")
func parseReviewResponse(s string) (decision, comment string) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "request_changes", "(empty response from LLM — defaulting to request_changes)"
	}
	lines := strings.SplitN(trimmed, "\n", 2)
	first := strings.TrimSpace(lines[0])
	rest := ""
	if len(lines) > 1 {
		rest = strings.TrimSpace(lines[1])
	}
	lower := strings.ToLower(first)
	// Order matters: check the longer keywords first so
	// "request_changes" doesn't get clipped to "request" if
	// we ever add "request" as a separate verdict.
	for _, verdict := range []string{"request_changes", "approve", "reject", "comment"} {
		if strings.HasPrefix(lower, verdict) {
			afterKeyword := strings.TrimSpace(first[len(verdict):])
			afterKeyword = strings.TrimLeft(afterKeyword, " :—-")
			switch {
			case rest != "" && afterKeyword != "":
				return verdict, afterKeyword + "\n" + rest
			case afterKeyword != "":
				return verdict, afterKeyword
			default:
				return verdict, rest
			}
		}
	}
	// Couldn't find a verdict keyword. Default to
	// request_changes + surface the full response as the
	// comment. Safer than reject (which kills the cascade) or
	// approve (which silently ships unreviewable work).
	return "request_changes", trimmed
}

// isClaimRaceMessage detects the coord's "task already claimed"
// response from a typed error message. Substring-match on the
// known coord-side phrases — brittle vs perfect, but a
// conventional way to interpret the coord's free-text error
// surface until it grows typed error codes.
func isClaimRaceMessage(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "already claimed") ||
		strings.Contains(low, "task is locked") ||
		strings.Contains(low, "no longer ready")
}

// contains is a tiny helper for slice membership without
// pulling in slices.Contains (which would also work — just
// shorter for one call site).
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Compile-time assertion: HTTPCoordClient satisfies CoordClient.
var _ CoordClient = (*HTTPCoordClient)(nil)
