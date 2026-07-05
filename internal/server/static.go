package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// staticHashOnce/staticHashVal cache the cache-buster below across calls —
// the embedded FS never changes at runtime, so the hash only needs computing
// once per process.
var (
	staticHashOnce sync.Once
	staticHashVal  string
)

// staticHash returns the first 12 hex chars of sha256(app.css || htmx.min.js)
// from the embedded static assets, used as the "?v=" cache-buster so a
// rebuilt stylesheet or vendored htmx busts client caches automatically
// instead of relying on the server's release version string.
func staticHash() string {
	staticHashOnce.Do(func() {
		css, _ := fs.ReadFile(staticFS, "static/app.css")
		js, _ := fs.ReadFile(staticFS, "static/htmx.min.js")
		sum := sha256.Sum256(append(css, js...))
		staticHashVal = hex.EncodeToString(sum[:])[:12]
	})
	return staticHashVal
}

// staticContentType maps the small, fixed set of vendored asset extensions
// this server ships to their Content-Type. Anything else 404s rather than
// guessing.
var staticContentType = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
}

// handleUIStatic serves the embedded CSS/JS assets under /ui/static/{file}.
// It never touches auth (the login page needs the stylesheet before any
// session exists) and only ever reads from the embedded FS by exact name —
// {file} is a single path segment from the router, so "/" or ".." in it
// can't escape the embedded static/ directory, but both are rejected
// explicitly as defense in depth.
func (s *server) handleUIStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	ct, ok := staticContentType[strings.ToLower(path.Ext(name))]
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(staticFS, "static/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
