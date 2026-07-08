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

	// GPU cloud card: tracked instances (with live provider status merged
	// across every configured provider), plus a vast.ai offer search when
	// ?gpu= / ?count= arrived from the form (Nebius rents by platform/preset,
	// no offer search).
	hasVastKey := false
	hasNebiusKey := false
	var gpuErr string
	var offers []gpucloud.Offer
	instances, err := s.st.ListGPUInstances()
	if err != nil {
		log.Printf("ui nodes: list gpu instances: %v", err)
	}
	live := map[string]gpucloud.Instance{}
	if v, err := s.vast(); err == nil {
		hasVastKey = true
		if ins, err := v.List(r.Context()); err == nil {
			for _, i := range ins {
				live[i.Ref] = i
			}
		}
		if q := r.URL.Query(); q.Get("gpu") != "" || q.Get("count") != "" {
			n, _ := strconv.Atoi(q.Get("count"))
			offers, err = v.SearchOffers(r.Context(), q.Get("gpu"), n, 10)
			if err != nil {
				gpuErr = err.Error()
			}
		}
	}
	if n, err := s.nebius(); err == nil {
		hasNebiusKey = true
		if ins, err := n.List(r.Context()); err == nil {
			for _, i := range ins {
				live[i.Ref] = i
			}
		}
	}
	rows := make([]map[string]any, 0, len(instances))
	for _, g := range instances {
		m := gpuInstanceJSON(g)
		if li, ok := live[g.ExternalRef]; ok {
			m["provider_status"] = li.Status
			m["dph_total"] = li.DPHTotal
		}
		rows = append(rows, m)
	}

	s.renderPage(w, "nodes.html", map[string]any{
		"User": u, "Nodes": nodes, "Error": kubeErr,
		"HasGPUKey": hasVastKey || hasNebiusKey,
		"HasVastKey": hasVastKey, "HasNebiusKey": hasNebiusKey,
		"GPUInstances": rows, "GPUOffers": offers,
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
	flash(w, "ok", "gpu key saved")
	http.Redirect(w, r, "/ui/nodes", http.StatusSeeOther)
}

// handleUIGPUKeyNebius stores the Nebius service-account credentials from
// the nodes page form. The private key textarea value is never rendered
// back into the page (renderPage only ever receives HasNebiusKey, a bool).
func (s *server) handleUIGPUKeyNebius(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if s.sealer == nil {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("sealer is not configured"), http.StatusSeeOther)
		return
	}
	c := nebiusCreds{
		SAID:       r.PostFormValue("sa_id"),
		PubKeyID:   r.PostFormValue("pubkey_id"),
		PrivateKey: r.PostFormValue("private_key"),
		ParentID:   r.PostFormValue("parent_id"),
		SubnetID:   r.PostFormValue("subnet_id"),
	}
	var missing []string
	if c.SAID == "" {
		missing = append(missing, "sa_id")
	}
	if c.PubKeyID == "" {
		missing = append(missing, "pubkey_id")
	}
	if c.PrivateKey == "" {
		missing = append(missing, "private_key")
	}
	if c.ParentID == "" {
		missing = append(missing, "parent_id")
	}
	if c.SubnetID == "" {
		missing = append(missing, "subnet_id")
	}
	if len(missing) > 0 {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("missing required fields"), http.StatusSeeOther)
		return
	}
	if err := s.storeNebiusCreds(c); err != nil {
		log.Printf("ui nebius key: %v", err)
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("could not store credentials"), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "gpu key saved")
	http.Redirect(w, r, "/ui/nodes", http.StatusSeeOther)
}

// handleUIGPURent rents one offer (vast.ai) or platform/preset (Nebius) from
// the nodes page rent form. provider defaults to "vastai" (back-compat: the
// offer table's hidden inputs carry no provider field).
func (s *server) handleUIGPURent(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	provider := r.PostFormValue("provider")
	if provider == "" {
		provider = "vastai"
	}
	disk, _ := strconv.Atoi(r.PostFormValue("disk_gb"))
	numGPUs, _ := strconv.Atoi(r.PostFormValue("num_gpus"))

	var offerID int64
	var platform, preset string
	switch provider {
	case "nebius":
		platform = r.PostFormValue("platform")
		preset = r.PostFormValue("preset")
		if platform == "" || preset == "" {
			http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("platform and preset are required"), http.StatusSeeOther)
			return
		}
	default:
		var err error
		offerID, err = strconv.ParseInt(r.PostFormValue("offer_id"), 10, 64)
		if err != nil {
			http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape("invalid offer id"), http.StatusSeeOther)
			return
		}
	}
	if _, err := s.rentGPU(r.Context(), provider, offerID, disk, r.PostFormValue("gpu_name"), numGPUs, platform, preset); err != nil {
		http.Redirect(w, r, "/ui/nodes?gpu_err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "gpu instance rented")
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
	flash(w, "ok", "gpu instance stopped")
	http.Redirect(w, r, "/ui/nodes", http.StatusSeeOther)
}
