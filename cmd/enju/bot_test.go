package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/bots"
)

// fakeCoord stands in for the coordinator's bot-registration
// endpoint. Records every request so tests can assert what was
// called with what payload — most setup-test failures come from
// "we sent the wrong body" rather than transport bugs.
type fakeCoord struct {
	t            *testing.T
	mu           map[string]int // requested-username -> times seen
	failNext     bool
	wantOwnerTok string
}

func newFakeCoord(t *testing.T, ownerToken string) (*httptest.Server, *fakeCoord) {
	fc := &fakeCoord{t: t, mu: map[string]int{}, wantOwnerTok: ownerToken}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/citizens/me/bots" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		got := r.Header.Get("Authorization")
		if got != "Bearer "+fc.wantOwnerTok {
			t.Errorf("Authorization header: got %q, want Bearer %s", got, fc.wantOwnerTok)
			http.Error(w, "unauthorized", 401)
			return
		}
		var body struct{ Name, Username string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "" {
			t.Error("registration request missing name")
		}
		fc.mu[body.Username]++
		if fc.failNext {
			fc.failNext = false
			http.Error(w, `{"error":"injected failure"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":    "tok-" + body.Username,
			"username": body.Username,
			"name":     body.Name,
		})
	}))
	return srv, fc
}

func TestRegisterBot_HappyPath(t *testing.T) {
	srv, fc := newFakeCoord(t, "owner-token")
	defer srv.Close()

	b := &bots.Bot{Name: "developer-bot"}
	tok, username, err := registerBot(context.Background(), srv.URL, "owner-token", b)
	if err != nil {
		t.Fatalf("registerBot: %v", err)
	}
	if tok != "tok-developer-bot" {
		t.Errorf("token: got %q", tok)
	}
	if username != "developer-bot" {
		t.Errorf("username: got %q", username)
	}
	if fc.mu["developer-bot"] != 1 {
		t.Errorf("expected 1 registration call for developer-bot, got %d", fc.mu["developer-bot"])
	}
}

func TestRegisterBot_CoordError(t *testing.T) {
	srv, fc := newFakeCoord(t, "owner-token")
	defer srv.Close()
	fc.failNext = true

	_, _, err := registerBot(context.Background(), srv.URL, "owner-token", &bots.Bot{Name: "x"})
	if err == nil {
		t.Fatal("expected error from failing coord, got nil")
	}
	if !strings.Contains(err.Error(), "injected failure") {
		t.Errorf("error should surface coord body, got: %v", err)
	}
}

func TestWriteBotCredentials_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds", "developer-bot.json")
	if err := writeBotCredentials(credsPath, "http://coord:8000", "developer-bot", "developer-bot", "tok-xyz"); err != nil {
		t.Fatalf("writeBotCredentials: %v", err)
	}
	// loadCredentialsAt is the same function the daemon will
	// use — round-trip through it to confirm format compatibility.
	got := loadCredentialsAt("http://coord:8000", credsPath)
	if got == nil {
		t.Fatal("loadCredentialsAt returned nil after writeBotCredentials")
	}
	if got.Token != "tok-xyz" || got.Username != "developer-bot" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// Permissions: 0600. Token files must not be world-readable.
	st, err := os.Stat(credsPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0600 {
		t.Errorf("credentials file mode: got %o, want 0600", mode)
	}
}

func TestWriteBotCredentials_TightensExistingParentDir(t *testing.T) {
	// MkdirAll's mode is "for newly created directories only" —
	// if the parent already exists at 0755 (created by some
	// earlier tool) the chmod-style mkdir is a no-op. We force
	// 0700 explicitly. Test: pre-create the parent at 0755,
	// confirm writeBotCredentials tightens it.
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "creds")
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		t.Fatal(err)
	}
	credsPath := filepath.Join(parentDir, "x.json")
	if err := writeBotCredentials(credsPath, "c", "u", "n", "t"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(parentDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0700 {
		t.Errorf("parent dir mode: got %o, want 0700 (tightened)", got)
	}
}

func TestWriteBotCredentials_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "x.json")
	if err := os.WriteFile(credsPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeBotCredentials(credsPath, "c", "u", "n", "t"); err == nil {
		t.Error("expected refusal on existing file, got nil")
	}
}
