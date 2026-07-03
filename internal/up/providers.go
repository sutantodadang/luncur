package up

import (
	"encoding/json"

	"sigs.k8s.io/yaml"

	"github.com/sutantodadang/luncur/internal/render"
)

// TraefikACMEConfig returns the HelmChartConfig that enables Traefik's
// built-in ACME "le" resolver on K3s (K3s reconciles Traefik from this
// HelmChartConfig — it is a K3s-only mechanism, not vanilla Traefik).
func TraefikACMEConfig(email string) (render.Object, error) {
	values := map[string]any{
		"persistence": map[string]any{
			"enabled": true,
		},
		"additionalArguments": []string{
			"--certificatesresolvers.le.acme.email=" + email,
			"--certificatesresolvers.le.acme.storage=/data/acme.json",
			"--certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
		},
	}
	valuesYAML, err := yaml.Marshal(values)
	if err != nil {
		return render.Object{}, err
	}
	obj := map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChartConfig",
		"metadata": map[string]any{
			"name":      "traefik",
			"namespace": "kube-system",
		},
		"spec": map[string]any{
			"valuesContent": string(valuesYAML),
		},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return render.Object{}, err
	}
	return render.Object{Kind: "HelmChartConfig", JSON: b}, nil
}

// ClusterIssuer returns the cert-manager ClusterIssuer luncur relies on when
// the cert-manager provider is selected: Let's Encrypt production, HTTP-01
// via the traefik Ingress class.
func ClusterIssuer(email string) (render.Object, error) {
	spec := map[string]any{
		"acme": map[string]any{
			"email":               email,
			"server":              "https://acme-v02.api.letsencrypt.org/directory",
			"privateKeySecretRef": map[string]any{"name": "luncur-le-account"},
			"solvers": []map[string]any{{
				"http01": map[string]any{"ingress": map[string]any{"class": "traefik"}},
			}},
		},
	}
	obj := map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata": map[string]any{
			"name": "luncur-le",
		},
		"spec": spec,
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return render.Object{}, err
	}
	return render.Object{Kind: "ClusterIssuer", JSON: b}, nil
}
