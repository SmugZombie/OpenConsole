package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/session"
)

// policyAPI builds an API with a create policy applied.
func policyAPI(t *testing.T, cfg Config) (*API, http.Handler) {
	t.Helper()
	m := session.NewManager(session.Options{
		TTL:         time.Minute,
		MaxSessions: cfg.MaxSessions,
	})
	t.Cleanup(m.Close)

	api := NewAPI(m, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)), "test", nil)
	api.SetCreatePolicy(cfg)
	return api, api.Routes()
}

// create posts a session creation request from a given source.
func create(h http.Handler, remote string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	r.RemoteAddr = remote
	for k, v := range header {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestCreateIsRateLimitedPerSource(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CreateRatePerMin = 60
	cfg.CreateBurst = 3
	_, h := policyAPI(t, cfg)

	for i := 0; i < 3; i++ {
		if rec := create(h, "203.0.113.1:1000", nil); rec.Code != http.StatusCreated {
			t.Fatalf("request %d = %d, want 201", i+1, rec.Code)
		}
	}

	rec := create(h, "203.0.113.1:1000", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	// A caller should be told when to come back, not left to guess.
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a 429 should carry Retry-After")
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "rate_limited" {
		t.Fatalf("error = %q", body.Error)
	}

	// Somebody else is unaffected.
	if rec := create(h, "198.51.100.9:1000", nil); rec.Code != http.StatusCreated {
		t.Fatalf("a different source got %d, want 201", rec.Code)
	}
}

// Behind a proxy, every request arrives from the proxy. Without knowing that,
// the per-source limit is one bucket for the whole internet.
func TestCreateRateLimitUsesForwardedClientWhenProxyTrusted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CreateRatePerMin = 60
	cfg.CreateBurst = 2
	proxies, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	cfg.TrustedProxies = proxies
	_, h := policyAPI(t, cfg)

	fromProxy := func(client string) *httptest.ResponseRecorder {
		return create(h, "10.0.0.1:443", map[string]string{"X-Forwarded-For": client})
	}

	for i := 0; i < 2; i++ {
		if rec := fromProxy("198.51.100.1"); rec.Code != http.StatusCreated {
			t.Fatalf("request %d = %d", i+1, rec.Code)
		}
	}
	if rec := fromProxy("198.51.100.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the forwarded client was not limited: %d", rec.Code)
	}
	// A different client behind the same proxy still gets its own allowance.
	if rec := fromProxy("198.51.100.2"); rec.Code != http.StatusCreated {
		t.Fatalf("a second client behind the proxy got %d", rec.Code)
	}
}

// The header is attacker-controlled, so it must not be a way around the limit
// when the connection did not come from a trusted proxy.
func TestCreateRateLimitIgnoresSpoofedForwardedHeader(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CreateRatePerMin = 60
	cfg.CreateBurst = 2
	_, h := policyAPI(t, cfg) // trusts no proxies

	for i := 0; i < 2; i++ {
		create(h, "203.0.113.5:1000", map[string]string{"X-Forwarded-For": "1.1.1.1"})
	}
	// Rotating the claimed address must not refill the bucket.
	rec := create(h, "203.0.113.5:1000", map[string]string{"X-Forwarded-For": "2.2.2.2"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a spoofed header evaded the limit: %d", rec.Code)
	}
}

func TestCreateRespectsSessionCeiling(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CreateRatePerMin = 0 // isolate the ceiling from the rate limit
	cfg.MaxSessions = 3
	_, h := policyAPI(t, cfg)

	for i := 0; i < 3; i++ {
		if rec := create(h, "203.0.113.1:1000", nil); rec.Code != http.StatusCreated {
			t.Fatalf("request %d = %d, want 201", i+1, rec.Code)
		}
	}

	rec := create(h, "203.0.113.2:1000", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "at_capacity" {
		t.Fatalf("error = %q, want at_capacity", body.Error)
	}
}

func TestCreateTokenRequiredWhenConfigured(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CreateRatePerMin = 0
	cfg.CreateToken = "s3cret-relay-token"
	_, h := policyAPI(t, cfg)

	t.Run("no token", func(t *testing.T) {
		rec := create(h, "203.0.113.1:1000", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		// Tell a client what is missing rather than looking broken.
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("a 401 should carry a challenge")
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		rec := create(h, "203.0.113.1:1000", map[string]string{
			"Authorization": "Bearer wrong",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong scheme", func(t *testing.T) {
		rec := create(h, "203.0.113.1:1000", map[string]string{
			"Authorization": "Basic s3cret-relay-token",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("correct token", func(t *testing.T) {
		rec := create(h, "203.0.113.1:1000", map[string]string{
			"Authorization": "Bearer s3cret-relay-token",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
	})
}

// An authorised caller should not share an allowance with whoever is probing
// the relay, so the token is checked first.
func TestCreateTokenCheckedBeforeRateLimit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CreateRatePerMin = 60
	cfg.CreateBurst = 1
	cfg.CreateToken = "tok"
	_, h := policyAPI(t, cfg)

	// Unauthorised attempts are refused without consuming the bucket.
	for i := 0; i < 5; i++ {
		if rec := create(h, "203.0.113.1:1000", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	rec := create(h, "203.0.113.1:1000", map[string]string{"Authorization": "Bearer tok"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("an authorised request was refused: %d", rec.Code)
	}
}

// The defaults have to work for a person on a laptop without configuration.
func TestDefaultsAllowOrdinaryUse(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CreateToken != "" {
		t.Fatal("a relay should not require a token out of the box")
	}
	_, h := policyAPI(t, cfg)
	for i := 0; i < cfg.CreateBurst; i++ {
		if rec := create(h, "127.0.0.1:1000", nil); rec.Code != http.StatusCreated {
			t.Fatalf("request %d = %d under default policy", i+1, rec.Code)
		}
	}
}
