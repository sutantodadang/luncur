package server

import (
	"fmt"
	"net/http"
)

// handlePrometheus serves the exposition format by hand — a scrape is
// "name{labels} value\n" lines; no client library needed. Gated by the
// sealed metrics_token setting: 404 while unset (endpoint invisible),
// 401 on a missing/wrong Authorization: Bearer <token> header.
func (s *server) handlePrometheus(w http.ResponseWriter, r *http.Request) {
	token, err := s.sealedSetting("metrics_token")
	if err != nil || token == "" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	fmt.Fprint(w, "# TYPE luncur_app_deploys_total counter\n")
	fmt.Fprint(w, "# TYPE luncur_app_cpu_millicores gauge\n")
	fmt.Fprint(w, "# TYPE luncur_app_memory_mib gauge\n")
	fmt.Fprint(w, "# TYPE luncur_app_replicas_ready gauge\n")
	fmt.Fprint(w, "# TYPE luncur_app_replicas_desired gauge\n")

	projects, err := s.st.ListProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, p := range projects {
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
			if err != nil {
				continue
			}
			m, err := s.appMetricsData(r.Context(), p, env, a)
			if err != nil {
				continue
			}
			l := fmt.Sprintf(`{project=%q,app=%q}`, p.Name, a.Name)
			fmt.Fprintf(w, "luncur_app_deploys_total%s %d\n", l, m.DeployCount)
			if m.Available {
				fmt.Fprintf(w, "luncur_app_cpu_millicores%s %d\n", l, m.CPUMillicores)
				fmt.Fprintf(w, "luncur_app_memory_mib%s %d\n", l, m.MemoryMiB)
			}
			fmt.Fprintf(w, "luncur_app_replicas_ready%s %d\n", l, m.ReadyReplicas)
			fmt.Fprintf(w, "luncur_app_replicas_desired%s %d\n", l, m.DesiredReplicas)
		}
	}
}
