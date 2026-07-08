package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/gpu"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// gpuQuotaKubeError wraps a cluster-apply/delete failure from setGPUQuota so
// callers can tell it apart from a store validation failure (e.g. a
// negative quota) — both would otherwise be plain errors — and map it to a
// 502 instead of a 400.
type gpuQuotaKubeError struct{ err error }

func (e *gpuQuotaKubeError) Error() string { return e.err.Error() }
func (e *gpuQuotaKubeError) Unwrap() error { return e.err }

// setGPUQuota is handleSetGPUQuota's (and the UI twin's, added later) shared
// core: persist the quota, then sync the namespace ResourceQuota —
// applying it when n>0, deleting it when n==0 (unlimited). No-ops the kube
// sync when s.kube is nil (tests, or kube unavailable at boot).
func (s *server) setGPUQuota(ctx context.Context, p store.Project, n int64) error {
	if err := s.st.SetProjectGPUQuota(p.ID, n); err != nil {
		return err
	}
	if s.kube == nil {
		return nil
	}
	if n > 0 {
		obj, err := gpu.QuotaObject(p.Namespace, n)
		if err != nil {
			return &gpuQuotaKubeError{err}
		}
		if err := s.ensureProjectNamespace(ctx, p.Namespace); err != nil {
			return &gpuQuotaKubeError{err}
		}
		if err := s.kube.Apply(ctx, p.Namespace, []render.Object{obj}); err != nil {
			return &gpuQuotaKubeError{err}
		}
		return nil
	}
	// Unlimited: best-effort remove the ResourceQuota. A delete failure
	// doesn't invalidate the (already-persisted) quota=0, so it's logged
	// rather than surfaced as a request error.
	if err := s.kube.DeleteObject(ctx, p.Namespace, "ResourceQuota", gpu.QuotaObjectName); err != nil {
		log.Printf("delete gpu quota %s: %v", p.Name, err)
	}
	return nil
}

// handleSetGPUQuota sets a project's GPU budget and syncs the namespace
// ResourceQuota via setGPUQuota.
func (s *server) handleSetGPUQuota(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Quota int64 `json:"quota"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := s.setGPUQuota(r.Context(), p, req.Quota); err != nil {
		var ke *gpuQuotaKubeError
		if errors.As(err, &ke) {
			log.Printf("apply gpu quota %s: %v", p.Name, err)
			writeError(w, http.StatusBadGateway, "kube_error", "quota stored but cluster apply failed: "+ke.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": p.Name, "gpu_quota": req.Quota})
}

// validateGPUBudget rejects a change that would push the project's summed
// GPU requests past its quota. addGPUs is the delta the request introduces
// (gpu × effective-replicas for the app being created/changed). Quota <= 0
// means unlimited.
func (s *server) validateGPUBudget(p store.Project, addGPUs int64) error {
	if addGPUs <= 0 || p.GPUQuota <= 0 {
		return nil
	}
	sum, err := s.st.SumProjectGPURequests(p.ID)
	if err != nil {
		return fmt.Errorf("gpu budget check: %w", err)
	}
	if sum+addGPUs > p.GPUQuota {
		return fmt.Errorf("project GPU budget is %d, this change needs %d more (already planned: %d) — raise the quota or free GPUs", p.GPUQuota, addGPUs, sum)
	}
	return nil
}

// gpuEffReplicas mirrors store.SumProjectGPURequests' replica counting: a
// cron app's GPUs count once regardless of replicas (cron has none in the
// usual sense); everything else counts at least 1, so a replicas=0 app
// still budgets for the single instance it would get once scaled up.
func gpuEffReplicas(kind string, replicas int) int64 {
	if kind == "cron" || replicas < 1 {
		return 1
	}
	return int64(replicas)
}
