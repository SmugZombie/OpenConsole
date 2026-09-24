package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/release"
)

// How often the relay looks up the latest client release. A release is not
// urgent news; hourly keeps clients current within the hour without the relay
// leaning on GitHub.
const (
	clientReleaseRefresh = time.Hour
	clientReleaseRetry   = 5 * time.Minute
	clientReleaseTimeout = 10 * time.Second
)

// latestReleaseURL is where the relay looks up the latest client release. A
// variable so tests can point it somewhere local, or at nothing.
var latestReleaseURL = release.LatestURL

// clientRelease is the client version the relay tells clients to run.
//
// Left to itself it follows the latest release on GitHub, so publishing a new
// client is enough for every relay to start mentioning it — no redeploy. An
// operator who wants their users on a particular release pins it instead, and
// then nothing is looked up.
type clientRelease struct {
	log   *slog.Logger
	url   string
	fixed bool

	mu      sync.RWMutex
	version string
}

// newClientRelease follows url unless setting names a version to pin.
func newClientRelease(setting, url string, log *slog.Logger) *clientRelease {
	c := &clientRelease{log: log, url: url}
	if setting != "" && setting != ClientVersionLatest {
		// Release tags carry the v, and clients hand this straight to the
		// installer as a tag, so 0.3.3 is advertised as v0.3.3.
		c.version, c.fixed = "v"+strings.TrimPrefix(setting, "v"), true
	}
	return c
}

// Version reports the client version to advertise, or "" when none is known
// yet.
func (c *clientRelease) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

// run keeps the version current until ctx ends.
func (c *clientRelease) run(ctx context.Context) {
	if c.fixed || c.url == "" {
		return
	}
	for {
		wait := clientReleaseRefresh
		if !c.refresh(ctx) {
			wait = clientReleaseRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// refresh looks the latest release up once and reports whether it succeeded.
// A failure keeps whatever was known before: an outage at GitHub is no reason
// to stop telling clients about a release that exists.
func (c *clientRelease) refresh(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, clientReleaseTimeout)
	defer cancel()

	v, err := release.Latest(ctx, c.url)
	if err != nil {
		if ctx.Err() == nil || ctx.Err() == context.DeadlineExceeded {
			c.log.Debug("could not look up the latest client release", slog.Any("error", err))
		}
		return false
	}

	c.mu.Lock()
	changed := v != c.version
	c.version = v
	c.mu.Unlock()
	if changed {
		c.log.Info("latest client release", slog.String("version", v))
	}
	return true
}
