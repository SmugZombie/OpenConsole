// Package release knows what an OpenConsole release version looks like and
// how to find the newest one.
//
// Both halves use it: the relay looks up the latest client release so that it
// can tell clients, and a client asks GitHub itself when its relay does not
// say.
package release

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// LatestURL redirects to the newest release's tag page. Reading the redirect
// gives the version without the GitHub API's rate limit or a JSON body to
// parse.
const LatestURL = "https://github.com/SmugZombie/OpenConsole/releases/latest"

// Latest asks url, a releases/latest page, which release is newest.
func Latest(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || !strings.Contains(loc, "/releases/tag/") {
		return "", fmt.Errorf("unexpected answer from %s: HTTP %d", url, resp.StatusCode)
	}
	v := path.Base(loc)
	if _, ok := Parse(v); !ok {
		return "", fmt.Errorf("latest release %q is not a release version", v)
	}
	return v, nil
}

// Version is a parsed release version.
type Version [3]int

// Parse reads a release version, vMAJOR.MINOR.PATCH.
//
// Anything else — "dev", a pre-release, a build with metadata — is refused,
// so nobody is told to move to a release candidate, and a pre-release build
// is not nagged about the release it came before.
func Parse(s string) (Version, bool) {
	var v Version
	s = strings.TrimPrefix(s, "v")
	if strings.ContainsAny(s, "-+") {
		return v, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// Newer reports whether v is a later release than other.
func (v Version) Newer(other Version) bool {
	for i := range v {
		if v[i] != other[i] {
			return v[i] > other[i]
		}
	}
	return false
}
