package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRelay answers /health with version as its client_version, leaves it out
// when version is "-", and fails when version is "".
func fakeRelay(t *testing.T, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || version == "" {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		if version == "-" {
			fmt.Fprint(w, `{"status":"ok","version":"v0.3.3"}`)
			return
		}
		fmt.Fprintf(w, `{"status":"ok","version":"dev","client_version":%q}`, version)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeGitHub redirects the way github.com's releases/latest does, and counts
// how often it is asked.
func fakeGitHub(t *testing.T, tag string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/SmugZombie/OpenConsole/releases/tag/"+tag, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	old := latestReleaseURL
	latestReleaseURL = srv.URL + "/SmugZombie/OpenConsole/releases/latest"
	t.Cleanup(func() { latestReleaseURL = old })
	return &hits
}

func TestUpdateCheckPrefersTheRelay(t *testing.T) {
	gh := fakeGitHub(t, "v9.9.9")
	api := NewClient(fakeRelay(t, "v0.3.3").URL)

	got := checkForUpdate(context.Background(), api, "v0.3.1")
	if got != "v0.3.3" {
		t.Errorf("newer = %q, want v0.3.3", got)
	}
	if gh.Load() != 0 {
		t.Error("GitHub was asked although the relay reported a release")
	}
}

func TestUpdateCheckTrustsARelayThatIsBehindGitHub(t *testing.T) {
	// A self-hosted relay may pin the release its users run.
	fakeGitHub(t, "v9.9.9")
	api := NewClient(fakeRelay(t, "v0.3.1").URL)
	if got := checkForUpdate(context.Background(), api, "v0.3.1"); got != "" {
		t.Errorf("notice = %q, want none", got)
	}
}

func TestUpdateCheckFallsBackToGitHub(t *testing.T) {
	for name, relayVersion := range map[string]string{
		"relay does not say":        "-",
		"relay says nothing usable": "dev",
		"relay has no health":       "",
	} {
		t.Run(name, func(t *testing.T) {
			gh := fakeGitHub(t, "v0.3.3")
			api := NewClient(fakeRelay(t, relayVersion).URL)

			got := checkForUpdate(context.Background(), api, "v0.3.1")
			if got != "v0.3.3" {
				t.Errorf("newer = %q, want v0.3.3", got)
			}
			if gh.Load() != 1 {
				t.Errorf("GitHub asked %d times, want 1", gh.Load())
			}
		})
	}
}

func TestUpdateCheckIsQuietWhenCurrent(t *testing.T) {
	fakeGitHub(t, "v0.3.3")
	api := NewClient(fakeRelay(t, "-").URL)
	if got := checkForUpdate(context.Background(), api, "v0.3.3"); got != "" {
		t.Errorf("notice = %q, want none", got)
	}
}

func TestUpdateCheckSkipsDevelopmentBuilds(t *testing.T) {
	gh := fakeGitHub(t, "v0.3.3")
	api := NewClient(fakeRelay(t, "v0.3.3").URL)
	if got := checkForUpdate(context.Background(), api, "dev"); got != "" {
		t.Errorf("notice = %q, want none", got)
	}
	if gh.Load() != 0 {
		t.Error("a dev build should not look anything up")
	}
}

func TestUpdateCheckIsQuietWhenNothingAnswers(t *testing.T) {
	old := latestReleaseURL
	latestReleaseURL = "http://127.0.0.1:1/releases/latest"
	t.Cleanup(func() { latestReleaseURL = old })
	api := NewClient("http://127.0.0.1:1")
	if got := checkForUpdate(context.Background(), api, "v0.3.1"); got != "" {
		t.Errorf("notice = %q, want none", got)
	}
}

func TestUpdateCommandInstallsTheOfferedVersion(t *testing.T) {
	got := updateCommand("https://relay.example/", "v0.3.2")
	if !strings.Contains(got, "https://relay.example/install.") {
		t.Errorf("updateCommand = %q; want the relay's own installer", got)
	}
	// The installer would otherwise fetch the latest release, which is not
	// what a relay pinned to an older one offered.
	if !strings.Contains(got, "OPENCONSOLE_VERSION") || !strings.Contains(got, "v0.3.2") {
		t.Errorf("updateCommand = %q; want it to name v0.3.2", got)
	}
}
