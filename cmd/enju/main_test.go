package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// isolateHome points HOME at a temp dir so credentialsPath() writes
// into a sandbox. Returns the resolved credentials.json path.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return filepath.Join(dir, ".enju", "credentials.json")
}

func TestSaveAndLoadCredentialsRoundTrip(t *testing.T) {
	isolateHome(t)

	saveCredentials("http://coord:8000", "alice", "Alice A.", "alice@example.com", "tok-xyz")

	got := loadCredentials("http://coord:8000")
	if got == nil {
		t.Fatal("expected credentials, got nil")
	}
	if got.Username != "alice" || got.Name != "Alice A." || got.Email != "alice@example.com" || got.Token != "tok-xyz" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestLoadCredentialsMissingFileReturnsNil(t *testing.T) {
	isolateHome(t)
	if got := loadCredentials("http://coord:8000"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestLoadCredentialsCoordinatorMismatchReturnsNil(t *testing.T) {
	isolateHome(t)
	saveCredentials("http://first:8000", "alice", "Alice", "", "tok")

	if got := loadCredentials("http://different:9000"); got != nil {
		t.Fatalf("expected nil for mismatched coordinator, got %+v", got)
	}
}

// TestPeekCredentialsFile pins the gate used by the bot
// daemon's self-heal step (TP53 Bug 4). Unlike loadCredentialsAt,
// peek returns true on coordinator-URL mismatch — the file is
// PRESENT and parseable, just for a different coord. That's an
// operator-config issue, not a needs-registering scenario, so
// self-heal must NOT fire and produce the alarming
// "self-heal: registering bot ... self-heal failed: 409"
// noise.
func TestPeekCredentialsFile(t *testing.T) {
	dir := t.TempDir()

	// Absent file → false.
	if peekCredentialsFile(filepath.Join(dir, "missing.json")) {
		t.Error("absent file: want false, got true")
	}

	// Unparseable JSON → false.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if peekCredentialsFile(bad) {
		t.Error("unparseable JSON: want false, got true")
	}

	// Empty username → false.
	emptyUser := filepath.Join(dir, "empty-user.json")
	if err := os.WriteFile(emptyUser, []byte(`{"coordinator":"http://x","token":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if peekCredentialsFile(emptyUser) {
		t.Error("empty username: want false, got true")
	}

	// Empty token → false. Token-less creds can't authenticate,
	// so the daemon must re-register (the existing behavior).
	emptyToken := filepath.Join(dir, "empty-token.json")
	if err := os.WriteFile(emptyToken, []byte(`{"coordinator":"http://x","username":"alice"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if peekCredentialsFile(emptyToken) {
		t.Error("empty token: want false, got true")
	}

	// Full and valid → true.
	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte(`{"coordinator":"http://x","username":"alice","token":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !peekCredentialsFile(ok) {
		t.Error("valid creds: want true, got false")
	}

	// Coordinator URL mismatch is intentionally NOT a fail
	// here — that's the key behavioral difference vs
	// loadCredentialsAt. peek answers "do creds exist on disk"
	// not "do they match the running coord". This is what
	// prevents TP53 Bug 4's 409 noise loop.
	mismatched := filepath.Join(dir, "mismatched.json")
	if err := os.WriteFile(mismatched, []byte(`{"coordinator":"http://other:9000","username":"alice","token":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !peekCredentialsFile(mismatched) {
		t.Error("coord-mismatched but parseable creds: peek should return true (lets caller surface a clearer error than triggering 409 self-heal)")
	}
}

// TestLoadCredentialsLocalSentinelMigratesToFallbackURL pins
// the one-shot migration for legacy credentials.json files
// that still carry the "local" sentinel from before --local
// mode pinned its port. On load, the sentinel is rewritten to
// the fallback URL (the pinned --local port) and persisted so
// subsequent loads see the real URL and never trigger the
// sentinel branch again.
func TestLoadCredentialsLocalSentinelMigratesToFallbackURL(t *testing.T) {
	path := isolateHome(t)
	// Simulate the legacy file shape.
	saveCredentials("local", "tamer", "Tamer", "tamer@example.com", "tok-tamer")

	// Caller asks for the fallback URL (which `enju mcp --local`
	// now uses verbatim). Migration must kick in and the
	// matching record must load.
	got := loadCredentials(fallbackCoordinatorURL)
	if got == nil {
		t.Fatal("expected creds to migrate + match fallback URL; got nil")
	}
	if got.Username != "tamer" || got.Token != "tok-tamer" {
		t.Errorf("identity not preserved: %+v", got)
	}
	if got.Coordinator != fallbackCoordinatorURL {
		t.Errorf("coordinator not migrated to fallback URL: %q", got.Coordinator)
	}

	// Check the migration persisted: re-read the file directly.
	raw, _ := os.ReadFile(path)
	var onDisk struct {
		Coordinator string `json:"coordinator"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("re-read credentials: %v", err)
	}
	if onDisk.Coordinator != fallbackCoordinatorURL {
		t.Errorf("on-disk coordinator after migration = %q, want %q",
			onDisk.Coordinator, fallbackCoordinatorURL)
	}
}

func TestLoadCredentialsMalformedJSONReturnsNil(t *testing.T) {
	path := isolateHome(t)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not valid json"), 0600); err != nil {
		t.Fatal(err)
	}

	if got := loadCredentials("http://coord:8000"); got != nil {
		t.Fatalf("expected nil for malformed json, got %+v", got)
	}
}

// TestSaveCredentialsPreservesUnknownFields covers the read-modify-write
// contract: a hand-added key (or a key from a newer enju version) must
// not be wiped when an older client saves its typed subset.
func TestSaveCredentialsPreservesUnknownFields(t *testing.T) {
	path := isolateHome(t)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}

	// Seed a credentials file with a field the Go struct doesn't know about.
	seed := map[string]any{
		"coordinator":  "http://coord:8000",
		"username":     "alice",
		"name":         "Alice",
		"future_field": "keep me",
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	// Now save — this triggers the read-modify-write merge.
	saveCredentials("http://coord:8000", "alice", "Alice Updated", "", "")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["future_field"] != "keep me" {
		t.Fatalf("unknown field dropped: got %v", out["future_field"])
	}
	if out["name"] != "Alice Updated" {
		t.Fatalf("expected updated name, got %v", out["name"])
	}
}

// TestSaveCredentialsEmptyTokenDoesNotClear — the save path skips
// token/email when they're empty so a partial update (e.g. name
// change) doesn't wipe the auth token from a prior registration.
func TestSaveCredentialsEmptyTokenDoesNotClear(t *testing.T) {
	isolateHome(t)
	saveCredentials("http://coord:8000", "alice", "Alice", "alice@example.com", "tok-original")

	// Second call with empty token and email.
	saveCredentials("http://coord:8000", "alice", "Alice Renamed", "", "")

	got := loadCredentials("http://coord:8000")
	if got == nil {
		t.Fatal("expected credentials, got nil")
	}
	if got.Token != "tok-original" {
		t.Fatalf("token was overwritten: got %q", got.Token)
	}
	if got.Email != "alice@example.com" {
		t.Fatalf("email was overwritten: got %q", got.Email)
	}
	if got.Name != "Alice Renamed" {
		t.Fatalf("expected name updated, got %q", got.Name)
	}
}

// TestCredentialsAtCustomPathIsolatesIdentities exercises the
// --credentials override path: two MCP processes pointing at the
// same coordinator with different credentials files must keep
// their identities separate without HOME isolation.
func TestCredentialsAtCustomPathIsolatesIdentities(t *testing.T) {
	isolateHome(t) // also seeded so HOME-based default isn't accidentally used

	dir := t.TempDir()
	pathA := filepath.Join(dir, "bot-a.json")
	pathB := filepath.Join(dir, "bot-b.json")

	saveCredentialsAt("http://coord:8000", "bot-a", "Bot A", "", "tok-a", pathA)
	saveCredentialsAt("http://coord:8000", "bot-b", "Bot B", "", "tok-b", pathB)

	gotA := loadCredentialsAt("http://coord:8000", pathA)
	gotB := loadCredentialsAt("http://coord:8000", pathB)
	if gotA == nil || gotB == nil {
		t.Fatalf("expected both creds, got A=%+v B=%+v", gotA, gotB)
	}
	if gotA.Username != "bot-a" || gotA.Token != "tok-a" {
		t.Fatalf("A mismatch: %+v", gotA)
	}
	if gotB.Username != "bot-b" || gotB.Token != "tok-b" {
		t.Fatalf("B mismatch: %+v", gotB)
	}

	// Default-path load (HOME-based) must NOT see either —
	// override files are not blended with the default location.
	if def := loadCredentials("http://coord:8000"); def != nil {
		t.Fatalf("default path leaked override creds: %+v", def)
	}
}

func TestResolveCredentialsPathOverride(t *testing.T) {
	isolateHome(t)
	if got := resolveCredentialsPath(""); got != credentialsPath() {
		t.Fatalf("empty override should fall back to default, got %q", got)
	}
	if got := resolveCredentialsPath("/tmp/explicit.json"); got != "/tmp/explicit.json" {
		t.Fatalf("explicit path not honored: %q", got)
	}
}

func TestRegisterCitizenSuccess(t *testing.T) {
	var gotBody map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/citizens/register" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"username": "alice",
			"token":    "tok-abc",
		})
	}))
	defer ts.Close()

	username, token, err := registerCitizen(ts.URL, "Alice A.", "", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if username != "alice" {
		t.Fatalf("expected username alice, got %q", username)
	}
	if token != "tok-abc" {
		t.Fatalf("expected token tok-abc, got %q", token)
	}
	if gotBody["name"] != "Alice A." || gotBody["email"] != "alice@example.com" {
		t.Fatalf("server did not receive expected body: %+v", gotBody)
	}
	if _, hasUsername := gotBody["username"]; hasUsername {
		t.Fatalf("username should be omitted when empty, got %+v", gotBody)
	}
}

func TestRegisterCitizenServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "username already taken",
		})
	}))
	defer ts.Close()

	_, _, err := registerCitizen(ts.URL, "Alice", "alice", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "username already taken" {
		t.Fatalf("expected server error passthrough, got %q", err.Error())
	}
}

func TestRegisterCitizenMissingUsernameInResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token": "tok-abc",
		})
	}))
	defer ts.Close()

	_, _, err := registerCitizen(ts.URL, "Alice", "", "")
	if err == nil {
		t.Fatal("expected error when server omits username, got nil")
	}
}

func TestRegisterCitizenNetworkError(t *testing.T) {
	// Port 1 is almost always unbound on Linux user accounts.
	_, _, err := registerCitizen("http://127.0.0.1:1", "Alice", "", "")
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
}
