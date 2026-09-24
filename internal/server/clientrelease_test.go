package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain keeps the relay's tests off the network: a relay started by any of
// them would otherwise look up the latest release on GitHub.
func TestMain(m *testing.M) {
	latestReleaseURL = ""
	os.Exit(m.Run())
}

// fakeReleases redirects the way GitHub's releases/latest does, to whatever
// tag holds, and counts requests.
func fakeReleases(t *testing.T, tag *atomic.Value) (url string, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/o/r/releases/tag/"+tag.Load().(string), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/o/r/releases/latest", hits
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestClientReleaseFollowsTheLatestRelease(t *testing.T) {
	var tag atomic.Value
	tag.Store("v0.3.3")
	url, _ := fakeReleases(t, &tag)

	c := newClientRelease(ClientVersionLatest, url, quietLogger())
	if !c.refresh(context.Background()) || c.Version() != "v0.3.3" {
		t.Fatalf("Version = %q, want v0.3.3", c.Version())
	}

	// A new release is picked up on the next look, with no redeploy.
	tag.Store("v0.3.4")
	c.refresh(context.Background())
	if c.Version() != "v0.3.4" {
		t.Fatalf("Version = %q, want v0.3.4", c.Version())
	}
}

func TestClientReleaseKeepsTheLastAnswerThroughAnOutage(t *testing.T) {
	var tag atomic.Value
	tag.Store("v0.3.3")
	url, _ := fakeReleases(t, &tag)
	c := newClientRelease("", url, quietLogger())
	c.refresh(context.Background())

	c.url = "http://127.0.0.1:1/releases/latest"
	if c.refresh(context.Background()) {
		t.Fatal("refresh reported success against nothing")
	}
	if c.Version() != "v0.3.3" {
		t.Fatalf("Version = %q after a failed look-up, want v0.3.3 kept", c.Version())
	}
}

func TestPinnedClientReleaseLooksNothingUp(t *testing.T) {
	var tag atomic.Value
	tag.Store("v9.9.9")
	url, hits := fakeReleases(t, &tag)

	c := newClientRelease("v0.3.1", url, quietLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.run(ctx) // returns at once when pinned

	if c.Version() != "v0.3.1" {
		t.Errorf("Version = %q, want the pinned v0.3.1", c.Version())
	}
	if hits.Load() != 0 {
		t.Errorf("a pinned relay asked for the latest release %d times", hits.Load())
	}
}

func TestHealthAdvertisesTheClientRelease(t *testing.T) {
	var tag atomic.Value
	tag.Store("v0.3.3")
	url, _ := fakeReleases(t, &tag)
	old := latestReleaseURL
	latestReleaseURL = url
	t.Cleanup(func() { latestReleaseURL = old })

	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	srv, err := New(cfg, quietLogger(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(5 * time.Second)
	for {
		var h healthResponse
		if resp, err := http.Get("http://" + srv.Addr() + "/health"); err == nil {
			json.NewDecoder(resp.Body).Decode(&h)
			resp.Body.Close()
		}
		if h.ClientVersion == "v0.3.3" {
			if h.Version != "dev" {
				t.Errorf("version = %q; the relay's own build should be unchanged", h.Version)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client_version = %q, want v0.3.3", h.ClientVersion)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPinnedClientReleaseIsATag(t *testing.T) {
	if got := newClientRelease("0.3.3", "", quietLogger()).Version(); got != "v0.3.3" {
		t.Errorf("Version = %q, want v0.3.3", got)
	}
}

func TestClientVersionSettingIsValidated(t *testing.T) {
	for v, ok := range map[string]bool{
		"":                  true,
		ClientVersionLatest: true,
		"v0.3.3":            true,
		"0.3.3":             true,
		"newest":            false,
		"v0.4.0-rc1":        false,
	} {
		cfg := DefaultConfig()
		cfg.ClientVersion = v
		if err := cfg.Validate(); (err == nil) != ok {
			t.Errorf("ClientVersion %q: Validate() = %v, want ok=%v", v, err, ok)
		}
	}
}
