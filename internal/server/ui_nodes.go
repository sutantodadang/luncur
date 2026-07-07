package server

import (
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sutantodadang/luncur/internal/gpucloud"
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

	// GPU cloud card: tracked instances (with live provider status merged),
	// plus an offer search when ?gpu= / ?count= arrived from the form.
	hasKey := false
	var gpuErr string
	var offers []gpucloud.Offer
	instances, err := s.st.ListGPUInstances()
	if err != nil {
		log.Printf("ui nodes: list gpu instances: %v", err)
	}
	rows := make([]map[string]any, 0, len(instances))
	if v, err := s.vast(); err == nil {
		hasKey = true
		live := map[string]gpucloud.Instance{}
		if ins, err := v.List(r.Context()); err == nil {
			for _, i := range ins {
				live[i.Ref] = i
			}
		}
		for _, g := range instances {
			m := gpuInstanceJSON(g)
			if li, ok := live[g.ExternalRef]; ok {
				m["provider_status"] = li.Status
				m["dph_total"] = li.DPHTotal
			}
			rows = append(rows, m)
		}
		if q := r.URL.Query(); q.Get("gpu") != "" || q.Get("count") != "" {
			n, _ := strconv.Atoi(q.Get("count"))
			offers, err = v.SearchOffers(r.Context(), q.Get("gpu"), n, 10)
			if err != nil {
				gpuErr = err.Error()
			}
		}
	} else {
		for _, g := range instances {
			rows = append(rows, gpuInstanceJSON(g))
		}
	}

	s.renderPage(w, "nodes.html", map[string]any{
		"User": u, "Nodes": nodes, "Error": kubeErr,
		"HasGPUKey": hasKey, "GPUInstances": rows, "GPUOffers": offers,
		"GPUError": firstNonEmpty(gpuErr, r.URL.Query().Get("gpu_err")),
		"GPUQuery": r.URL.Query().Get("gpu"), "GPUCount": r.URL.Query().Get("count"),
		"CSRF": s.csrf(w, r), "IsAdmin": true,
	})
}

// handleUIGPUKey stores the vast.ai API key from the nodes page form.
func (s *server) handleUIGPUKey(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	key := r.PostFormValue("api_key")
	if key == "" {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("api key is required"), http.StatusSeeOther)
		return
	}
	if s.sealer == nil {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("sealer is not configured"), http.StatusSeeOther)
		return
	}
	if err := s.storeGPUKey(key); err != nil {
		log.Printf("ui gpu key: %v", err)
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("could not store key"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/nodes", http.StatusSeeOther)
}

// handleUIGPURent rents one offer from the nodes page offers table.
func (s *server) handleUIGPURent(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	offerID, err := strconv.ParseInt(r.PostFormValue("offer_id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("invalid offer id"), http.StatusSeeOther)
		return
	}
	disk, _ := strconv.Atoi(r.PostFormValue("disk_gb"))
	numGPUs, _ := strconv.Atoi(r.PostFormValue("num_gpus"))
	if _, err := s.rentGPU(r.Context(), offerID, disk, r.PostFormValue("gpu_name"), numGPUs); err != nil {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/nodes", http.StatusSeeOther)
}

// handleUIGPUStop destroys one rented instance from the nodes page.
func (s *server) handleUIGPUStop(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("invalid instance id"), http.StatusSeeOther)
		return
	}
	if err := s.destroyGPUInstance(r.Context(), id); err != nil {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/nodes", http.StatusSeeOther)
}
