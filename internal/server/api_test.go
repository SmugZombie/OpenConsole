package server

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
	"time"

	"github.com/SmugZombie/OpenConsole/internal/session"
)

func testAPI(t *testing.T, ttl time.Duration) (*API, http.Handler) {
	t.Helper()
	m := session.NewManager(session.Options{TTL: ttl})
	t.Cleanup(m.Close)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	api := NewAPI(m, nil, log, "test", context.Background())
	return api, api.Routes()
}

func TestHandleHealth(t *testing.T) {
	_, h := testAPI(t, time.Minute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.Version != "test" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestCreateAndGetSession(t *testing.T) {
	_, h := testAPI(t, 30*time.Minute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var created createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.HostToken == "" || created.GuestToken == "" {
		t.Fatal("create response must include both tokens")
	}
	if created.ExpiresIn <= 0 {
		t.Fatalf("expires_in_seconds = %d, want > 0", created.ExpiresIn)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+created.SessionID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body)
	}

	// The lookup response must not leak credentials, in any field.
	raw := rec.Body.String()
	for _, secret := range []string{created.HostToken, created.GuestToken} {
		if strings.Contains(raw, secret) {
			t.Fatalf("GET response leaked a token: %s", raw)
		}
	}
	var got sessionResponse
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionID != created.SessionID {
		t.Fatalf("session_id = %q, want %q", got.SessionID, created.SessionID)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	_, h := testAPI(t, time.Minute)

	unused, err := session.NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}

	// Unknown and malformed IDs must be indistinguishable to a caller.
	for _, id := range []string{unused, "nope", strings.Repeat("z", 200)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+id, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %q status = %d, want 404", id, rec.Code)
		}
		var body errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Error != "session_not_found" {
			t.Fatalf("error = %q, want session_not_found", body.Error)
		}
	}
}

func TestGetExpiredSessionIsNotFound(t *testing.T) {
	// The clock is stepped rather than raced against. A TTL short enough to
	// expire "between the create and the lookup" relies on the platform
	// resolving time finely enough to notice, and on Windows both calls can
	// land on the same tick — where a session with a one-nanosecond life is
	// still valid.
	var clock atomicClock
	clock.set(time.Now())
	m := session.NewManager(session.Options{TTL: time.Minute, Now: clock.now})
	t.Cleanup(m.Close)
	api := NewAPI(m, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)), "test", context.Background())
	h := api.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil))
	var created createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	clock.advance(2 * time.Minute)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+created.SessionID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// atomicClock is a clock a test moves by hand.
//
// The manager reads it from its own goroutines, so the reads and the writes
// have to be safe against each other and not merely ordered by the test.
type atomicClock struct{ nanos atomic.Int64 }

func (c *atomicClock) set(t time.Time)         { c.nanos.Store(t.UnixNano()) }
func (c *atomicClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }
func (c *atomicClock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }

func TestMethodNotAllowed(t *testing.T) {
	_, h := testAPI(t, time.Minute)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
