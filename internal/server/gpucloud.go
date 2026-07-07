package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/gpucloud"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
	"github.com/sutantodadang/luncur/internal/up"
)

// GPU cloud rental (vast.ai). The API key is sealed at rest in the settings
// table; instances are tracked in gpu_instances so the idle loop can stop
// billing when nothing schedules on GPUs. All endpoints are admin-only:
// renting hardware spends real money.
//
// Settings keys:
//
//	gpu_vastai_key    sealed API key, base64(seal(key))
//	gpu_vastai_image  VM image for rented boxes (default ubuntu-22.04)
//	gpu_idle_minutes  idle window before auto-destroy; "0"/unset = disabled
const (
	settingVastKey     = "gpu_vastai_key"
	settingVastImage   = "gpu_vastai_image"
	settingIdleMinutes = "gpu_idle_minutes"

	// ponytail: default image name is a best-guess vast.ai VM template;
	// verify on the first real rent and override via the setting if wrong.
	defaultVastImage = "ubuntu-22.04"
)

// vast returns a configured client, or an error when no key is stored.
func (s *server) vast() (*gpucloud.VastAI, error) {
	if s.sealer == nil {
		return nil, errors.New("sealer is not configured")
	}
	v, err := s.st.GetSetting(settingVastKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errors.New("vast.ai API key is not set (PUT /v1/gpu/key)")
		}
		return nil, err
	}
	sealed, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("stored key: %w", err)
	}
	key, err := s.sealer.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("unseal key: %w", err)
	}
	return &gpucloud.VastAI{APIKey: string(key), BaseURL: s.vastBaseURL}, nil
}

// handleSetGPUKey stores the provider API key, sealed.
func (s *server) handleSetGPUKey(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Provider != "vastai" {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported provider (vastai)")
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "api_key is required")
		return
	}
	if s.sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		return
	}
	if err := s.storeGPUKey(req.APIKey); err != nil {
		log.Printf("store gpu key: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": req.Provider, "set": true})
}

// storeGPUKey seals and persists the provider API key. Shared by the JSON
// API and the nodes-page form.
func (s *server) storeGPUKey(apiKey string) error {
	sealed, err := s.sealer.Seal([]byte(apiKey))
	if err != nil {
		return fmt.Errorf("seal: %w", err)
	}
	return s.st.SetSetting(settingVastKey, base64.StdEncoding.EncodeToString(sealed))
}

// handleGPUOffers proxies a cheapest-first offer search.
func (s *server) handleGPUOffers(w http.ResponseWriter, r *http.Request, u store.User) {
	v, err := s.vast()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gpu_unconfigured", err.Error())
		return
	}
	n := 0
	if q := r.URL.Query().Get("num_gpus"); q != "" {
		n, _ = strconv.Atoi(q)
	}
	limit := 10
	if q := r.URL.Query().Get("limit"); q != "" {
		limit, _ = strconv.Atoi(q)
	}
	offers, err := v.SearchOffers(r.Context(), r.URL.Query().Get("gpu_name"), n, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"offers": offers})
}

// joinOnstart is the boot script for a rented VM: write the registry
// mirror config, then install the K3s agent pointed at this server with the
// GPU node label. Drivers come with the provider's VM image; the device
// plugin DaemonSet (deployed by C1) advertises the GPUs once the node is up.
func (s *server) joinOnstart(label, token string) string {
	return "#!/bin/bash\nset -e\n" +
		"mkdir -p /etc/rancher/k3s\n" +
		"cat > /etc/rancher/k3s/registries.yaml <<'LUNCUR_REG'\n" + up.RegistriesYAML() + "\nLUNCUR_REG\n" +
		"curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC=\"agent\" " +
		"K3S_URL=\"https://" + s.externalIP + ":6443\" " +
		"K3S_TOKEN=\"" + token + "\" " +
		"K3S_NODE_NAME=\"" + label + "\" " +
		"sh -s - --node-label " + render.GPUNodeLabelKey + "=" + render.GPUNodeLabelValue + "\n"
}

