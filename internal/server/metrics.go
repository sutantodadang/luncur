package server

import (
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleAppMetrics(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	deploys, err := s.st.CountDeployments(a.ID)
	if err != nil {
		log.Printf("count deployments: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := map[string]any{
		"available": false, "cpu_millicores": int64(0), "memory_mib": int64(0),
		"pods": 0, "ready_replicas": int64(0), "desired_replicas": int64(0),
		"deploy_count": deploys,
	}
	if s.kube != nil {
		if m, ok := s.kube.AppMetrics(r.Context(), p.Namespace, a.Name); ok {
			out["available"] = true
			out["cpu_millicores"] = m.CPUMilli
			out["memory_mib"] = m.MemoryMiB
			out["pods"] = m.Pods
		}
		if ready, desired, err := s.kube.DeploymentStatus(r.Context(), p.Namespace, a.Name); err == nil {
			out["ready_replicas"] = ready
			out["desired_replicas"] = desired
		}
	}
	writeJSON(w, http.StatusOK, out)
}
