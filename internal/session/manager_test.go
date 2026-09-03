package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock lets expiry be tested without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestCreateSession(t *testing.T) {
	clock := newFakeClock()
	m := NewManager(Options{TTL: 30 * time.Minute, Now: clock.Now})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !ValidID(s.SessionID) {
		t.Fatalf("Create produced an invalid session id: %q", s.SessionID)
	}
	if s.HostToken == "" || s.GuestToken == "" {
		t.Fatal("Create must populate both tokens")
	}
	if s.HostToken == s.GuestToken {
		t.Fatal("host and guest tokens must differ")
	}
	if s.HostToken == s.SessionID || s.GuestToken == s.SessionID {
		t.Fatal("tokens must not be derived from the public session id")
	}
	if !s.CreatedAt.Equal(clock.Now()) {
		t.Fatalf("CreatedAt = %v, want %v", s.CreatedAt, clock.Now())
	}
	if want := clock.Now().Add(30 * time.Minute); !s.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", s.ExpiresAt, want)
	}
	if n := m.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}

	got, err := m.Get(context.Background(), s.SessionID)
	if err != nil {
		t.Fatalf("Get after Create: %v", err)
	}
	if got.SessionID != s.SessionID {
		t.Fatalf("Get returned %q, want %q", got.SessionID, s.SessionID)
	}
}

func TestCreateUsesDefaultTTL(t *testing.T) {
	m := NewManager(Options{})
	defer m.Close()
	if m.TTL() != DefaultTTL {
		t.Fatalf("TTL = %s, want %s", m.TTL(), DefaultTTL)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := m.Get(context.Background(), s.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.HostToken = "tampered"

	again, err := m.Get(context.Background(), s.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.HostToken == "tampered" {
		t.Fatal("Get returned a pointer into stored state; callers can mutate it")
	}
}

func TestSessionExpiration(t *testing.T) {
	clock := newFakeClock()
	m := NewManager(Options{TTL: 30 * time.Minute, Now: clock.Now})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Just before the TTL the session is still live.
	clock.Advance(30*time.Minute - time.Second)
	if _, err := m.Get(context.Background(), s.SessionID); err != nil {
		t.Fatalf("Get just before expiry: %v", err)
	}

	// Exactly at the TTL it is gone: ExpiresAt is exclusive.
	clock.Advance(time.Second)
	if _, err := m.Get(context.Background(), s.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get at expiry = %v, want ErrNotFound", err)
	}

	// The failed lookup must also have evicted it.
	if n := m.Len(); n != 0 {
		t.Fatalf("Len after expired lookup = %d, want 0", n)
	}
}

func TestSweepExpired(t *testing.T) {
	clock := newFakeClock()
	m := NewManager(Options{TTL: time.Minute, Now: clock.Now})
	defer m.Close()

	for i := 0; i < 3; i++ {
		if _, err := m.Create(context.Background()); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	clock.Advance(30 * time.Second)
	live, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Past the first three TTLs but not the fourth.
	clock.Advance(31 * time.Second)
	if n := m.sweepExpired(); n != 3 {
		t.Fatalf("sweepExpired removed %d, want 3", n)
	}
	if n := m.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
	if _, err := m.Get(context.Background(), live.SessionID); err != nil {
		t.Fatalf("live session was swept: %v", err)
	}
}

func TestRunSweepsInBackground(t *testing.T) {
	clock := newFakeClock()
	m := NewManager(Options{
		TTL:           time.Minute,
		SweepInterval: time.Millisecond,
		Now:           clock.Now,
	})
	defer m.Close()

	if _, err := m.Create(context.Background()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	clock.Advance(2 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	deadline := time.After(2 * time.Second)
	for m.Len() != 0 {
		select {
		case <-deadline:
			t.Fatal("background sweeper did not remove the expired session")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestGetNonexistentSession(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	// A well-formed but unused id.
	unused, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}

	tests := []struct {
		name string
		id   string
		want error
	}{
		{"well-formed but unknown", unused, ErrNotFound},
		{"empty", "", ErrInvalidID},
		{"malformed", "not-a-session-id", ErrInvalidID},
		{"path traversal", "../../etc/passwd", ErrInvalidID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Get(context.Background(), tc.id)
			if got != nil {
				t.Fatalf("Get returned a session for %q", tc.id)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Get(%q) = %v, want %v", tc.id, err, tc.want)
			}
		})
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.Delete(s.SessionID)
	m.Delete(s.SessionID) // must not panic
	if _, err := m.Get(context.Background(), s.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestCreateRespectsCancelledContext(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Create(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create with cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestCreateAfterClose(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	m.Close()
	m.Close() // must be safe to call twice
	if _, err := m.Create(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Create after Close = %v, want ErrClosed", err)
	}
}

func TestConcurrentCreateAndGet(t *testing.T) {
	m := NewManager(Options{TTL: time.Minute})
	defer m.Close()

	const n = 50
	var wg sync.WaitGroup
	ids := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.Create(context.Background())
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			ids <- s.SessionID
		}()
	}
	wg.Wait()
	close(ids)

	seen := 0
	for id := range ids {
		if _, err := m.Get(context.Background(), id); err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		seen++
	}
	if seen != n {
		t.Fatalf("created %d sessions, want %d", seen, n)
	}
}
