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

// GPU cloud rental (vast.ai, Nebius). Credentials are sealed at rest in the
// settings table; instances are tracked in gpu_instances so the idle loop
// can stop billing when nothing schedules on GPUs. All endpoints are
// admin-only: renting hardware spends real money.
//
// Settings keys:
//
//	gpu_vastai_key       sealed vast.ai API key, base64(seal(key))
//	gpu_vastai_image     VM image for rented vast.ai boxes (default ubuntu-22.04)
//	gpu_nebius_creds     sealed Nebius creds JSON, base64(seal(json))
//	gpu_nebius_endpoint  plain, unsealed Nebius API base override; "" = production.
//	                     test seam: lets tests point Nebius at an httptest fake;
//	                     also useful ops-side for a regional endpoint.
//	gpu_idle_minutes     idle window before auto-destroy; "0"/unset = disabled
const (
	settingVastKey        = "gpu_vastai_key"
	settingVastImage      = "gpu_vastai_image"
	settingNebiusCreds    = "gpu_nebius_creds"
	settingNebiusEndpoint = "gpu_nebius_endpoint"
	settingIdleMinutes    = "gpu_idle_minutes"

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

// nebiusCreds is the JSON shape sealed under settingNebiusCreds.
type nebiusCreds struct {
	SAID       string `json:"sa_id"`
	PubKeyID   string `json:"pubkey_id"`
	PrivateKey string `json:"private_key"`
	ParentID   string `json:"parent_id"`
	SubnetID   string `json:"subnet_id"`
}

// nebius returns a configured client, or an error when no credentials are
// stored.
func (s *server) nebius() (*gpucloud.Nebius, error) {
	if s.sealer == nil {
		return nil, errors.New("sealer is not configured")
	}
	v, err := s.st.GetSetting(settingNebiusCreds)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errors.New("nebius credentials are not set (PUT /v1/gpu/key)")
		}
		return nil, err
	}
	sealed, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("stored creds: %w", err)
	}
	raw, err := s.sealer.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("unseal creds: %w", err)
	}
	var creds nebiusCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("stored creds: %w", err)
	}
	// test seam: an optional plain (unsealed) setting points Nebius at a
	// fake server in tests; ops can use it for a regional endpoint too.
	endpoint := ""
	if v, err := s.st.GetSetting(settingNebiusEndpoint); err == nil {
		endpoint = v
	}
	return gpucloud.NewNebius(gpucloud.NebiusConfig{
		ServiceAccountID: creds.SAID,
		PublicKeyID:      creds.PubKeyID,
		PrivateKeyPEM:    []byte(creds.PrivateKey),
		ParentID:         creds.ParentID,
		SubnetID:         creds.SubnetID,
		Endpoint:         endpoint,
	}), nil
}

// storeNebiusCreds seals and persists the Nebius service-account creds.
func (s *server) storeNebiusCreds(c nebiusCreds) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := s.sealer.Seal(raw)
	if err != nil {
		return fmt.Errorf("seal: %w", err)
	}
	return s.st.SetSetting(settingNebiusCreds, base64.StdEncoding.EncodeToString(sealed))
}

// errGPUUnconfigured wraps a gpuProvider factory error so handlers can map
// it to 503 gpu_unconfigured instead of a mid-rent 502 provider_error.
type errGPUUnconfigured struct{ err error }

func (e *errGPUUnconfigured) Error() string { return e.err.Error() }
func (e *errGPUUnconfigured) Unwrap() error  { return e.err }

// gpuProvider resolves a configured client for name ("vastai" or "nebius"),
// or an error identifying what's missing/unknown.
func (s *server) gpuProvider(name string) (gpucloud.Provider, error) {
	switch name {
	case "vastai":
		v, err := s.vast()
		if err != nil {
			return nil, err
		}
		return v, nil
	case "nebius":
		n, err := s.nebius()
		if err != nil {
			return nil, err
		}
		return n, nil
	default:
		return nil, fmt.Errorf("unknown gpu provider %q (supported: vastai, nebius)", name)
	}
}

