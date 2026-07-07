package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	netv1 "k8s.io/api/networking/v1"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// hostFor builds the sslip.io hostname luncur assigns each app: the app
// name, then the external IP with dots swapped for dashes (sslip.io
// resolves that pattern back to the IP without any DNS setup).
func hostFor(app, externalIP string) string {
	return app + "." + strings.ReplaceAll(externalIP, ".", "-") + ".sslip.io"
}

// internalURLFor is the cluster-internal address an internal web app is
// reachable at: the ClusterIP Service's in-cluster DNS name (same as the
// Service name render.go assigns — in.AppName), port 80 (the Service port;
// it forwards to the container port). Reachable from any pod in the
// cluster, not just the app's own namespace, because it's the full
// <service>.<namespace> form rather than the short <service> form.
func internalURLFor(appName, namespace string) string {
	return fmt.Sprintf("http://%s.%s:80", appName, namespace)
}

// plainEnv unseals an app's stored env vars to plaintext. Shared by
// renderApp and addon attach's collision check (addonEnv).
func (s *server) plainEnv(a store.App) (map[string]string, error) {
	sealedEnv, err := s.st.ListEnv(a.ID)
	if err != nil {
		return nil, fmt.Errorf("list env: %w", err)
	}
	if len(sealedEnv) > 0 && s.sealer == nil {
		return nil, fmt.Errorf("cannot unseal env: no sealer configured")
	}
	env := make(map[string]string, len(sealedEnv))
	for k, sealed := range sealedEnv {
		plain, err := s.sealer.Open(sealed)
		if err != nil {
			return nil, fmt.Errorf("unseal env %q: %w", k, err)
		}
		env[k] = string(plain)
	}
	return env, nil
}

// renderApp assembles the manifest set for one app: unseals its env vars,
// injects connection env for attached addons, loads its overrides, and
// renders against imageRef.
func (s *server) renderApp(p store.Project, a store.App, imageRef string, withOverrides bool) (render.Rendered, error) {
	env, err := s.plainEnv(a)
	if err != nil {
		return render.Rendered{}, err
	}

	addonOut, collisions, err := s.addonEnv(p, a, env)
	if err != nil {
		return render.Rendered{}, fmt.Errorf("addon env: %w", err)
	}
	for k, v := range addonOut {
		env[k] = v
	}
	if len(collisions) > 0 {
		log.Printf("app %s: addon env collides with user env, user wins: %s", a.Name, strings.Join(collisions, ", "))
	}

	// Buildpack contract: apps built from source (nixpacks, most
	// Dockerfiles) bind to $PORT. Without it they fall back to their own
	// default (8000, 3000, ...) while the Service targets a.Port and every
	// request 502s. User-set PORT wins, like addon env.
	if _, taken := env["PORT"]; !taken && a.Port > 0 {
		env["PORT"] = strconv.Itoa(a.Port)
	}

	overrides := map[string]string{}
	if withOverrides {
		overrides, err = s.st.Overrides(a.ID)
		if err != nil {
			return render.Rendered{}, fmt.Errorf("load overrides: %w", err)
		}
	}

	vols, err := s.st.ListVolumes(a.ID)
	if err != nil {
		return render.Rendered{}, fmt.Errorf("list volumes: %w", err)
	}
	var renderVols []render.Volume
	for _, v := range vols {
		renderVols = append(renderVols, render.Volume{Name: v.Name, Path: v.Path, SizeGB: v.SizeGB})
	}

	var extraHosts []string
	var tls []netv1.IngressTLS
	annotations := map[string]string{}
	// Domains/TLS/cert-provider annotations only apply to web apps: worker
	// and cron kinds render no Service/Ingress to attach them to.
	if a.Kind == "web" {
		domains, err := s.st.ListDomains(a.ID)
		if err != nil {
			return render.Rendered{}, fmt.Errorf("list domains: %w", err)
		}
		provider := s.certProviderName()
		for _, d := range domains {
			extraHosts = append(extraHosts, d.Hostname)
			switch provider {
			case "builtin":
				if d.CertStatus == "issued" {
					tls = append(tls, netv1.IngressTLS{
						Hosts: []string{d.Hostname}, SecretName: certSecretName(a.Name, d.Hostname),
					})
				}
			case "cert-manager":
				tls = append(tls, netv1.IngressTLS{
					Hosts: []string{d.Hostname}, SecretName: certSecretName(a.Name, d.Hostname),
				})
			}
		}
		if len(domains) > 0 {
			switch provider {
			case "traefik":
				annotations["traefik.ingress.kubernetes.io/router.tls"] = "true"
				annotations["traefik.ingress.kubernetes.io/router.tls.certresolver"] = "le"
			case "cert-manager":
				annotations["cert-manager.io/cluster-issuer"] = "luncur-le"
			}
		}
	}
	if len(annotations) == 0 {
		annotations = nil
	}

	in := render.Input{
		AppName:            a.Name,
		Namespace:          p.Namespace,
		Image:              imageRef,
		Host:               hostFor(a.Name, s.externalIP),
		Port:               int32(a.Port),
		Replicas:           int32(a.Replicas),
		Kind:               a.Kind,
		Schedule:           a.Schedule,
		CPUMilli:           a.CPUMilli,
		MemoryMB:           a.MemoryMB,
		GPU:                a.GPUCount,
		HealthPath:         a.HealthPath,
		Internal:           a.Internal,
		Overrides:          overrides,
		ExtraHosts:         extraHosts,
		IngressAnnotations: annotations,
		TLS:                tls,
		Volumes:            renderVols,
	}
	return render.Render(in, env)
}

// syncApp re-applies an app's current state to the cluster, using the image
// from its latest deployment. If there is no deployment, or the latest one
// isn't live, there is nothing running to sync — that's a no-op, not an
// error.
func (s *server) syncApp(ctx context.Context, p store.Project, a store.App) error {
	if a.Ejected {
		return nil
	}
	d, err := s.st.LatestDeployment(a.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if d.Status != "live" {
		return nil
	}

	rendered, err := s.renderApp(p, a, d.ImageRef, true)
	if err != nil {
		return err
	}
	if err := s.kube.EnsureNamespace(ctx, p.Namespace); err != nil {
		return err
	}
	return s.kube.Apply(ctx, p.Namespace, rendered.Objects)
}

// syncIfLive re-applies an app's manifests if kube is configured and the
// app's latest deployment is live. Used after env/override mutations so
// running apps pick up the change without requiring an explicit deploy.
// Any error is logged, never surfaced — these are opportunistic syncs.
func (s *server) syncIfLive(ctx context.Context, p store.Project, a store.App) {
	if a.Ejected {
		return
	}
	if s.kube == nil {
		return
	}
	d, err := s.st.LatestDeployment(a.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("latest deployment: %v", err)
		}
		return
	}
	if d.Status != "live" {
		return
	}
	if err := s.syncApp(ctx, p, a); err != nil {
		log.Printf("sync app: %v", err)
	}
}
