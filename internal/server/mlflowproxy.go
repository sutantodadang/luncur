package server

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/sutantodadang/luncur/internal/addon"
	"github.com/sutantodadang/luncur/internal/store"
)

// handleUIMlflow reverse-proxies /ui/mlflow/{ns}/{name}/* to the mlflow
// addon's in-cluster service. mlflow serves its UI, API and health endpoint
// under the same prefix (--static-prefix, set at render time), so paths
// forward unchanged. The route is keyed by namespace, which is immutable,
// so links survive project renames. Same leak-nothing membership rule as
// uiProject; CSRF is covered by the SameSite=Strict session cookie (mlflow's
// own POSTs carry no panel token).
func (s *server) handleUIMlflow(w http.ResponseWriter, r *http.Request, u store.User) {
	ns := r.PathValue("ns")
	projects, err := s.st.ListProjects()
	if err != nil {
		log.Printf("ui mlflow list projects: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var p store.Project
	found := false
	for _, pr := range projects {
		if pr.Namespace == ns {
			p, found = pr, true
			break
		}
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if u.Role != "admin" {
		ok, err := s.st.IsMember(p.ID, u.ID)
		if err != nil || !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	ad, ok := s.uiAddon(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	if ad.Type != "mlflow" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s.%s:%d", addon.ServiceName(ad.Name), p.Namespace, addon.MLflowPort),
	}
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}
