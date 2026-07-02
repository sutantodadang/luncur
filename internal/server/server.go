// Package server implements luncur's REST API.
package server

import (
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

type server struct {
	st *store.Store
}

// New builds the full API handler. Later plans add their routes here.
func New(st *store.Store) http.Handler {
	s := &server{st: st}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/login", s.handleLogin)

	return mux
}
