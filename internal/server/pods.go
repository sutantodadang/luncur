package server

import (
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleAppPods returns an app's live pods with per-pod usage.
func (s *server) handleAppPods(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}
	pods, err := s.kube.AppPodInfos(r.Context(), env.Namespace, a.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "kube_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pods": pods})
}
