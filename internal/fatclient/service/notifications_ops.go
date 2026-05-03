package service

// enju_notifications backing — read/unread notification list
// for the active project. Reads
// {project_clone}/enju/events/live.jsonl, filters lines through
// the 9 built-in Layer 1 default rules, returns the latest
// matches with their seq numbers so the caller can surface
// `*` (unread) / ` ` (read) markers in newest-first order.
//
// State: a single integer cursor at
// {project_clone}/enju/events/notifications-read-seq tracking
// the highest seq the user has "seen." MarkNotificationsRead
// advances this cursor.
//
// Performance: scans live.jsonl backward from EOF in 64KB
// chunks and stops as soon as `limit` matches are collected.
// A 1GB log + recent activity → only the tail is read.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/notify"
)

// Notification is one rendered item ready for display by the
// handler.
type Notification struct {
	Seq      int64
	Ts       time.Time
	RuleName string
	Message  string
}

// NotificationsResult bundles the matched events with the
// last-read cursor so the handler can render unread vs read
// markers without a second round trip.
type NotificationsResult struct {
	Matches     []Notification
	LastReadSeq int64
	// ProjectClonePresent is false when the workspace knows the
	// project but no local clone exists yet (handler renders a
	// distinct message). True even when zero matches were found.
	ProjectClonePresent bool
}

// ReadNotifications loads the project's local clone path from
// the workspace, walks live.jsonl backward, applies the
// effective default rules, and returns up to `limit` matches
// (newest-first) along with the persisted read cursor. Returns
// ProjectClonePresent=false when the project has no local
// clone yet — the caller should surface a friendly message
// rather than treat it as an error.
func (s *Session) ReadNotifications(projectID int64, username string, limit int) (*NotificationsResult, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("workspace not configured")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	projectDir := s.workspace.ProjectDir(projectID)
	if projectDir == "" {
		return &NotificationsResult{ProjectClonePresent: false}, nil
	}

	livePath := filepath.Join(projectDir, "enju", "events", "live.jsonl")
	matches, err := readLatestNotifications(livePath, username, projectDir, limit)
	if err != nil {
		return nil, fmt.Errorf("read live log: %w", err)
	}

	readSeqPath := filepath.Join(projectDir, "enju", "events", "notifications-read-seq")
	return &NotificationsResult{
		Matches:             matches,
		LastReadSeq:         loadReadSeq(readSeqPath),
		ProjectClonePresent: true,
	}, nil
}

// MarkNotificationsRead advances the read-seq cursor to the
// highest seq in `matches` (which the caller obtained from
// ReadNotifications). No-op when there are no matches or when
// the highest seq isn't ahead of the persisted cursor.
func (s *Session) MarkNotificationsRead(projectID int64, matches []Notification) error {
	if s.workspace == nil || len(matches) == 0 {
		return nil
	}
	projectDir := s.workspace.ProjectDir(projectID)
	if projectDir == "" {
		return nil
	}
	readSeqPath := filepath.Join(projectDir, "enju", "events", "notifications-read-seq")
	last := loadReadSeq(readSeqPath)
	highest := matches[0].Seq // matches is newest-first
	if highest <= last {
		return nil
	}
	return saveReadSeq(readSeqPath, highest)
}

// readLatestNotifications scans live.jsonl backward from EOF
// and returns the last `limit` events that match a Layer 1
// default rule (filtered by the project's notify.yaml
// disable_defaults). Output order: newest-first.
func readLatestNotifications(livePath, username, projectDir string, limit int) ([]Notification, error) {
	uc, _ := notify.LoadUserConfig(notify.UserConfigPath(projectDir))
	defaults := notify.EffectiveDefaults(uc.DisableDefaults)
	if len(defaults) == 0 {
		return nil, nil
	}
	cfg := notify.Config{Username: username}

	var out []Notification
	err := tailJSONL(livePath, func(line []byte) (stop bool) {
		var ev notify.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return false // skip malformed line
		}
		for _, rule := range defaults {
			if !notify.PredicateMatches(rule.When, ev, cfg) {
				continue
			}
			out = append(out, Notification{
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

// tailJSONL reads a JSONL file backward from EOF in 64KB
// chunks and invokes fn once per line, newest-first. fn
// returns true to stop scanning early. Missing file → no-op
// (nil error).
//
// Why backward: live.jsonl is append-only and seq-ordered, so
// the newest events live at the tail. Scanning from end with
// an early-stop budget means a 1GB log doesn't pay 1GB of
// read cost when the caller only wants the latest 20 matches.
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

		// Walk newlines from end to start.
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