// nodeToken reads the K3s server node token used by agents to join.
func (s *server) nodeToken() (string, error) {
	path := s.nodeTokenPath
	if path == "" {
		path = up.NodeTokenPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("node token: %w (is this the K3s server?)", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// handleRentGPU accepts one offer as a VM that auto-joins the cluster.
func (s *server) handleRentGPU(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		OfferID int64  `json:"offer_id"`
		DiskGB  int    `json:"disk_gb"`
		GPUName string `json:"gpu_name"`
		NumGPUs int    `json:"num_gpus"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.OfferID == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "offer_id is required")
		return
	}
	if req.DiskGB <= 0 {
		req.DiskGB = 40
	}
	g, err := s.rentGPU(r.Context(), req.OfferID, req.DiskGB, req.GPUName, req.NumGPUs)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, gpuInstanceJSON(g))
}

// rentGPU is the rent core shared by the JSON API and the nodes-page form:
// resolve client + node token + VM image, accept the offer with a K3s-join
// onstart script, record the contract.
func (s *server) rentGPU(ctx context.Context, offerID int64, diskGB int, gpuName string, numGPUs int) (store.GPUInstance, error) {
	if diskGB <= 0 {
		diskGB = 40
	}
	v, err := s.vast()
	if err != nil {
		return store.GPUInstance{}, err
	}
	token, err := s.nodeToken()
	if err != nil {
		return store.GPUInstance{}, err
	}
	image, err := s.st.GetSetting(settingVastImage)
	if errors.Is(err, store.ErrNotFound) || image == "" {
		image = defaultVastImage
	} else if err != nil {
		return store.GPUInstance{}, err
	}
	label := fmt.Sprintf("luncur-gpu-%d", time.Now().Unix())
	extID, err := v.Rent(ctx, gpucloud.RentSpec{
		OfferID: offerID,
		Image:   image,
		DiskGB:  diskGB,
		Label:   label,
		Onstart: s.joinOnstart(label, token),
	})
	if err != nil {
		return store.GPUInstance{}, err
	}
	g, err := s.st.CreateGPUInstance("vastai", strconv.FormatInt(extID, 10), label, gpuName, numGPUs)
	if err != nil {
		// The rent went through; losing the row must not hide the contract.
		return store.GPUInstance{}, fmt.Errorf("rented (contract %d) but failed to record it: %w", extID, err)
	}
	return g, nil
}

// handleListGPUInstances merges tracked rows with the provider's live view.
func (s *server) handleListGPUInstances(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.st.ListGPUInstances()
	if err != nil {
		log.Printf("list gpu instances: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	live := map[string]gpucloud.Instance{}
	if v, err := s.vast(); err == nil {
		if ins, err := v.List(r.Context()); err == nil {
			for _, i := range ins {
				live[strconv.FormatInt(i.ID, 10)] = i
			}
		} else {
			log.Printf("vast list: %v", err)
		}
	}
	out := make([]map[string]any, 0, len(list))
	for _, g := range list {
		m := gpuInstanceJSON(g)
		if li, ok := live[g.ExternalRef]; ok {
			m["provider_status"] = li.Status
			m["dph_total"] = li.DPHTotal
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDestroyGPUInstance deletes the VM at the provider, then marks the row.
func (s *server) handleDestroyGPUInstance(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	g, err := s.st.GetGPUInstance(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "gpu instance not found")
		return
	}
	if err != nil {
		log.Printf("get gpu instance: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.destroyGPUInstance(r.Context(), id); err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"destroyed": true, "label": g.Label})
}

// destroyGPUInstance is the delete core shared by the JSON API and the
// nodes-page stop button.
func (s *server) destroyGPUInstance(ctx context.Context, id int64) error {
	g, err := s.st.GetGPUInstance(id)
	if err != nil {
		return err
	}
	v, err := s.vast()
	if err != nil {
		return err
	}
	extID, err := strconv.ParseInt(g.ExternalRef, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid vast.ai external ref %q: %w", g.ExternalRef, err)
	}
	if err := v.Destroy(ctx, extID); err != nil {
		return err
	}
	if err := s.st.MarkGPUInstanceDestroyed(g.ID); err != nil {
		log.Printf("mark gpu instance destroyed: %v", err)
	}
	return nil
}

func gpuInstanceJSON(g store.GPUInstance) map[string]any {
	return map[string]any{
		"id": g.ID, "provider": g.Provider, "external_id": g.ExternalRef,
		"label": g.Label, "gpu_name": g.GPUName, "num_gpus": g.NumGPUs,
		"status": g.Status, "created_at": g.CreatedAt,
	}
}

// runGPUIdleLoop destroys rented instances after gpu_idle_minutes of no pod
// requesting GPUs. Disabled unless the setting is a positive integer.
// ponytail: one global idle window across all instances — per-instance
// tracking when someone actually runs mixed always-on + burst fleets.
func (s *server) runGPUIdleLoop(ctx context.Context) {
	var idleSince time.Time
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		mins := 0
		if v, err := s.st.GetSetting(settingIdleMinutes); err == nil {
			mins, _ = strconv.Atoi(v)
		}
		if mins <= 0 || s.kube == nil {
			idleSince = time.Time{}
			continue
		}
		list, err := s.st.ListGPUInstances()
		if err != nil || len(list) == 0 {
			idleSince = time.Time{}
			continue
		}
		busy, err := s.kube.GPUPodsRequested(ctx)
		if err != nil {
			log.Printf("gpu idle check: %v", err)
			continue
		}
		if busy {
			idleSince = time.Time{}
			continue
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
			continue
		}
		if time.Since(idleSince) < time.Duration(mins)*time.Minute {
			continue
		}
		v, err := s.vast()
		if err != nil {
			continue
		}
		for _, g := range list {
			extID, err := strconv.ParseInt(g.ExternalRef, 10, 64)
			if err != nil {
				log.Printf("gpu idle destroy %s: invalid external ref %q: %v", g.Label, g.ExternalRef, err)
				continue
			}
			if err := v.Destroy(ctx, extID); err != nil {
				log.Printf("gpu idle destroy %s: %v", g.Label, err)
				continue
			}
			if err := s.st.MarkGPUInstanceDestroyed(g.ID); err != nil {
				log.Printf("gpu idle mark %s: %v", g.Label, err)
			}
			log.Printf("gpu idle: destroyed %s after %dm without GPU pods", g.Label, mins)
		}
		idleSince = time.Time{}
	}
}