// handleSetGPUKey stores the provider credentials, sealed. provider defaults
// to "vastai" (back-compat: a body with just api_key and no provider still
// works). provider "nebius" instead expects the five Nebius service-account
// fields, all required.
func (s *server) handleSetGPUKey(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Provider   string `json:"provider"`
		APIKey     string `json:"api_key"`
		SAID       string `json:"sa_id"`
		PubKeyID   string `json:"pubkey_id"`
		PrivateKey string `json:"private_key"`
		ParentID   string `json:"parent_id"`
		SubnetID   string `json:"subnet_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if s.sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		return
	}
	provider := req.Provider
	if provider == "" {
		provider = "vastai"
	}
	switch provider {
	case "vastai":
		if strings.TrimSpace(req.APIKey) == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "api_key is required")
			return
		}
		if err := s.storeGPUKey(req.APIKey); err != nil {
			log.Printf("store gpu key: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	case "nebius":
		c := nebiusCreds{
			SAID: req.SAID, PubKeyID: req.PubKeyID, PrivateKey: req.PrivateKey,
			ParentID: req.ParentID, SubnetID: req.SubnetID,
		}
		var missing []string
		if strings.TrimSpace(c.SAID) == "" {
			missing = append(missing, "sa_id")
		}
		if strings.TrimSpace(c.PubKeyID) == "" {
			missing = append(missing, "pubkey_id")
		}
		if strings.TrimSpace(c.PrivateKey) == "" {
			missing = append(missing, "private_key")
		}
		if strings.TrimSpace(c.ParentID) == "" {
			missing = append(missing, "parent_id")
		}
		if strings.TrimSpace(c.SubnetID) == "" {
			missing = append(missing, "subnet_id")
		}
		if len(missing) > 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "missing required fields: "+strings.Join(missing, ", "))
			return
		}
		if err := s.storeNebiusCreds(c); err != nil {
			log.Printf("store nebius creds: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported provider (vastai, nebius)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": provider, "set": true})
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

// handleRentGPU accepts one offer/preset as a VM that auto-joins the
// cluster. provider defaults to "vastai" (back-compat: offer_id alone still
// works); provider "nebius" instead selects hardware via platform/preset.
func (s *server) handleRentGPU(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Provider string `json:"provider"`
		OfferID  int64  `json:"offer_id"`
		Platform string `json:"platform"`
		Preset   string `json:"preset"`
		DiskGB   int    `json:"disk_gb"`
		GPUName  string `json:"gpu_name"`
		NumGPUs  int    `json:"num_gpus"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	providerName := req.Provider
	if providerName == "" {
		providerName = "vastai"
	}
	switch providerName {
	case "vastai":
		if req.OfferID == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "offer_id is required")
			return
		}
	case "nebius":
		if strings.TrimSpace(req.Platform) == "" || strings.TrimSpace(req.Preset) == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "platform and preset are required")
			return
		}
	}
	if req.DiskGB <= 0 {
		req.DiskGB = 40
	}
	g, err := s.rentGPU(r.Context(), providerName, req.OfferID, req.DiskGB, req.GPUName, req.NumGPUs, req.Platform, req.Preset)
	if err != nil {
		var unconf *errGPUUnconfigured
		if errors.As(err, &unconf) {
			writeError(w, http.StatusServiceUnavailable, "gpu_unconfigured", unconf.Error())
			return
		}
		if errors.Is(err, gpucloud.ErrRentAmbiguous) {
			// The provider accepted the rent but its outcome isn't known
			// yet; still record the row (below) rather than dropping real
			// spend, and tell the operator to check the provider console.
			writeJSON(w, http.StatusAccepted, map[string]any{
				"message":  "rent accepted but outcome is unknown; check the provider console",
				"instance": gpuInstanceJSON(g),
			})
			return
		}
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, gpuInstanceJSON(g))
}

