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

func TestCreateSessionSurfacesRelayErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate_limited","message":"slow down"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).CreateSession(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	// The operator needs to see what the relay actually said.
	if !strings.Contains(err.Error(), "rate_limited") || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("error lost the relay's reason: %v", err)
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
