package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"session_id":"abc","host_token":"h","guest_token":"g","expires_in_seconds":1800}`))
	}))
	defer srv.Close()

	s, err := NewClient(srv.URL).CreateSession(context.Background())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.SessionID != "abc" || s.HostToken != "h" || s.GuestToken != "g" {
		t.Fatalf("got %+v", s)
	}
}

// An unrecognised refusal must carry the relay's own words: it is the only
// clue anyone has.
func TestCreateSessionSurfacesUnknownRelayErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte(`{"error":"weird_failure","message":"something specific"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).CreateSession(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "weird_failure") || !strings.Contains(err.Error(), "something specific") {
		t.Fatalf("error lost the relay's reason: %v", err)
	}
}

// The refusals a person actually meets get an explanation of what to do about
// them, because the relay's own wording does not name the missing setting.
func TestCreateSessionExplainsCommonRefusals(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
	}{
		{"needs a token", http.StatusUnauthorized, EnvRelayToken},
		{"rate limited", http.StatusTooManyRequests, "rate limiting"},
		{"at capacity", http.StatusServiceUnavailable, "as many sessions as it allows"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"error":"x"}`))
			}))
			defer srv.Close()

			_, err := NewClient(srv.URL).CreateSession(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, should mention %q", err, tc.want)
			}
		})
	}
}

// A private relay's secret travels in a header, and never on a command line.
func TestCreateSessionSendsRelayToken(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"session_id":"a","host_token":"h","guest_token":"g","viewer_token":"v"}`))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL).WithToken("s3cret").CreateSession(context.Background()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if seen != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", seen)
	}
}

func TestCreateSessionRejectsIncompleteResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"session_id":"abc"}`)) // no tokens
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL).CreateSession(context.Background()); err == nil {
		t.Fatal("expected an error for a session with no host token")
	}
}

func TestTunnelURL(t *testing.T) {
	// A trailing slash on the configured server must not double up.
	c := NewClient("http://localhost:8080/")
	if got, want := c.TunnelURL(), "http://localhost:8080/api/v1/tunnel"; got != want {
		t.Fatalf("TunnelURL = %q, want %q", got, want)
	}
}
