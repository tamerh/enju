package mcphandlers

// enju_notifications — read/unread notification list for the
// active project. Reads {project_clone}/enju/events/live.jsonl,
// filters lines through the 9 built-in Layer 1 default rules,
// returns the latest matches with `*` (unread) / ` ` (read)
// markers in newest-first order.
//
// State: a single integer cursor at {project_clone}/enju/events/
// notifications-read-seq tracking the highest seq the user has
// "seen." Calling enju_notifications with mark_read=true (the
// default) advances this cursor to the highest seq returned, so
// items shown as unread on this call appear as read on the next.
//
// Honors enju/notify.yaml's `disable_defaults` so users can mute
// individual default rules.
//
// Performance: scans live.jsonl backward from EOF in 64KB chunks
// and stops as soon as `limit` matches are collected. A 1GB log
// + recent activity → only the tail is read; the whole-file scan
// only happens when the requested limit exceeds the number of
// matches in recent history. Replaces the original
// "always-read-the-whole-file" approach which got slow once
// projects accumulated multi-MB logs.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/notify"
	"github.com/mark3labs/mcp-go/mcp"
)

// notification is one rendered item ready for display.
type notification struct {
	Seq      int64
	Ts       time.Time
	RuleName string
	Message  string
}

// handleNotifications returns the list with read/unread markers.
//
// Args:
//
//	project_id  (required) the project to surface
//	limit       optional — default 20, max 100
//	mark_read   optional — default true; advances the read cursor
//	            to the highest seq returned
func (c *apiClient) handleNotifications(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	limit := req.GetInt("limit", 20)
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	markRead := req.GetBool("mark_read", true)

	if c.workspace == nil {
		return mcp.NewToolResultError("workspace not configured"), nil
	}
	projectDir := c.workspace.ProjectDir(int64(projectID))
	if projectDir == "" {
		return mcp.NewToolResultText("(no notifications — project has no local clone yet; will populate after first task work)"), nil
	}

	livePath := filepath.Join(projectDir, "enju", "events", "live.jsonl")
	matches, err := readLatestNotifications(livePath, c.username, projectDir, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read live log: %v", err)), nil
	}

	readSeqPath := filepath.Join(projectDir, "enju", "events", "notifications-read-seq")
	lastReadSeq := loadReadSeq(readSeqPath)

	out := formatNotifications(matches, lastReadSeq)

	if markRead && len(matches) > 0 {
		// Highest seq returned advances the read cursor.
		highest := matches[0].Seq // matches is newest-first
		if highest > lastReadSeq {
			if err := saveReadSeq(readSeqPath, highest); err != nil {
				c.logger.Warn("notifications: failed to persist read-seq", "err", err)
			}
		}
	}

	return mcp.NewToolResultText(out), nil
}

// formatNotifications renders the user-visible list. Plain ASCII
// — leading "*" for unread, two spaces for read. Newest first.
func formatNotifications(matches []notification, lastReadSeq int64) string {
	if len(matches) == 0 {
		return "(no notifications)"
	}
	var b strings.Builder
	for _, m := range matches {
		marker := "  "
		if m.Seq > lastReadSeq {
			marker = "* "
		}
		fmt.Fprintf(&b, "%s%s  %s\n",
			marker,
			m.Ts.Local().Format("2006-01-02 15:04:05"),
			m.Message,
		)
	}
	return strings.TrimRight(b.String(), "\n")
}

// readLatestNotifications scans live.jsonl backward from EOF
// and returns the last `limit` events that match a Layer 1
// default rule (filtered by the project's notify.yaml
// disable_defaults). Output order: newest-first.
func readLatestNotifications(livePath, username, projectDir string, limit int) ([]notification, error) {
	uc, _ := notify.LoadUserConfig(notify.UserConfigPath(projectDir))
	defaults := notify.EffectiveDefaults(uc.DisableDefaults)
	if len(defaults) == 0 {
		return nil, nil
	}
	cfg := notify.Config{Username: username}

	var out []notification
	err := tailJSONL(livePath, func(line []byte) (stop bool) {
		var ev notify.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return false // skip malformed line
		}
		for _, rule := range defaults {
			if !notify.PredicateMatches(rule.When, ev, cfg) {
				continue
			}
			out = append(out, notification{
				Seq:      ev.Seq,
				Ts:       ev.Timestamp,
				RuleName: rule.Name,
				Message:  notify.RenderTemplate(rule.Message, ev),
			})
			break // one notification per event
		}
		return len(out) >= limit
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// tailJSONL reads a JSONL file backward from EOF in 64KB chunks
// and invokes fn once per line, newest-first. fn returns true to
// stop scanning early. Missing file → no-op (nil error).
//
// Why backward: live.jsonl is append-only and seq-ordered, so
// the newest events live at the tail. Scanning from end with an
// early-stop budget means a 1GB log doesn't pay 1GB of read cost
// when the caller only wants the latest 20 matches.
func tailJSONL(path string, fn func(line []byte) (stop bool)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()
	if fileSize == 0 {
		return nil
	}

	const chunkSize = int64(64 * 1024)

	pos := fileSize
	var carry []byte // start-of-newer-chunk fragment, prepended to next read
	for pos > 0 {
		readSize := chunkSize
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize

		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return err
		}
		if len(carry) > 0 {
			buf = append(buf, carry...)
		}

		// Walk newlines from end to start. Each newline boundary
		// produces one line (the bytes between this newline and
		// the previous). Anything before the first newline is a
		// fragment we save for the next (earlier) chunk.
		end := len(buf)
		stop := false
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			line := bytes.TrimSpace(buf[i+1 : end])
			end = i
			if len(line) == 0 {
				continue
			}
			if fn(line) {
				stop = true
				break
			}
		}
		if stop {
			return nil
		}

		if pos == 0 {
			// Reached file start. Anything left in buf[:end] is
			// the earliest line.
			line := bytes.TrimSpace(buf[:end])
			if len(line) > 0 {
				fn(line)
			}
			return nil
		}
		// Save partial line for next iteration.
		carry = make([]byte, end)
		copy(carry, buf[:end])
	}
	return nil
}

func loadReadSeq(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func saveReadSeq(path string, seq int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

