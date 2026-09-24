package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/release"
)

// updateCheckTimeout bounds the whole update check. It runs alongside session
// setup and nothing waits on it for long; a slow answer is simply dropped.
const updateCheckTimeout = 3 * time.Second

// latestReleaseURL is where to look up the latest release when the relay
// does not say. A variable so tests can point it at a local server.
var latestReleaseURL = release.LatestURL

// checkForUpdate returns the newer release the user should move to, or ""
// when there is none or it cannot be told.
//
// The relay is asked first: it is already being contacted, it keeps track of
// the latest release itself, and a self-hosted one may pin the release its
// users should run. Only a relay that does not say — one too old to know, or
// one that could not reach GitHub — sends this to GitHub directly.
//
// Every failure is silent. This is a courtesy, and it must never delay or
// stop a share.
func checkForUpdate(ctx context.Context, api *Client, current string) string {
	cur, ok := release.Parse(current)
	if !ok {
		// A development build has nothing sensible to compare against.
		return ""
	}

	ctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()

	latestStr, err := api.ClientVersion(ctx)
	latest, ok := release.Parse(latestStr)
	if err != nil || !ok {
		if latestStr, err = release.Latest(ctx, latestReleaseURL); err != nil {
			return ""
		}
		if latest, ok = release.Parse(latestStr); !ok {
			return ""
		}
	}
	if !latest.Newer(cur) {
		return ""
	}
	return latestStr
}

// updateNotice says that latest is available to someone running current.
func updateNotice(latest, current string) string {
	return fmt.Sprintf("openconsole %s is available (you have %s)", latest, current)
}

// updateCommand is how to install version with this relay's own installer.
//
// The version is named rather than left to the installer's default of the
// latest release: a relay may pin an older one, and this should install what
// the notice just offered.
func updateCommand(server, version string) string {
	server = strings.TrimRight(server, "/")
	if runtime.GOOS == "windows" {
		return "$env:OPENCONSOLE_VERSION='" + version + "'; irm " + server + "/install.ps1 | iex"
	}
	return "curl -fsSL " + server + "/install.sh | OPENCONSOLE_VERSION=" + version + " sh"
}

// ClientVersion reports the client release the relay says to run, or "" when
// it does not say.
func (c *Client) ClientVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("relay returned HTTP %d", resp.StatusCode)
	}
	var h struct {
		ClientVersion string `json:"client_version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponse)).Decode(&h); err != nil {
		return "", err
	}
	return h.ClientVersion, nil
}
