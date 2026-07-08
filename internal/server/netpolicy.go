package server

import (
	"context"
	"fmt"

	"github.com/sutantodadang/luncur/internal/kube"
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

// ensureProjectNamespace is the single choke-point every lazily-created
// project namespace goes through, so the isolation NetworkPolicy always
// rides along with namespace creation instead of needing a separate pass.
func (s *server) ensureProjectNamespace(ctx context.Context, namespace string) error {
	if err := s.kube.EnsureNamespace(ctx, namespace); err != nil {
		return err
	}
	if s.isolationOn() {
		return s.kube.ApplyIsolation(ctx, namespace)
	}
	return nil
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
