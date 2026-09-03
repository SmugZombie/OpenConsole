package session

import (
	"log/slog"
	"sync"
)

// Bridges tracks the live bridge for each session.
//
// A session record (Manager) and its live wiring (Bridge) are separate on
// purpose: a session exists as soon as it is created over HTTP, but it only
// gets a bridge when a host actually connects a terminal to it. Keeping them
// apart means an abandoned session costs a map entry, not a goroutine.
type Bridges struct {
	log *slog.Logger

	mu sync.Mutex
	m  map[string]*Bridge
}

// NewBridges returns an empty registry.
func NewBridges(log *slog.Logger) *Bridges {
	if log == nil {
		log = slog.Default()
	}
	return &Bridges{log: log, m: make(map[string]*Bridge)}
}

// Open creates the bridge for a session. It fails with ErrHostAlreadyAttached
// if one already exists, which is how a second host is refused.
//
// The bridge removes itself from the registry when it closes, so callers do not
// have to coordinate teardown with the guests still draining.
func (r *Bridges) Open(id string) (*Bridge, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.m[id]; exists {
		return nil, ErrHostAlreadyAttached
	}
	b := newBridge(id, r.log)
	b.onClosed = func() { r.remove(id, b) }
	r.m[id] = b
	return b, nil
}

// Get returns the live bridge for a session.
func (r *Bridges) Get(id string) (*Bridge, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.m[id]
	return b, ok
}

// Len reports how many sessions currently have a connected host.
func (r *Bridges) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// remove deletes id, but only if it still maps to b. The identity check keeps a
// closing bridge from evicting a newer one that reused the same id.
func (r *Bridges) remove(id string, b *Bridge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[id]; ok && cur == b {
		delete(r.m, id)
	}
}

// CloseAll tears down every bridge, used on server shutdown.
func (r *Bridges) CloseAll(reason string) {
	r.mu.Lock()
	all := make([]*Bridge, 0, len(r.m))
	for _, b := range r.m {
		all = append(all, b)
	}
	r.mu.Unlock()

	for _, b := range all {
		b.Close(reason)
	}
}
