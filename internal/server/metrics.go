package server

import (
	"context"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// appMetricsView is the metrics view-model shared by the JSON /metrics
// endpoint and the UI app page.
type appMetricsView struct {
	Available       bool
	CPUMillicores   int64
	MemoryMiB       int64
	Pods            int
	ReadyReplicas   int64
	DesiredReplicas int64
	DeployCount     int64
}

// appMetricsData builds an app's metrics view: deploy count always comes
// from the store; CPU/memory/replica fields stay zero (Available=false)
// when kube is nil or metrics-server is unreachable.
func (s *server) appMetricsData(ctx context.Context, p store.Project, a store.App) (appMetricsView, error) {
	deploys, err := s.st.CountDeployments(a.ID)
	if err != nil {
		return appMetricsView{}, err
	}
	out := appMetricsView{DeployCount: deploys}
	if s.kube != nil {
		if m, ok := s.kube.AppMetrics(ctx, p.Namespace, a.Name); ok {
			out.Available = true
			out.CPUMillicores = m.CPUMilli
			out.MemoryMiB = m.MemoryMiB
			out.Pods = m.Pods
		}
		if ready, desired, err := s.kube.DeploymentStatus(ctx, p.Namespace, a.Name); err == nil {
			out.ReadyReplicas = ready
			out.DesiredReplicas = desired
		}
	}
	return out, nil
}

func (s *server) handleAppMetrics(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	m, err := s.appMetricsData(r.Context(), p, a)
	if err != nil {
		log.Printf("count deployments: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": m.Available, "cpu_millicores": m.CPUMillicores, "memory_mib": m.MemoryMiB,
		"pods": m.Pods, "ready_replicas": m.ReadyReplicas, "desired_replicas": m.DesiredReplicas,
		"deploy_count": m.DeployCount,
	})
}
