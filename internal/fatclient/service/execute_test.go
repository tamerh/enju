package service

// Tests for the service-layer compute helpers moved out of
// internal/fatclient/mcphandlers when the execute path was
// ported. Covers the small pure functions
// (stringSliceNonNil + encodeParamEnv) plus the claim retry
// loop that earlier lived as apiClient.claimWithTransientRetry.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

func TestStringSliceNonNil(t *testing.T) {
	if got := stringSliceNonNil(nil); got == nil {
		t.Fatal("nil input should become empty slice")
	} else if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}

	in := []string{"a", "b"}
	got := stringSliceNonNil(in)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("non-nil input should round-trip, got %v", got)
	}
}

func TestEncodeParamEnv(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, ""},
		{"string", "hello", "hello"},
		{"bool-true", true, "true"},
		{"bool-false", false, "false"},
		{"int-via-float64", float64(42), "42"},
		{"negative-int", float64(-7), "-7"},
		{"real-float", 3.14, "3.14"},
		{"list-of-strings", []interface{}{"a", "b", "c"}, "a,b,c"},
		{"list-of-ints", []interface{}{float64(1), float64(2)}, "1,2"},
		{"empty-list", []interface{}{}, ""},
		{"nested-list", []interface{}{"a", []interface{}{"x", "y"}}, "a,x,y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeParamEnv(tc.in)
			if got != tc.want {
				t.Errorf("encodeParamEnv(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEncodeParamEnvMapFallback covers the JSON fallback branch
// for unexpected types (nested structs / maps). The exact
// rendering is not load-bearing — we just need a non-empty
// representation so scripts see something rather than Go's
// default "map[...]".
func TestEncodeParamEnvMapFallback(t *testing.T) {
	got := encodeParamEnv(map[string]interface{}{"k": "v"})
	if got == "" {
		t.Fatal("expected non-empty fallback representation")
	}
	if !strings.Contains(got, "v") {
		t.Errorf("expected value in rendering, got %q", got)
	}
}

// TestClaimTransientRetryRecovers is the regression for the
// claim-with-retry path added after the SQLITE_BUSY parallel-
// claims bug. The store layer's _txlock=immediate normally
// prevents this, but the claim-time retry is defense-in-depth
// for any other transient HTTP/network blip the coordinator
// might surface (5xx during restart, reconcile race, etc.).
//
// The stub coordinator returns a transient SQLITE_BUSY error
// on the first claim attempt, then succeeds on the second.
// claimWithTransientRetry must mask the first attempt and
// surface only the success.
func TestClaimTransientRetryRecovers(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks/1:1:t/claim", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"error":"set_claim: database is locked (5) (SQLITE_BUSY)"}`))
			return
		}
		_, _ = w.Write([]byte(`{"task":{"id":"1:1:t","action":"compute"},"deadline":"2026-04-30T00:00:00Z"}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:   logger,
		}),
		Logger: logger,
	})

	if _, err := sess.claimWithTransientRetry(context.Background(), "1:1:t"); err != nil {
		t.Fatalf("claimWithTransientRetry: %v (expected success after one transient retry)", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("expected 2 coordinator calls (1 transient + 1 success), got %d", got)
	}
}

// TestClaimTransientRetrySkipsSubstantiveErrors verifies the
// retry logic does NOT swallow real claim refusals. A "task
// not in claimable state" or role-mismatch error is the
// coordinator's deterministic verdict; retrying would just
// burn time and produce the same refusal. The retry path must
// surface immediately on substantive errors.
func TestClaimTransientRetrySkipsSubstantiveErrors(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks/1:1:t/claim", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"task is not in claimable state"}`))
	})
	mux.HandleFunc("/api/v1/citizens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL:  ts.URL,
			Username: "tester",
			Logger:   logger,
		}),
		Logger: logger,
	})

	_, err := sess.claimWithTransientRetry(context.Background(), "1:1:t")
	if err == nil {
		t.Fatalf("claimWithTransientRetry: expected error for non-claimable task, got nil")
	}
	if !strings.Contains(err.Error(), "not in claimable state") {
		t.Errorf("expected substantive error to surface, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected 1 coordinator call (substantive error, no retry), got %d", got)
	}
}

// TestBuildComputeEnvHonorsTaskScratchDir pins the Phase 2.1
// contract: when buildComputeEnv is given a non-empty
// taskScratchDir, the resulting env exports ENJU_TASK_DIR with
// that value; an empty taskScratchDir suppresses the var.
//
// This is the boundary test for the env-vs-Spec split: the
// wrapper's own scratch lifecycle is exercised in
// internal/fatclient/compute/wrapper_test.go; here we just
// verify the service-side caller composes the env the wrapper
// expects.
func TestBuildComputeEnvHonorsTaskScratchDir(t *testing.T) {
	meta := &TaskMeta{}

	envWith := buildComputeEnv("1:1:fetch",
		"/some/work", "enju/runs/1/fetch", "", "", "",
		"/scratch/abc-iter-1", meta)
	if !containsEnv(envWith, "ENJU_TASK_DIR=/scratch/abc-iter-1") {
		t.Errorf("ENJU_TASK_DIR missing or wrong: %v", envWith)
	}

	envWithout := buildComputeEnv("1:1:fetch",
		"/some/work", "enju/runs/1/fetch", "", "", "",
		"", meta)
	for _, e := range envWithout {
		if strings.HasPrefix(e, "ENJU_TASK_DIR=") {
			t.Errorf("ENJU_TASK_DIR should be suppressed when scratch dir is empty, got: %v", e)
		}
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
