// Package webui serves the browser terminal client.
//
// The built bundle is embedded in the binary, so the relay is still a single
// file with nothing to mount and no static-file directory to configure. The
// sources live in ../../web; see web/README.md for the build.
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"strings"
)

// dist holds the Vite build output. `all:` is required so Vite's hashed asset
// files are included whatever they are named.
//
//go:embed all:dist
var dist embed.FS

// assetPrefix is where Vite writes fingerprinted bundles.
const assetPrefix = "/assets/"

func init() {
	// Go's mime table has no entry for .webmanifest, so it would fall back to
	// sniffing and land on text/plain. With nosniff set, a browser then refuses
	// the manifest outright.
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		panic("webui: registering the webmanifest media type: " + err.Error())
	}
}

// Assets returns the embedded bundle rooted at its top level.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive stops matching, which would
		// fail the build first.
		panic("webui: " + err.Error())
	}
	return sub
}

// Register mounts the browser client on mux.
//
// Routes are registered explicitly rather than as a catch-all `GET /`. A
// catch-all matches every path the API did not claim, which sounds harmless
// but silently changes API behaviour: `GET /api/v1/sessions` would match the
// catch-all and return the UI's 404 instead of the 405 the pattern mux gives
// when a path exists under a different method. Naming the UI's real paths keeps
// the two surfaces from interfering.
func Register(mux *http.ServeMux) {
	assets := Assets()

	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic("webui: missing index.html in the embedded bundle: " + err.Error())
	}
	entry := indexHandler(index)
	files := fileHandler(assets)

	// The single-page entry: the landing form, and every session page.
	mux.Handle("GET /{$}", entry)
	mux.Handle("GET /s/{id}", entry)

	// Fingerprinted bundles.
	mux.Handle("GET "+assetPrefix, files)

	// Everything else the bundle puts at the root — favicons, the manifest,
	// the social image. Enumerated from the bundle so adding a brand asset
	// needs no change here.
	roots, err := fs.ReadDir(assets, ".")
	if err != nil {
		panic("webui: reading the embedded bundle: " + err.Error())
	}
	for _, e := range roots {
		if e.IsDir() || e.Name() == "index.html" {
			continue
		}
		mux.Handle("GET /"+e.Name(), files)
	}
}

// indexHandler serves the SPA entry point.
func indexHandler(index []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// index.html names the current asset hashes, so it must revalidate or a
		// client would keep loading a bundle that no longer exists.
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	})
}

// noDirFS hides directories from the file server.
//
// http.FileServer renders an index listing for a directory, so a bare
// /assets/ would enumerate the bundle. It is not a secret, but a relay should
// not answer requests nobody meant to make.
type noDirFS struct{ fs.FS }

func (n noDirFS) Open(name string) (fs.File, error) {
	f, err := n.FS.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}

// fileHandler serves static files out of the bundle.
func fileHandler(assets fs.FS) http.Handler {
	srv := http.FileServer(http.FS(noDirFS{assets}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		// Vite fingerprints asset filenames with a content hash, so a given URL's
		// bytes never change and it can be cached forever. Root files keep their
		// names across builds, so they only revalidate.
		if strings.HasPrefix(r.URL.Path, assetPrefix) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		srv.ServeHTTP(w, r)
	})
}

// setSecurityHeaders applies the headers that matter for a page holding a
// credential in its URL fragment.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()

	// Everything the page loads is served from this origin and embedded in the
	// binary, so the policy can be strict. 'unsafe-inline' for styles is
	// xterm.js, which sets element styles directly as it renders.
	h.Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"font-src 'self'; "+
			"connect-src 'self' ws: wss:; "+
			"frame-ancestors 'none'; "+
			"base-uri 'none'; "+
			"form-action 'none'")

	// The session URL carries a token in its fragment. Fragments are never sent
	// in a Referer, but suppressing it entirely also keeps the session ID from
	// leaking to wherever a user navigates next.
	h.Set("Referrer-Policy", "no-referrer")

	// Types come from file extensions; a mislabelled file must fail loudly
	// rather than be sniffed into something executable.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
}
