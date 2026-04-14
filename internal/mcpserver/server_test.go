package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAutoReregisterOnStaleCitizen verifies that an API call hitting
// a 404 "citizen not found" triggers a re-register + retry, and that
// the persisted credentials callback fires with the fresh handle.
func TestAutoReregisterOnStaleCitizen(t *testing.T) {
	var (
		firstCallServed   atomic.Bool
		registerCalls     atomic.Int32
		retryCallSucceeds atomic.Bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/citizens/by-username/alice", func(w http.ResponseWriter, r *http.Request) {
		if !firstCallServed.Load() {
			// First call: pretend the server forgot alice.
			firstCallServed.Store(true)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"citizen \"alice\" not found"}`))
			return
		}
		// Retry call: server now knows alice again.
		retryCallSucceeds.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"username":"alice","name":"Alice"}`))
	})
	mux.HandleFunc("/api/v1/citizens/register", func(w http.ResponseWriter, r *http.Request) {
		registerCalls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "alice" {
			t.Errorf("expected re-register to send username=alice, got %q", body["username"])
		}
		if body["name"] != "Alice" {
			t.Errorf("expected re-register to send name=Alice, got %q", body["name"])
		}
		if body["email"] != "alice@example.com" {
			t.Errorf("expected re-register to send email=alice@example.com, got %q", body["email"])
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"username":"alice","id":42}`))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	var savedUser, savedName, savedEmail string
	var saveCalls atomic.Int32
	c := &apiClient{
		baseURL:      ts.URL,
		username:     "alice",
		citizenName:  "Alice",
		citizenEmail: "alice@example.com",
		httpClient:   &http.Client{},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		saveCreds: func(u, n, e string) {
			savedUser = u
			savedName = n
			savedEmail = e
			saveCalls.Add(1)
		},
	}

	data, err := c.get(context.Background(), "/api/v1/citizens/by-username/alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if registerCalls.Load() != 1 {
		t.Errorf("expected exactly 1 register call, got %d", registerCalls.Load())
	}
	if !retryCallSucceeds.Load() {
		t.Error("expected the retry call to succeed against the refreshed coordinator")
	}
	if !strings.Contains(string(data), `"username":"alice"`) {
		t.Errorf("expected retry response body, got: %s", data)
	}
	if saveCalls.Load() != 1 || savedUser != "alice" || savedName != "Alice" || savedEmail != "alice@example.com" {
		t.Errorf("expected SaveCredentials(alice, Alice, alice@example.com) once, got %d calls with (%q, %q, %q)",
			saveCalls.Load(), savedUser, savedName, savedEmail)
	}
}

// TestStaleCitizenWithoutNameGivesUp verifies that when CitizenName
// is empty the client returns the original 404 body unchanged
// instead of silently swallowing it.
func TestStaleCitizenWithoutNameGivesUp(t *testing.T) {
	var registerCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/citizens/by-username/alice", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"citizen not found"}`))
	})
	mux.HandleFunc("/api/v1/citizens/register", func(w http.ResponseWriter, r *http.Request) {
		registerCalls.Add(1)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := &apiClient{
		baseURL:    ts.URL,
		username:   "alice",
		// citizenName intentionally empty
		httpClient: &http.Client{},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	data, err := c.get(context.Background(), "/api/v1/citizens/by-username/alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if registerCalls.Load() != 0 {
		t.Errorf("expected no register calls when CitizenName is empty, got %d", registerCalls.Load())
	}
	if !strings.Contains(string(data), "citizen not found") {
		t.Errorf("expected original error body to pass through, got: %s", data)
	}
}

// TestStaleCitizenDetection covers the status/body classifier.
func TestStaleCitizenDetection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"quoted form", http.StatusNotFound, `{"error":"citizen \"alice\" not found"}`, true},
		{"plain form", http.StatusNotFound, `{"error":"citizen not found"}`, true},
		{"404 other", http.StatusNotFound, `{"error":"project not found"}`, false},
		{"200 with phrase", http.StatusOK, `{"error":"citizen not found"}`, false},
		{"500 with phrase", http.StatusInternalServerError, `{"error":"citizen not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isStaleCitizenResponse(tc.status, []byte(tc.body))
			if got != tc.want {
				t.Errorf("isStaleCitizenResponse(%d, %q) = %v, want %v",
					tc.status, tc.body, got, tc.want)
			}
		})
	}
}