// rentGPU is the rent core shared by the JSON API and the nodes-page form:
// resolve the named provider + node token + VM image, then hand off to
// rentWithProvider to accept the offer/preset and record the contract.
func (s *server) rentGPU(ctx context.Context, providerName string, offerID int64, diskGB int, gpuName string, numGPUs int, platform, preset string) (store.GPUInstance, error) {
	if diskGB <= 0 {
		diskGB = 40
	}
	if providerName == "" {
		providerName = "vastai"
	}
	prov, err := s.gpuProvider(providerName)
	if err != nil {
		return store.GPUInstance{}, &errGPUUnconfigured{err}
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
	spec := gpucloud.RentSpec{
		Label:          label,
		Image:          image,
		DiskGB:         diskGB,
		Onstart:        s.joinOnstart(label, token),
		VastOfferID:    offerID,
		NebiusPlatform: platform,
		NebiusPreset:   preset,
	}
	return s.rentWithProvider(ctx, prov, providerName, spec, label, gpuName, numGPUs)
}

// rentWithProvider accepts spec against prov and records the contract. A
// gpucloud.ErrRentAmbiguous outcome (e.g. a Nebius operation still pending at
// the poll deadline) still gets a row recorded -- status "renting" -- so
// real spend is never silently dropped; the error is still returned
// (unwrapped, so errors.Is(err, gpucloud.ErrRentAmbiguous) works) alongside
// the row so callers can tell the two outcomes apart.
func (s *server) rentWithProvider(ctx context.Context, prov gpucloud.Provider, providerName string, spec gpucloud.RentSpec, label, gpuName string, numGPUs int) (store.GPUInstance, error) {
	ref, err := prov.Rent(ctx, spec)
	if err != nil {
		if errors.Is(err, gpucloud.ErrRentAmbiguous) {
			g, serr := s.st.CreateGPUInstanceWithStatus(providerName, ref, label, gpuName, numGPUs, "renting")
			if serr != nil {
				return store.GPUInstance{}, fmt.Errorf("rent ambiguous (contract unknown) and failed to record it: %w", serr)
			}
			return g, err
		}
		return store.GPUInstance{}, err
	}
	g, err := s.st.CreateGPUInstance(providerName, ref, label, gpuName, numGPUs)
	if err != nil {
		// The rent went through; losing the row must not hide the contract.
		return store.GPUInstance{}, fmt.Errorf("rented (contract %s) but failed to record it: %w", ref, err)
	}
	return g, nil
}

// handleListGPUInstances merges tracked rows with every configured
// provider's live view.
func (s *server) handleListGPUInstances(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.st.ListGPUInstances()
	if err != nil {
		log.Printf("list gpu instances: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	live := map[string]gpucloud.Instance{}
	for _, name := range []string{"vastai", "nebius"} {
		prov, err := s.gpuProvider(name)
		if err != nil {
			continue // not configured; nothing live to merge
		}
		ins, err := prov.List(r.Context())
		if err != nil {
			log.Printf("%s list: %v", name, err)
			continue
		}
		for _, i := range ins {
			live[i.Ref] = i
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
	if err := v.Destroy(ctx, g.ExternalRef); err != nil {
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

// decideIdleDestroys is the pure core of the idle loop, extracted for tests:
// given tracked (non-destroyed) instances, the busy-node set, per-label
// idle-since state, now, and the window, it returns labels to destroy and the
// updated idle-since state.
//
// ponytail: a Pending GPU pod with no NodeName yet freezes ALL destroys this
// tick (busy[""]) rather than tracking per-label state through scheduler
// churn — simplest safe behavior while the scheduler could still place that
// pod on any node.
func decideIdleDestroys(instances []store.GPUInstance, busy map[string]bool, idleSince map[string]time.Time, now time.Time, window time.Duration) (destroy []string, next map[string]time.Time) {
	if busy[""] {
		return nil, idleSince
	}
	next = map[string]time.Time{}
	for _, g := range instances {
		if g.Status == "destroyed" {
			continue
		}
		label := g.Label
		if busy[label] {
			// Busy: don't carry an idle clock — resets on next idle tick.
			continue
		}
		since, ok := idleSince[label]
		if !ok {
			since = now
		}
		next[label] = since
		if now.Sub(since) >= window {
			destroy = append(destroy, label)
		}
	}
	return destroy, next
}

// runGPUIdleLoop destroys rented instances, per instance, after
// gpu_idle_minutes of no GPU pod scheduled on that instance's node.
// Disabled unless the setting is a positive integer.
func (s *server) runGPUIdleLoop(ctx context.Context) {
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
			s.gpuIdleSince = nil
			continue
		}
		list, err := s.st.ListGPUInstances()
		if err != nil {
			continue
		}
		if len(list) == 0 {
			s.gpuIdleSince = nil
			continue
		}
		busy, err := s.kube.GPUBusyNodes(ctx)
		if err != nil {
			log.Printf("gpu idle check: %v", err)
			continue
		}
		destroy, next := decideIdleDestroys(list, busy, s.gpuIdleSince, time.Now(), time.Duration(mins)*time.Minute)
		s.gpuIdleSince = next
		if len(destroy) == 0 {
			continue
		}
		byLabel := make(map[string]store.GPUInstance, len(list))
		for _, g := range list {
			byLabel[g.Label] = g
		}
		for _, label := range destroy {
			g, ok := byLabel[label]
			if !ok {
				continue
			}
			prov, err := s.gpuProvider(g.Provider)
			if err != nil {
				log.Printf("gpu idle destroy %s: %v", g.Label, err)
				continue
			}
			if err := prov.Destroy(ctx, g.ExternalRef); err != nil {
				log.Printf("gpu idle destroy %s: %v", g.Label, err)
				continue
			}
			if err := s.st.MarkGPUInstanceDestroyed(g.ID); err != nil {
				log.Printf("gpu idle mark %s: %v", g.Label, err)
			}
			log.Printf("gpu idle: destroyed %s after %dm without GPU pods", g.Label, mins)
		}
	}
}
