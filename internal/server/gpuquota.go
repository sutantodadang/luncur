package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/gpu"
	"github.com/sutantodadang/luncur/internal/kube"
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
// core: persist the quota, then sync the ResourceQuota in every one of the
// project's environment namespaces — applying it when n>0, deleting it when
// n==0 (unlimited). No-ops the kube sync when s.kube is nil (tests, or kube
// unavailable at boot).
func (s *server) setGPUQuota(ctx context.Context, p store.Project, n int64) error {
	if err := s.st.SetProjectGPUQuota(p.ID, n); err != nil {
		return err
	}
	if s.kube == nil {
		return nil
	}
	nss, err := s.projectEnvNamespaces(p)
	if err != nil {
		return &gpuQuotaKubeError{err}
	}
	for _, ns := range nss {
		if err := s.syncGPUQuotaIn(ctx, ns, n, ns == p.Namespace); err != nil {
			return &gpuQuotaKubeError{err}
		}
	}
	return nil
}

// projectEnvNamespaces lists every namespace a project's workloads can run
// in: the project namespace (also the production environment's) first, then
// each other environment's luncur-<project>-<env>. Budgets are enforced in
// all of them — otherwise a non-production environment runs unlimited.
func (s *server) projectEnvNamespaces(p store.Project) ([]string, error) {
	envs, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	out := []string{p.Namespace}
	seen := map[string]bool{p.Namespace: true}
	for _, e := range envs {
		if e.Namespace != "" && !seen[e.Namespace] {
			seen[e.Namespace] = true
			out = append(out, e.Namespace)
		}
	}
	return out, nil
}

// syncGPUQuotaIn applies (n>0) or removes (n==0, best-effort) the GPU
// ResourceQuota in one namespace. create makes sure the namespace exists
// first; without it a namespace that hasn't been created yet is skipped —
// ensureEnvNamespace applies the budget when it first creates it.
func (s *server) syncGPUQuotaIn(ctx context.Context, ns string, n int64, create bool) error {
	if n <= 0 {
		if err := s.kube.DeleteObject(ctx, ns, "ResourceQuota", gpu.QuotaObjectName); err != nil {
			log.Printf("delete gpu quota in %s: %v", ns, err)
		}
		return nil
	}
	obj, err := gpu.QuotaObject(ns, n)
	if err != nil {
		return err
	}
	if create {
		if err := s.ensureNamespace(ctx, ns); err != nil {
			return err
		}
	}
	if err := s.kube.Apply(ctx, ns, []render.Object{obj}); err != nil && (create || !kube.IsNotFound(err)) {
		return err
	}
	return nil
}

// syncProjectQuotaIn is syncGPUQuotaIn for the CPU/memory ResourceQuota +
// LimitRange pair (both 0 = unlimited = remove).
func (s *server) syncProjectQuotaIn(ctx context.Context, ns string, cpuMilli, memMB int64, create bool) error {
	if cpuMilli <= 0 && memMB <= 0 {
		if err := s.kube.DeleteObject(ctx, ns, "ResourceQuota", render.ProjectQuotaName); err != nil {
			log.Printf("delete project quota in %s: %v", ns, err)
		}
		if err := s.kube.DeleteObject(ctx, ns, "LimitRange", render.LimitRangeName); err != nil {
			log.Printf("delete project limitrange in %s: %v", ns, err)
		}
		return nil
	}
	objs, err := render.ProjectQuotaObjects(ns, cpuMilli, memMB)
	if err != nil {
		return err
	}
	if create {
		if err := s.ensureNamespace(ctx, ns); err != nil {
			return err
		}
	}
	if err := s.kube.Apply(ctx, ns, objs); err != nil && (create || !kube.IsNotFound(err)) {
		return err
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

// setProjectQuota is handleSetProjectQuota's shared core, mirroring
// setGPUQuota exactly: persist the CPU/memory budget, then sync the
// ResourceQuota+LimitRange in every environment namespace — applying them when either is set,
// deleting both (best-effort) when both are 0 (unlimited). No-ops the kube
// sync when s.kube is nil.
func (s *server) setProjectQuota(ctx context.Context, p store.Project, cpuMilli, memMB int64) error {
	if err := s.st.SetProjectResourceQuota(p.ID, cpuMilli, memMB); err != nil {
		return err
	}
	if s.kube == nil {
		return nil
	}
	nss, err := s.projectEnvNamespaces(p)
	if err != nil {
		return &gpuQuotaKubeError{err}
	}
	for _, ns := range nss {
		if err := s.syncProjectQuotaIn(ctx, ns, cpuMilli, memMB, ns == p.Namespace); err != nil {
			return &gpuQuotaKubeError{err}
		}
	}
	return nil
}

// handleSetProjectQuota sets a project's namespace CPU/memory budget and
// syncs the ResourceQuota+LimitRange via setProjectQuota.
func (s *server) handleSetProjectQuota(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		CPUMilli int64 `json:"cpu_milli"`
		MemoryMB int64 `json:"memory_mb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := s.setProjectQuota(r.Context(), p, req.CPUMilli, req.MemoryMB); err != nil {
		var ke *gpuQuotaKubeError
		if errors.As(err, &ke) {
			log.Printf("apply project quota %s: %v", p.Name, err)
			writeError(w, http.StatusBadGateway, "kube_error", "quota stored but cluster apply failed: "+ke.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": p.Name, "cpu_milli": req.CPUMilli, "memory_mb": req.MemoryMB,
	})
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
