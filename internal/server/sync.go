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

// appURL is the app's public URL shown in the UI/API and sent in deploy
// notifications: its first non-wildcard custom domain when one exists
// (https once the cert is issued or externally managed), else the assigned
// sslip.io host over plain HTTP.
func (s *server) appURL(a store.App) string {
	if domains, err := s.st.ListDomains(a.ID); err == nil {
		for _, d := range domains {
			if strings.HasPrefix(d.Hostname, "*.") {
				continue
			}
			if d.CertStatus == "issued" || d.CertStatus == "external" {
				return "https://" + d.Hostname
			}
			return "http://" + d.Hostname
		}
	}
	return "http://" + hostFor(a.Name, s.externalIP)
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
	return s.renderAppWithRun(p, a, imageRef, withOverrides, "", a.Nodes, a.Framework, nil)
}

// renderRunWith renders one triggered run of a kind=job app with per-run
// overrides of nodes/framework/env — startRun's core, shared by the JSON
// API and the UI run-now button.
func (s *server) renderRunWith(p store.Project, a store.App, imageRef string, runID int64, nodes int, framework string, runEnv map[string]string) (render.Rendered, error) {
	return s.renderAppWithRun(p, a, imageRef, true, jobRunName(a.Name, runID), nodes, framework, runEnv)
}

func (s *server) renderAppWithRun(p store.Project, a store.App, imageRef string, withOverrides bool, runName string, nodes int, framework string, runEnv map[string]string) (render.Rendered, error) {
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

	// Opt-in external S3: LUNCUR_S3_* from the project's stored config.
	// User env (and an attached minio addon, injected above) wins per key.
	if a.InjectS3 || (a.Kind == "model" && strings.HasPrefix(a.ModelSource, "s3:")) {
		cfg, err := s.st.GetProjectS3(p.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Flag on but nothing configured: nothing to inject.
		case err != nil:
			return render.Rendered{}, fmt.Errorf("project s3: %w", err)
		default:
			if s.sealer == nil {
				return render.Rendered{}, fmt.Errorf("cannot unseal project s3 keys: no sealer configured")
			}
			ak, err := s.sealer.Open(cfg.AccessKeyEnc)
			if err != nil {
				return render.Rendered{}, fmt.Errorf("unseal s3 access key: %w", err)
			}
			sk, err := s.sealer.Open(cfg.SecretKeyEnc)
			if err != nil {
				return render.Rendered{}, fmt.Errorf("unseal s3 secret key: %w", err)
			}
			s3env := map[string]string{
				"LUNCUR_S3_ENDPOINT": cfg.Endpoint,
				"LUNCUR_S3_KEY":      string(ak),
				"LUNCUR_S3_SECRET":   string(sk),
				"LUNCUR_S3_BUCKET":   cfg.Bucket,
			}
			if cfg.Region != "" {
				s3env["LUNCUR_S3_REGION"] = cfg.Region
			}
			for k, v := range s3env {
				if _, taken := env[k]; !taken {
					env[k] = v
				}
			}
		}
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
	host := hostFor(a.Name, s.externalIP)
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
		// A custom domain replaces the assigned sslip.io host entirely once a
		// routable (non-wildcard) one exists; wildcard-only apps keep the sslip
		// host because appURL still points there.
		for i, h := range extraHosts {
			if !strings.HasPrefix(h, "*.") {
				host = h
				extraHosts = append(extraHosts[:i], extraHosts[i+1:]...)
				break
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
		Host:               host,
		Port:               int32(a.Port),
		Replicas:           int32(a.Replicas),
		Kind:               a.Kind,
		Schedule:           a.Schedule,
		CPUMilli:           a.CPUMilli,
		MemoryMB:           a.MemoryMB,
		GPU:                a.GPUCount,
		RunName:            runName,
		ModelSource:        a.ModelSource,
		Runtime:            a.Runtime,
		HealthPath:         a.HealthPath,
		Internal:           a.Internal,
		Overrides:          overrides,
		ExtraHosts:         extraHosts,
		IngressAnnotations: annotations,
		TLS:                tls,
		Volumes:            renderVols,
		Nodes:              int32(nodes),
		Framework:          framework,
		RunEnv:             runEnv,
		AutoMin:            int32(a.AutoMin),
		AutoMax:            int32(a.AutoMax),
		AutoCPU:            int32(a.AutoCPU),
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
