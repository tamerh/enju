package coord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

// TestGetStatus_SurfacesHTTPStatus pins the contract REV.3
// relies on: GetStatus returns the response's HTTP status
// without folding it into the data return. The supervisor's
// reconcile uses this to distinguish 404 (run gone, treat as
// terminal) from 200 (parse the body for state). Without it,
// the alternative was string-matching the error body for "404"
// — brittle to coord error-format changes.
func TestGetStatus_SurfacesHTTPStatus(t *testing.T) {
	cases := []struct {
		name           string
		serverStatus   int
		serverBody     string
		wantStatus     int
		wantBodyPrefix string
	}{
		{"200 OK", http.StatusOK, `{"seq":5,"state":"completed"}`, 200, `{"seq":5`},
		{"404 not found", http.StatusNotFound, `{"error":"run not found"}`, 404, `{"error"`},
		{"500 server error", http.StatusInternalServerError, `oops`, 500, `oops`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.serverStatus)
				_, _ = w.Write([]byte(tc.serverBody))
			}))
			defer srv.Close()
			c := New(Config{BaseURL: srv.URL, Username: "u", CitizenName: "u", AuthToken: "tok"})
			data, status, err := c.GetStatus(context.Background(), "/anything")
			if err != nil {
				t.Fatalf("GetStatus: %v", err)
			}
			if status != tc.wantStatus {
				t.Errorf("status: want %d, got %d", tc.wantStatus, status)
			}
			if got := string(data); got[:len(tc.wantBodyPrefix)] != tc.wantBodyPrefix {
				t.Errorf("body prefix: want %q, got %q", tc.wantBodyPrefix, got)
			}
		})
	}
}
