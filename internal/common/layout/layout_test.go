package layout

import (
	"strings"
	"testing"
)

// TestBotCloneDirFor pins the per-bot clone path schema and
// the path-safety guards. The shape is what the daemon and
// the gitignore managed block both read against; a regression
// here would either co-locate two bots' clones (parallelism
// regression) or let a malicious manifest escape into the
// project tree.
func TestBotCloneDirFor(t *testing.T) {
	cases := []struct {
		name        string
		botUsername string
		wantPath    string
		wantErr     string
	}{
		{
			name:        "happy path",
			botUsername: "developer-bot",
			wantPath:    "enju/bots/developer-bot/clone",
		},
		{
			name:        "alphanumerics + dashes ok",
			botUsername: "reviewer-bot-1",
			wantPath:    "enju/bots/reviewer-bot-1/clone",
		},
		{
			name:        "empty rejected",
			botUsername: "",
			wantErr:     "required",
		},
		{
			name:        "forward slash rejected",
			botUsername: "evil/escape",
			wantErr:     "path separator",
		},
		{
			name:        "backslash rejected",
			botUsername: `evil\escape`,
			wantErr:     "path separator",
		},
		{
			name:        "dot rejected",
			botUsername: ".",
			wantErr:     "path traversal",
		},
		{
			name:        "dotdot rejected",
			botUsername: "..",
			wantErr:     "path traversal",
		},
		{
			name:        "embedded dotdot rejected",
			botUsername: "a..b",
			wantErr:     "path traversal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BotCloneDirFor(tc.botUsername)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (path=%q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q should contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantPath {
				t.Errorf("path: got %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// TestBotCloneDirFor_DistinctBotsGetDistinctPaths is the
// load-bearing parallelism property: two bots on the same
// project must NEVER share a clone path. This pins it.
func TestBotCloneDirFor_DistinctBotsGetDistinctPaths(t *testing.T) {
	a, err := BotCloneDirFor("alice-bot")
	if err != nil {
		t.Fatal(err)
	}
	b, err := BotCloneDirFor("bob-bot")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("distinct bots produced same path: %q", a)
	}
}
