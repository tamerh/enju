package bots

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// auto_stop.go — the live.jsonl tailer that drives the
// auto_bots reference-counted lifecycle.
//
// Flow: NDA.3's enju_create_run starts each bot with
// StartedBy="auto_run" and marks the run's seq on its pid
// file. Once the run reaches a terminal state, coord emits a
// run_completed / run_failed / run_terminated event into
// .enju/events/live.jsonl (per project, via notify). This
// file tails that JSONL, decrements the matching bots'
// AutoRunIDs lists, and stops any that drop to zero refs AND
// are still flagged StartedBy=auto_run.
//
// Why tail the file rather than open a separate long-poll to
// coord: notify is already paying for the long-poll plumbing
// and persisting the events durably to disk. Reading the same
// file from the supervisor is the cheap, decoupled hookup —
// the supervisor doesn't need its own bearer-token rotation
// logic and inherits notify's reconnection / retry behavior
// for free.
//
// Why poll the file rather than fsnotify: fsnotify isn't in
// the dependency closure today, and a 200ms poll on a
// project-scoped file is cheap enough that the simpler
// approach wins. Operators don't notice 200ms delay between
// a run completing and its bots shutting down.

// runTerminalTypes lists the event types that trigger
// auto-stop bookkeeping. Other event types in live.jsonl are
// ignored.
var runTerminalTypes = map[string]bool{
	"run_completed":  true,
	"run_failed":     true,
	"run_terminated": true,
}

