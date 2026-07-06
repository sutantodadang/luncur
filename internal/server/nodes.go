package server

import (
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleListNodes(w http.ResponseWriter, r *http.Request, _ store.User) {
	if !s.requireKube(w) {
		return
	}
	nodes, err := s.kube.ListNodes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "kube_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}
