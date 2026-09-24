package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]bool{
		"v0.3.3":      true,
		"1.2.10":      true,
		"dev":         false,
		"":            false,
		"v0.4.0-rc1":  false,
		"v0.3.3+meta": false,
		"v0.3":        false,
		"v0.x.1":      false,
	} {
		if _, ok := Parse(in); ok != want {
			t.Errorf("Parse(%q) ok = %v, want %v", in, ok, want)
		}
	}
}

func TestNewerComparesNumerically(t *testing.T) {
	v := func(s string) Version { p, _ := Parse(s); return p }
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.3.10", "v0.3.9", true},
		{"v0.4.0", "v0.3.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.3.3", "v0.3.3", false},
		{"v0.3.2", "v0.3.3", false},
	}
	for _, c := range cases {
		if got := v(c.a).Newer(v(c.b)); got != c.want {
			t.Errorf("%s.Newer(%s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestLatestReadsTheRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/o/r/releases/tag/v0.3.3", http.StatusFound)
	}))
	defer srv.Close()

	got, err := Latest(context.Background(), srv.URL+"/o/r/releases/latest")
	if err != nil || got != "v0.3.3" {
		t.Fatalf("Latest = %q, %v; want v0.3.3", got, err)
	}
}

func TestLatestRefusesOddAnswers(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"no redirect": func(w http.ResponseWriter, r *http.Request) {},
		"not a tag":   func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) },
		"pre-release": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/o/r/releases/tag/v0.4.0-rc1", http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if got, err := Latest(context.Background(), srv.URL); err == nil {
				t.Errorf("Latest = %q, want an error", got)
			}
		})
	}
}
