package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	Register(mux)
	return mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestBundleIsEmbedded(t *testing.T) {
	// A build that forgot to run the web build would otherwise ship a relay
	// whose UI is a 404, and nothing else would catch it.
	assets := Assets()
	for _, name := range []string{"index.html", "favicon.svg", "icon.svg"} {
		if _, err := fs.Stat(assets, name); err != nil {
			t.Errorf("bundle is missing %s: %v", name, err)
		}
	}

	entries, err := fs.ReadDir(assets, "assets")
	if err != nil {
		t.Fatalf("bundle has no assets directory: %v", err)
	}
	var js, css bool
	for _, e := range entries {
		js = js || strings.HasSuffix(e.Name(), ".js")
		css = css || strings.HasSuffix(e.Name(), ".css")
	}
	if !js || !css {
		t.Fatalf("bundle is missing built js/css: %v", entries)
	}
}

func TestServesIndexForSessionPages(t *testing.T) {
	mux := newMux()
	for _, path := range []string{"/", "/s/x5s5gzxptgfksy3hu75jmcoltm", "/s/anything"} {
		rec := get(t, mux, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q", path, ct)
		}
		if !strings.Contains(rec.Body.String(), "<title>OpenConsole</title>") {
			t.Errorf("GET %s did not serve the app shell", path)
		}
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	// A catch-all would turn every typo into a blank terminal page.
	mux := newMux()
	for _, path := range []string{"/nope", "/s/a/b", "/s/a/b/c", "/assets/missing.js"} {
		if rec := get(t, mux, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// http.FileServer renders an index listing for a directory, which would let a
// bare /assets/ enumerate the bundle.
func TestDirectoryListingIsNotServed(t *testing.T) {
	rec := get(t, newMux(), "/assets/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /assets/ = %d, want 404\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<a href=") {
		t.Fatalf("GET /assets/ rendered a directory listing:\n%s", rec.Body.String())
	}
}

func TestServesBrandAssets(t *testing.T) {
	mux := newMux()
	tests := []struct {
		path     string
		wantType string
	}{
		{"/favicon.svg", "image/svg+xml"},
		{"/favicon.ico", "image/"},
		{"/icon.svg", "image/svg+xml"},
		{"/icon-512.png", "image/png"},
		{"/apple-touch-icon.png", "image/png"},
		{"/og.jpg", "image/jpeg"},
		{"/site.webmanifest", "application/manifest+json"},
		// Plain text so a browser shows the installer rather than downloading
		// it: anyone piping a script into a shell should be able to read it.
		{"/install.sh", "text/plain"},
	}
	for _, tc := range tests {
		rec := get(t, mux, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s served an empty body", tc.path)
		}
		ct := rec.Header().Get("Content-Type")
		if tc.wantType != "" && !strings.HasPrefix(ct, tc.wantType) {
			// nosniff is set, so a wrong Content-Type is a broken asset, not a
			// cosmetic problem.
			t.Errorf("GET %s Content-Type = %q, want %s*", tc.path, ct, tc.wantType)
		}
	}
}

func TestFingerprintedAssetsAreCachedForever(t *testing.T) {
	mux := newMux()
	assets := Assets()
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("no built assets: %v", err)
	}

	rec := get(t, mux, "/assets/"+entries[0].Name())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asset = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("fingerprinted asset Cache-Control = %q, want immutable", cc)
	}
}

func TestIndexIsNotCached(t *testing.T) {
	// index.html names the current asset hashes; caching it would strand a
	// client on a bundle that no longer exists after an upgrade.
	rec := get(t, newMux(), "/")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", cc)
	}
}

func TestSecurityHeaders(t *testing.T) {
	mux := newMux()
	for _, path := range []string{"/", "/s/abc", "/favicon.svg"} {
		rec := get(t, mux, path)
		h := rec.Header()

		if got := h.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("GET %s Referrer-Policy = %q", path, got)
		}
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q", path, got)
		}
		if got := h.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("GET %s X-Frame-Options = %q", path, got)
		}

		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{
			"default-src 'self'",
			"script-src 'self'",
			"frame-ancestors 'none'",
			"connect-src 'self' ws: wss:",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s CSP missing %q: %s", path, want, csp)
			}
		}
	}
}

// The installer is the one file people are told to pipe into a shell, so it
// has to actually be there and actually be a script.
func TestInstallerIsServed(t *testing.T) {
	rec := get(t, newMux(), "/install.sh")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /install.sh = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "#!/bin/sh") {
		t.Fatalf("the installer does not start with a shebang: %.40q", body)
	}
	for _, want := range []string{"SmugZombie/OpenConsole", "openconsole"} {
		if !strings.Contains(body, want) {
			t.Errorf("the installer does not mention %q", want)
		}
	}
}

func TestPageDoesNotEmbedACredential(t *testing.T) {
	// The token reaches the page through the URL fragment at runtime. Nothing
	// server-rendered should ever contain one.
	body := get(t, newMux(), "/s/abc").Body.String()
	for _, forbidden := range []string{"host_token", "guest_token"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the app shell mentions %q", forbidden)
		}
	}
}
