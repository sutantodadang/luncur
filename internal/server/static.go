package server

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static/*
var staticFS embed.FS

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
