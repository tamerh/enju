package mcphandlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractErrorString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"present", `{"error":"boom"}`, "boom"},
		{"absent", `{"ok":true}`, ""},
		{"non-string-error", `{"error":{"code":1}}`, ""},
		{"malformed", `not json`, ""},
		{"empty-object", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorString([]byte(tc.in)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSortStringsStable(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"c", "a", "b"}, []string{"a", "b", "c"}},
		{[]string{}, []string{}},
		{[]string{"x"}, []string{"x"}},
		{[]string{"b", "a"}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		xs := append([]string(nil), tc.in...)
		sortStringsStable(xs)
		if strings.Join(xs, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("sortStringsStable(%v) = %v, want %v", tc.in, xs, tc.want)
		}
	}
}

func TestIndexOfNewline(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"no newline here", -1},
		{"line1\nline2", 5},
		{"\nleading", 0},
		{"", -1},
	}
	for _, tc := range cases {
		if got := indexOfNewline(tc.in); got != tc.want {
			t.Fatalf("indexOfNewline(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseTaskCommitMessage(t *testing.T) {
	cases := []struct {
		name        string
		msg         string
		wantTaskID  string
		wantOwner   string
	}{
		{
			name:       "plain subject",
			msg:        "Task 1:2:step by @alice: wrote the thing",
			wantTaskID: "1:2:step",
			wantOwner:  "alice",
		},
		{
			name:       "subject plus body",
			msg:        "Task xyz by @bob: hi\n\nlong body\nwith lines",
			wantTaskID: "xyz",
			wantOwner:  "bob",
		},
		{
			name:       "non-task commit",
			msg:        "Initial commit",
			wantTaskID: "",
			wantOwner:  "",
		},
		{
			name:       "missing @",
			msg:        "Task xyz by alice: thing",
			wantTaskID: "",
			wantOwner:  "",
		},
		{
			name:       "missing colon",
			msg:        "Task xyz by @alice does a thing",
			wantTaskID: "",
			wantOwner:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tid, owner := parseTaskCommitMessage(tc.msg)
			if tid != tc.wantTaskID || owner != tc.wantOwner {
				t.Fatalf("got (%q, %q), want (%q, %q)", tid, owner, tc.wantTaskID, tc.wantOwner)
			}
		})
	}
}

func TestFormatJSON(t *testing.T) {
	got := formatJSON([]byte(`{"a":1,"b":[2,3]}`))
	// Must have newline + indentation.
	if !strings.Contains(got, "\n") || !strings.Contains(got, "  ") {
		t.Fatalf("expected pretty output, got %q", got)
	}

	// Invalid input falls back to the raw string.
	raw := "not json"
	if got := formatJSON([]byte(raw)); got != raw {
		t.Fatalf("expected fallback to raw input, got %q", got)
	}
}

func TestUpdateLocalCredentialsPreservesUntouchedFields(t *testing.T) {
	// Isolate HOME so we don't stomp real credentials.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := filepath.Join(dir, ".enju", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}

	seed := map[string]any{
		"coordinator":  "http://coord:8000",
		"username":     "alice",
		"name":         "Old Name",
		"email":        "old@example.com",
		"token":        "tok-keep",
		"future_field": "dont-touch",
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	// Update name only — email/token/unknown field must survive.
	updateLocalCredentials(true, "New Name", false, "")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["name"] != "New Name" {
		t.Fatalf("expected name updated, got %v", out["name"])
	}
	if out["email"] != "old@example.com" {
		t.Fatalf("email was clobbered: got %v", out["email"])
	}
	if out["token"] != "tok-keep" {
		t.Fatalf("token was clobbered: got %v", out["token"])
	}
	if out["future_field"] != "dont-touch" {
		t.Fatalf("unknown field dropped: got %v", out["future_field"])
	}
}

func TestUpdateLocalCredentialsSettingEmail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := filepath.Join(dir, ".enju", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{"username": "alice", "name": "Alice", "email": ""}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	updateLocalCredentials(false, "", true, "new@example.com")

	raw, _ := os.ReadFile(path)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["email"] != "new@example.com" {
		t.Fatalf("expected email set, got %v", out["email"])
	}
	if out["name"] != "Alice" {
		t.Fatalf("name should be untouched, got %v", out["name"])
	}
}

func TestUpdateLocalCredentialsMissingFileNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// File doesn't exist. Must not create it or panic.
	updateLocalCredentials(true, "Name", true, "e@x")

	path := filepath.Join(dir, ".enju", "credentials.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file created; got err=%v", err)
	}
}
