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
// Per-namespace ResourceQuota/LimitRange (setGPUQuota/setProjectQuota) still
// read and apply the env's project's quota unchanged for now — per-env
// quota is a later refinement; v1 reuses the project's quota everywhere.
func (s *server) ensureEnvNamespace(ctx context.Context, env store.Environment) error {
	return s.ensureNamespace(ctx, env.Namespace)
}

// networkIsolationChanged runs after network_isolation is written via
// setSetting — both handleSetSetting (JSON API) and handleUISettingsSet (UI
// form) call it right after a successful write, mirroring panelDomainChanged.
// It fans the new value out to every existing project's namespace: applying
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
		var applyErr error
		if on {
			applyErr = s.kube.ApplyIsolation(ctx, p.Namespace)
		} else {
			applyErr = s.kube.RemoveIsolation(ctx, p.Namespace)
		}
		if applyErr != nil && !kube.IsNotFound(applyErr) {
			if firstErr == nil {
				firstErr = fmt.Errorf("project %s: %w", p.Name, applyErr)
			}
		}
	}
	return firstErr
}
