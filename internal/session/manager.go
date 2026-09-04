package session

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DefaultTTL is used when a Manager is configured with a non-positive TTL.
const DefaultTTL = 30 * time.Minute

// defaultSweepInterval bounds how long an expired session lingers in memory.
// Expiry itself is enforced on every lookup, so the sweeper is only about
// reclaiming memory, not about correctness.
const defaultSweepInterval = time.Minute

// Options configures a Manager.
type Options struct {
	// TTL is how long a session stays valid after creation.
	TTL time.Duration
	// SweepInterval is how often expired sessions are purged. Zero selects a
	// sane default.
	SweepInterval time.Duration
	// Now is the clock, overridable in tests. Zero value means time.Now.
	Now func() time.Time
}

// Manager is an in-memory session store, safe for concurrent use.
//
// In-memory is a deliberate Phase 1 choice: a session is meaningless once the
// relay process that holds its tunnel goes away, so durable storage would buy
// nothing until the relay is clustered.
type Manager struct {
	ttl   time.Duration
	now   func() time.Time
	sweep time.Duration

	mu       sync.RWMutex
	sessions map[string]*Session
	closed   bool

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewManager returns a Manager. The returned Manager does not run a background
// sweeper until Run is called; expiry is still enforced on lookup regardless.
func NewManager(opts Options) *Manager {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	sweep := opts.SweepInterval
	if sweep <= 0 {
		sweep = defaultSweepInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		ttl:      ttl,
		now:      now,
		sweep:    sweep,
		sessions: make(map[string]*Session),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// TTL reports the configured session lifetime.
func (m *Manager) TTL() time.Duration { return m.ttl }

// Create allocates a new session with freshly generated credentials.
func (m *Manager) Create(ctx context.Context) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id, err := NewID()
	if err != nil {
		return nil, err
	}
	hostToken, err := NewToken()
	if err != nil {
		return nil, err
	}
	guestToken, err := NewToken()
	if err != nil {
		return nil, err
	}
	viewerToken, err := NewToken()
	if err != nil {
		return nil, err
	}

	now := m.now()
	s := &Session{
		SessionID:   id,
		HostToken:   hostToken,
		GuestToken:  guestToken,
		ViewerToken: viewerToken,
		CreatedAt:   now,
		ExpiresAt:   now.Add(m.ttl),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	// A collision is astronomically unlikely with 128 bits of entropy, but
	// silently overwriting a live session would be a security bug, so refuse.
	if _, exists := m.sessions[id]; exists {
		return nil, fmt.Errorf("session: id collision")
	}
	m.sessions[id] = s
	return s.clone(), nil
}

// Get returns a live session by public ID. Expired sessions are treated as
// absent and removed.
func (m *Manager) Get(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ValidID(id) {
		return nil, ErrInvalidID
	}

	m.mu.RLock()
	s, ok := m.sessions[id]
	expired := ok && s.Expired(m.now())
	if ok && !expired {
		c := s.clone()
		m.mu.RUnlock()
		return c, nil
	}
	m.mu.RUnlock()

	if expired {
		m.Delete(id)
	}
	return nil, ErrNotFound
}

// Delete removes a session. Deleting a session that does not exist is not an
// error: callers are usually cleaning up and should not have to care.
func (m *Manager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// Len returns the number of sessions currently held, including any that have
// expired but not yet been swept.
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// Run purges expired sessions until ctx is cancelled or Close is called. It is
// intended to run in its own goroutine for the lifetime of the process.
func (m *Manager) Run(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(m.sweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-t.C:
			m.sweepExpired()
		}
	}
}

// Close stops the sweeper and drops all sessions. It is safe to call more than
// once.
func (m *Manager) Close() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.sessions = make(map[string]*Session)
}

// sweepExpired drops every session whose TTL has passed. It returns the number
// removed so callers (and tests) can observe progress.
func (m *Manager) sweepExpired() int {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if s.Expired(now) {
			delete(m.sessions, id)
			n++
		}
	}
	return n
}
