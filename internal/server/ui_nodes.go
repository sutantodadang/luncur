package server

import (
	"net/http"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleUINodes(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	var nodes []kube.NodeInfo
	var kubeErr string
	if s.kube == nil {
		kubeErr = "kubernetes is not configured"
	} else if list, err := s.kube.ListNodes(r.Context()); err != nil {
		kubeErr = err.Error()
	} else {
		nodes = list
	}
	s.renderPage(w, "nodes.html", map[string]any{
		"User": u, "Nodes": nodes, "Error": kubeErr,
		"CSRF": s.csrf(w, r), "IsAdmin": true,
	})
}
