package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestUIStaticAssets covers the embedded CSS/JS route: correct content
// types and cache headers for the two vendored assets, a 404 for unknown
// names, and — crucially — no auth required (the login page needs the
// stylesheet before any session cookie exists).
func TestUIStaticAssets(t *testing.T) {
	srv, _ := testServer(t)

	css, err := http.Get(srv.URL + "/ui/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer css.Body.Close()
	if css.StatusCode != http.StatusOK {
		t.Fatalf("GET app.css: want 200, got %d", css.StatusCode)
	}
	if ct := css.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("app.css content-type: got %q", ct)
	}
	if cc := css.Header.Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Fatalf("app.css cache-control: got %q", cc)
	}

	js, err := http.Get(srv.URL + "/ui/static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	defer js.Body.Close()
	if js.StatusCode != http.StatusOK {
		t.Fatalf("GET htmx.min.js: want 200, got %d", js.StatusCode)
	}
	if ct := js.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("htmx.min.js content-type: got %q", ct)
	}

	notFound, err := http.Get(srv.URL + "/ui/static/nope.css")
	if err != nil {
		t.Fatal(err)
	}
	defer notFound.Body.Close()
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("GET nope.css: want 404, got %d", notFound.StatusCode)
	}
}
