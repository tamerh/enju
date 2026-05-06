package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/bots"
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

func TestAddBotToProject_HappyPath(t *testing.T) {
	var got struct {
		path string
		body map[string]string
		auth string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"username":"reviewer-bot","role":"member"}`))
	}))
	defer srv.Close()

	err := addBotToProject(context.Background(), srv.URL, "owner-token", 42, "reviewer-bot")
	if err != nil {
		t.Fatalf("addBotToProject: %v", err)
	}
	if got.path != "/api/v1/projects/42/members" {
		t.Errorf("path: got %q", got.path)
	}
	if got.body["username"] != "reviewer-bot" || got.body["role"] != "member" {
		t.Errorf("body: %+v", got.body)
	}
	if got.auth != "Bearer owner-token" {
		t.Errorf("auth header: got %q", got.auth)
	}
}

func TestAddBotToProject_AlreadyMemberIsSuccess(t *testing.T) {
	// Re-running setup against the same project shouldn't fail
	// at the membership step. Both 409 and "already a member"
	// substring forms are treated as no-op success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already a member"}`))
	}))
	defer srv.Close()

	if err := addBotToProject(context.Background(), srv.URL, "owner-token", 42, "x"); err != nil {
		t.Errorf("already-a-member should be treated as success, got: %v", err)
	}
}

func TestAddBotToProject_RealError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"only project owners can add members"}`))
	}))
	defer srv.Close()

	err := addBotToProject(context.Background(), srv.URL, "owner-token", 42, "x")
	if err == nil {
		t.Fatal("expected error from 403 forbidden response")
	}
	if !strings.Contains(err.Error(), "only project owners") {
		t.Errorf("error should carry coord message, got: %v", err)
	}
}

// TestEnsureBotMembership_HappyPathPostsToMembersEndpoint pins
// the regression: even when bot creds already exist locally,
// `enju bot run` MUST call addBotToProject on every startup so
// a bot starting against a fresh project becomes a member
// without the operator having to enju_add_project_member by
// hand. The behavior was broken when membership-add lived only
// inside the "creds missing → register" branch.
func TestEnsureBotMembership_HappyPathPostsToMembersEndpoint(t *testing.T) {
	var seen struct {
		path  string
		body  map[string]string
		auth  string
		count int
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.count++
		seen.path = r.URL.Path
		seen.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&seen.body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"username":"developer-bot","role":"member"}`))
	}))
	defer srv.Close()

	var stderr bytes.Buffer
	ensureBotMembership(context.Background(), srv.URL, "owner-token", 7, "developer-bot", &stderr)

	if seen.count != 1 {
		t.Fatalf("expected exactly 1 membership POST, got %d", seen.count)
	}
	if seen.path != "/api/v1/projects/7/members" {
		t.Errorf("path: got %q", seen.path)
	}
	if seen.body["username"] != "developer-bot" {
		t.Errorf("body username: got %q", seen.body["username"])
	}
	if seen.auth != "Bearer owner-token" {
		t.Errorf("auth: got %q", seen.auth)
	}
	if stderr.Len() != 0 {
		t.Errorf("happy path should produce no stderr output; got %q", stderr.String())
	}
}

// TestEnsureBotMembership_AlreadyMemberSilent pins idempotency:
// re-running `enju bot run` for a bot that's already a member
// must not log noise. The coord's "already a member" response
// is treated as success by addBotToProject, and ensureBotMembership
// stays silent.
func TestEnsureBotMembership_AlreadyMemberSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already a member"}`))
	}))
	defer srv.Close()

	var stderr bytes.Buffer
	ensureBotMembership(context.Background(), srv.URL, "owner-token", 7, "developer-bot", &stderr)
	if stderr.Len() != 0 {
		t.Errorf("already-member path should be silent; got %q", stderr.String())
	}
}

// TestEnsureBotMembership_LogsButDoesNotErrorOnCoordFailure
// pins fail-soft behavior: a real coord error (403, 5xx, etc.)
// is logged to stderr with the bot+project pair so the operator
// knows what to fix, but ensureBotMembership returns without
// error so the daemon still gets to start. The poll loop's
// "not a member" error is the louder, more actionable failure
// surface; we'd rather see the bot try than block startup on a
// transient coord blip.
func TestEnsureBotMembership_LogsButDoesNotErrorOnCoordFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"only project owners can add members"}`))
	}))
	defer srv.Close()

	var stderr bytes.Buffer
	// Should not panic, should not block.
	ensureBotMembership(context.Background(), srv.URL, "owner-token", 7, "tester-bot", &stderr)

	out := stderr.String()
	if !strings.Contains(out, "tester-bot") {
		t.Errorf("stderr should name the bot for actionable triage; got %q", out)
	}
	if !strings.Contains(out, "7") {
		t.Errorf("stderr should name the project id; got %q", out)
	}
	if !strings.Contains(out, "only project owners") {
		t.Errorf("stderr should bubble up the coord's reason; got %q", out)
	}
	if !strings.Contains(out, "first poll") {
		t.Errorf("stderr should hint at the surfacing failure mode; got %q", out)
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

func TestWatchStdinEOF_TriggersOnEOF(t *testing.T) {
	// bytes.Reader hits EOF immediately (zero-length input).
	// watchStdinEOF should call cancel and return.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchStdinEOF(bytes.NewReader(nil), cancel)
	select {
	case <-ctx.Done():
		// Shutdown triggered as expected.
	case <-time.After(1 * time.Second):
		t.Fatal("watchStdinEOF didn't cancel ctx within 1s of EOF")
	}
}

func TestWatchStdinEOF_DiscardsBytesUntilEOF(t *testing.T) {
	// Supervisor pre-sends a few bytes (heartbeat, junk, etc.)
	// before closing. The watcher should ignore the bytes and
	// only react to EOF.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchStdinEOF(bytes.NewReader([]byte("ignore me\n")), cancel)
	select {
	case <-ctx.Done():
		// EOF reached after the bytes were drained.
	case <-time.After(1 * time.Second):
		t.Fatal("watchStdinEOF didn't cancel ctx after draining input")
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