// tailEvent mirrors the subset of the wire-event shape the
// auto-stop tailer needs. Local to this file so the supervisor
// doesn't grow a dependency on internal/fatclient/notify just
// to share the type.
type tailEvent struct {
	Seq      int64          `json:"seq"`
	Type     string         `json:"type"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// runSeqFromMetadata extracts the run_seq field from a
// terminal-event metadata blob. Returns 0 if the field is
// absent or the wrong type — caller treats 0 as "skip, can't
// identify the run." Wire encoding is JSON, so numeric
// metadata fields decode to float64.
func runSeqFromMetadata(md map[string]any) int64 {
	if md == nil {
		return 0
	}
	switch v := md["run_seq"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// WatchProjectEvents starts a goroutine (idempotent — same
// projectDir is a no-op on the second call) that tails
// {projectDir}/.enju/events/live.jsonl and stops any bots
// eligible for auto-stop when terminal events for runs in
// their AutoRunIDs lists arrive.
//
// projectID is the coord-side project id; passed in so the
// tailer can scope its pid-file walk by project (run seqs are
// per-project, not globally unique). projectDir is the
// fat-client clone path that notify writes live.jsonl into.
//
// The goroutine lives for the supervisor's lifetime — there's
// no explicit Stop. Fat-client shutdown takes everything down
// in one move; the tailer's only resource is one file handle
// + a sleep loop.
func (s *Supervisor) WatchProjectEvents(ctx context.Context, projectDir string, projectID int64) {
	if projectDir == "" {
		s.logger().Warn("WatchProjectEvents: empty projectDir, skipping", "project_id", projectID)
		return
	}
	s.tailMu.Lock()
	if s.tailing == nil {
		s.tailing = make(map[string]bool)
	}
	if s.tailing[projectDir] {
		s.tailMu.Unlock()
		return
	}
	s.tailing[projectDir] = true
	s.tailMu.Unlock()

	// Capture initial offset SYNCHRONOUSLY before launching
	// the goroutine. Without this, events written between
	// WatchProjectEvents returning and the goroutine being
	// scheduled get skipped (the os.Stat inside the goroutine
	// would see the post-write size and seek past them).
	// Starting at EOF matches operator intent: auto_bots only
	// manages runs created with auto_bots=true, which are
	// future from this point.
	path := filepath.Join(projectDir, ".enju", "events", "live.jsonl")
	var offset int64
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}
	go s.runEventTailer(ctx, path, offset, projectID)
}

// runEventTailer is the per-project poll loop. Reads new
// lines from live.jsonl, dispatches terminal events to
// onRunTerminal. Survives missing-file (notify hasn't
// written the first line yet) by retrying; survives parse
// errors per-line.
func (s *Supervisor) runEventTailer(ctx context.Context, path string, offset int64, projectID int64) {
	const pollInterval = 200 * time.Millisecond

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		next, err := s.consumeEventTail(path, offset, projectID)
		if err != nil {
			s.logger().Warn("auto_bots event tailer: read failed", "path", path, "error", err)
		}
		offset = next
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// consumeEventTail reads from offset to EOF, dispatches each
// terminal event to onRunTerminal, and returns the new
// offset (EOF). On a missing file (notify hasn't started yet
// for this project) returns offset unchanged. Errors other
// than not-exist propagate so the caller can log.
func (s *Supervisor) consumeEventTail(path string, offset int64, projectID int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return offset, nil
		}
		return offset, err
	}
	defer f.Close()
	// File may have been truncated by an external tool. If
	// the current size is smaller than our saved offset, start
	// over from zero rather than seeking past EOF.
	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if info.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, fmt.Errorf("seek: %w", err)
	}
	scanner := bufio.NewScanner(f)
	// notify writes one event per JSONL line; bump the buffer
	// to handle large metadata blobs (terminate's
	// abandoned_claims list can be substantial).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var newOffset = offset
	for scanner.Scan() {
		line := scanner.Bytes()
		newOffset += int64(len(line)) + 1 // \n
		var ev tailEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			s.logger().Warn("auto_bots tailer: malformed jsonl line, skipping", "error", err)
			continue
		}
		if !runTerminalTypes[ev.Type] {
			continue
		}
		runSeq := runSeqFromMetadata(ev.Metadata)
		if runSeq == 0 {
			s.logger().Warn("auto_bots tailer: terminal event has no run_seq metadata",
				"type", ev.Type, "seq", ev.Seq)
			continue
		}
		s.onRunTerminal(projectID, runSeq, ev.Type)
	}
	if err := scanner.Err(); err != nil {
		return newOffset, fmt.Errorf("scan: %w", err)
	}
	return newOffset, nil
}

// onRunTerminal walks every pid file in PIDDir and, for each
// entry whose ProjectID matches AND whose AutoRunIDs contains
// runSeq, removes the seq and stops the bot if it's now
// eligible for auto-stop.
//
// Best-effort: errors on a single bot don't abort the rest
// (one mangled pid file shouldn't keep the fleet running).
func (s *Supervisor) onRunTerminal(projectID, runSeq int64, eventType string) {
	entries, err := os.ReadDir(s.PIDDir)
	if err != nil {
		s.logger().Warn("auto_bots: reading pid dir", "error", err)
		return
	}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.PIDDir, ent.Name())
		entry, err := readPIDFile(path)
		if err != nil {
			continue
		}
		if entry.ProjectID != projectID {
			continue
		}
		if !slices.Contains(entry.AutoRunIDs, runSeq) {
			continue
		}
		if err := s.UnmarkAutoRun(entry.Name, runSeq); err != nil {
			s.logger().Warn("auto_bots: unmark failed", "bot", entry.Name, "run_seq", runSeq, "error", err)
			continue
		}
		eligible, err := s.EligibleForAutoStop(entry.Name)
		if err != nil {
			s.logger().Warn("auto_bots: eligibility check failed", "bot", entry.Name, "error", err)
			continue
		}
		if !eligible {
			continue
		}
		s.logger().Info("auto_bots: stopping bot — last referencing run completed",
			"bot", entry.Name, "run_seq", runSeq, "event", eventType)
		if _, err := s.Stop(context.Background(), entry.Name); err != nil {
			s.logger().Warn("auto_bots: stop failed", "bot", entry.Name, "error", err)
		}
	}
}

