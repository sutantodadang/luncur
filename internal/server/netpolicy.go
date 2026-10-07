package server

import (
	"context"
	"fmt"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/store"
)

// isolationOn reports whether the network_isolation setting is "on". Any
// error (unset, store failure) reads as off — isolation is opt-in, so the
// safe default on read failure is "don't apply a policy".
func (s *server) isolationOn() bool {
	v, err := s.st.GetSetting("network_isolation")
	if err != nil {
		return false
	}
	return v == "on"
}

// ensureNamespace is the shared core every lazily-created namespace goes
// through: stamp the namespace (PodSecurity baseline, via
// kube.EnsureNamespace) and, if network_isolation is on, apply the
// project-isolation NetworkPolicy right alongside it. Per-namespace
// ResourceQuota/LimitRange are a separate concern (setGPUQuota/
// setProjectQuota), not part of this choke-point.
func (s *server) ensureNamespace(ctx context.Context, namespace string) error {
	if err := s.kube.EnsureNamespace(ctx, namespace); err != nil {
		return err
	}
	if s.isolationOn() {
		return s.kube.ApplyIsolation(ctx, namespace)
	}
	return nil
}

// ensureProjectNamespace is a thin, namespace-string wrapper over
// ensureNamespace for callers not yet migrated to resolve an explicit
// environment (see Task 7); today that's every caller, and they all pass
// p.Namespace — the project's default (production) environment namespace.
func (s *server) ensureProjectNamespace(ctx context.Context, namespace string) error {
	return s.ensureNamespace(ctx, namespace)
}

// ensureEnvNamespace is ensureProjectNamespace's environment-aware sibling:
// the same PodSecurity/NetworkPolicy isolation lands on env.Namespace
// instead of the project's namespace directly, so a non-default
// environment's namespace goes through the identical choke-point.
// It also applies the project's GPU/CPU/memory budgets there (see
// applyEnvQuotas) — v1 reuses the project's quota in every environment.
func (s *server) ensureEnvNamespace(ctx context.Context, env store.Environment) error {
	if err := s.ensureNamespace(ctx, env.Namespace); err != nil {
		return err
	}
	return s.applyEnvQuotas(ctx, env)
}

// applyEnvQuotas puts the project's GPU and CPU/memory budgets into an
// environment namespace (setGPUQuota/setProjectQuota only reach namespaces
// that already exist, so one created later gets them here). Unset budgets
// are left alone.
func (s *server) applyEnvQuotas(ctx context.Context, env store.Environment) error {
	if env.ProjectID == 0 {
		return nil
	}
	p, err := s.st.GetProjectByID(env.ProjectID)
	if err != nil {
		return fmt.Errorf("get project %d: %w", env.ProjectID, err)
	}
	if p.GPUQuota > 0 {
		if err := s.syncGPUQuotaIn(ctx, env.Namespace, p.GPUQuota, false); err != nil {
			return err
		}
	}
	if p.CPUQuotaMilli > 0 || p.MemQuotaMB > 0 {
		if err := s.syncProjectQuotaIn(ctx, env.Namespace, p.CPUQuotaMilli, p.MemQuotaMB, false); err != nil {
			return err
		}
	}
	return nil
}

// networkIsolationChanged runs after network_isolation is written via
// setSetting — both handleSetSetting (JSON API) and handleUISettingsSet (UI
// form) call it right after a successful write, mirroring panelDomainChanged.
// It fans the new value out to every existing project's environment
// namespaces: applying
// or removing the isolation NetworkPolicy. A project whose namespace hasn't
// been created yet (no deploy so far) is skipped rather than failed —
// ensureProjectNamespace picks up the current setting on its first deploy.
func (s *server) networkIsolationChanged(ctx context.Context) error {
	if s.kube == nil {
		return nil
	}
	on := s.isolationOn()
	projects, err := s.st.ListProjects()
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	var firstErr error
	for _, p := range projects {
		// Every environment's namespace, not just the project (production)
		// one — develop/staging/preview must be isolated too.
		nss, err := s.projectEnvNamespaces(p)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("project %s: %w", p.Name, err)
			}
			continue
		}
		for _, ns := range nss {
			var applyErr error
			if on {
				applyErr = s.kube.ApplyIsolation(ctx, ns)
			} else {
				applyErr = s.kube.RemoveIsolation(ctx, ns)
			}
			if applyErr != nil && !kube.IsNotFound(applyErr) {
				if firstErr == nil {
					firstErr = fmt.Errorf("project %s (%s): %w", p.Name, ns, applyErr)
				}
			}
		}
	}
	return firstErr
}
