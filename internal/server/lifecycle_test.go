package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func testServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv, err := New(cfg, log, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func dialable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func waitClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !dialable(addr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s is still accepting connections", addr)
}

func TestShutdownClosesSSHListener(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.SSHAddr = "127.0.0.1:0"

	srv := testServer(t, cfg)
	sshAddr := srv.SSHAddr()
	if sshAddr == "" {
		t.Fatal("SSH listener was not started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	waitFor(t, func() bool { return dialable(sshAddr) })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	waitClosed(t, sshAddr)
}

// Regression: an HTTP failure used to return from Run without touching the SSH
// listener, leaving it accepting connections for a server that had already
// given up.
func TestHTTPFailureAlsoClosesSSHListener(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.SSHAddr = "127.0.0.1:0"

	srv := testServer(t, cfg)
	sshAddr := srv.SSHAddr()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	waitFor(t, func() bool { return dialable(sshAddr) })

	// Break the HTTP listener out from under Serve.
	srv.ln.Close()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the HTTP listener failed")
	}
	waitClosed(t, sshAddr)
}

func TestSSHDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"

	srv := testServer(t, cfg)
	if got := srv.SSHAddr(); got != "" {
		t.Fatalf("SSHAddr = %q, want no listener", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	waitFor(t, func() bool { return dialable(srv.Addr()) })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
