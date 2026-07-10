package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/client"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/up"
)

func defaultImage() string {
	tag := version
	if tag == "dev" {
		tag = "latest"
	}
	return "ghcr.io/sutantodadang/luncur:" + tag
}

func upCmd() *cobra.Command {
	var ip, image, builderImage, kubeconfig, certProvider, acmeEmail string
	var replicaURL, replicaEndpoint, replicaAccessKey, replicaSecretKey string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Install (or repair) luncur on this machine's K3s",
		RunE: func(cmd *cobra.Command, args []string) error {
			if replicaURL != "" {
				if replicaAccessKey == "" || replicaSecretKey == "" {
					return fmt.Errorf("--replica-url requires --replica-access-key and --replica-secret-key")
				}
			} else if replicaEndpoint != "" || replicaAccessKey != "" || replicaSecretKey != "" {
				return fmt.Errorf("--replica-endpoint, --replica-access-key, and --replica-secret-key require --replica-url")
			}

			ctx := cmd.Context()
			runner := up.ExecRunner{}
			kubeconfigPath := kubeconfig

			if kubeconfig == "" {
				if runtime.GOOS != "linux" {
					return fmt.Errorf("luncur up installs K3s and must run on linux (use --kubeconfig to target an existing cluster)")
				}
				// registries.yaml must exist before k3s first starts: the
				// install script launches the service immediately, and
				// containerd only reads mirror config at startup. Writing
				// it afterwards left a fresh install pulling
				// registry.luncur-system over https — every app image
				// failed with "no such host" until a manual restart.
				cmd.Println("==> writing registries.yaml")
				changed, err := up.WriteRegistriesYAML(up.RegistriesPath)
				if err != nil {
					return err
				}
				cmd.Println("==> ensuring K3s")
				installed, err := up.EnsureK3s(runner)
				if err != nil {
					return err
				}
				if changed && !installed {
					cmd.Println("==> restarting k3s (registry config changed)")
					if out, err := runner.Run("systemctl", "restart", "k3s"); err != nil {
						return fmt.Errorf("restart k3s: %v\n%s", err, out)
					}
				}
				kubeconfigPath = up.K3sKubeconfig
			}

			cmd.Println("==> connecting to kubernetes")
			kc, err := waitKube(ctx, kubeconfigPath)
			if err != nil {
				return err
			}

			if ip == "" {
				if ip, err = kc.NodeIP(ctx); err != nil {
					return fmt.Errorf("detect IP (use --ip): %w", err)
				}
			}
			cmd.Printf("==> external IP %s\n", ip)

			cmd.Println("==> applying system infrastructure")
			if err := build.EnsureSystem(ctx, kc, "luncur-system", "luncur-data", "luncur-registry", "registry:2"); err != nil {
				return err
			}

			email, password, fresh, err := ensureBootstrapSecret(ctx, kc)
			if err != nil {
				return err
			}

			if replicaURL != "" {
				cmd.Println("==> applying litestream replica credentials")
				secJSON, err := json.Marshal(map[string]any{
					"apiVersion": "v1", "kind": "Secret",
					"metadata": map[string]any{
						"name": up.LitestreamSecretName, "namespace": "luncur-system",
						"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
					},
					"type":       "Opaque",
					"stringData": map[string]string{"access-key": replicaAccessKey, "secret-key": replicaSecretKey},
				})
				if err != nil {
					return err
				}
				if err := kc.Apply(ctx, "luncur-system", []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
					return err
				}
			}

			cmd.Println("==> deploying luncur")
			objs, err := up.LuncurObjects(up.Params{
				Image: image, ExternalIP: ip, BuilderImage: builderImage,
				CertProvider: certProvider, ACMEEmail: acmeEmail,
				ReplicaURL: replicaURL, ReplicaEndpoint: replicaEndpoint,
			})
			if err != nil {
				return err
			}
			if err := kc.Apply(ctx, "luncur-system", objs); err != nil {
				return err
			}

			switch certProvider {
			case "traefik":
				ok, err := kc.HasGroupVersion(ctx, "helm.cattle.io/v1")
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("--cert-provider traefik requires K3s's bundled Traefik (helm.cattle.io/v1 not found) — this isn't available on a generic cluster targeted with --kubeconfig")
				}
				cmd.Println("==> configuring traefik ACME resolver")
				obj, err := up.TraefikACMEConfig(acmeEmail)
				if err != nil {
					return err
				}
				if err := kc.Apply(ctx, "kube-system", []render.Object{obj}); err != nil {
					return err
				}
			case "cert-manager":
				ok, err := kc.HasGroupVersion(ctx, "cert-manager.io/v1")
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("--cert-provider cert-manager requires cert-manager to be installed on the cluster first (cert-manager.io/v1 not found)")
				}
				cmd.Println("==> configuring cert-manager ClusterIssuer")
				obj, err := up.ClusterIssuer(acmeEmail)
				if err != nil {
					return err
				}
				if err := kc.Apply(ctx, "", []render.Object{obj}); err != nil {
					return err
				}
			}

			cmd.Println("==> waiting for rollout")
			waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if err := kc.WaitDeployment(waitCtx, "luncur-system", "luncur", 2*time.Second); err != nil {
				return fmt.Errorf("luncur deployment not ready: %w", err)
			}

			serverURL := "http://" + up.PanelHost(ip)
			cmd.Println("==> logging in")
			if err := mintToken(ctx, serverURL, email, password); err != nil {
				cmd.Printf("warning: automatic login failed (%v)\nrun: luncur login %s\n", err, serverURL)
			}

			cmd.Printf("\nluncur is up: %s\n", serverURL)
			if fresh {
				cmd.Printf("\nadmin login (shown once — store it now):\n  email:    %s\n  password: %s\n", email, password)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ip, "ip", "", "public IP (default: detect from the node)")
	cmd.Flags().StringVar(&image, "image", defaultImage(), "luncur server image")
	cmd.Flags().StringVar(&builderImage, "builder-image", build.DefaultBuilderImage, "builder image")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "target an existing cluster (skips K3s install)")
	cmd.Flags().StringVar(&certProvider, "cert-provider", "builtin", "TLS cert provider: builtin, traefik, or cert-manager")
	cmd.Flags().StringVar(&acmeEmail, "acme-email", "", "email for Let's Encrypt account registration")
	cmd.Flags().StringVar(&replicaURL, "replica-url", "", "continuously replicate the control-plane DB with a Litestream sidecar to this destination (e.g. s3://bucket/luncur)")
	cmd.Flags().StringVar(&replicaEndpoint, "replica-endpoint", "", "S3 endpoint for --replica-url (non-AWS providers)")
	cmd.Flags().StringVar(&replicaAccessKey, "replica-access-key", "", "S3 access key for --replica-url")
	cmd.Flags().StringVar(&replicaSecretKey, "replica-secret-key", "", "S3 secret key for --replica-url")
	return cmd
}

// waitKube retries kube.New — right after a K3s install the apiserver may
// still be coming up.
func waitKube(ctx context.Context, kubeconfig string) (*kube.Client, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		kc, err := kube.New(kubeconfig)
		if err == nil {
			if _, ipErr := kc.NodeIP(ctx); ipErr == nil {
				return kc, nil
			} else {
				err = ipErr
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("kubernetes not reachable: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ensureBootstrapSecret returns the admin credentials, creating them (and
// the Secret) on first run. fresh reports whether they were just minted.
func ensureBootstrapSecret(ctx context.Context, kc *kube.Client) (email, password string, fresh bool, err error) {
	data, err := kc.GetSecretData(ctx, "luncur-system", up.BootstrapSecretName)
	if err != nil {
		return "", "", false, err
	}
	if v, ok := data["admin"]; ok {
		e, p, found := strings.Cut(string(v), ":")
		if !found {
			return "", "", false, fmt.Errorf("bootstrap secret is malformed")
		}
		return e, p, false, nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", "", false, err
	}
	email, password = "admin@luncur.local", hex.EncodeToString(raw)
	secJSON, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": up.BootstrapSecretName, "namespace": "luncur-system",
			"labels": map[string]string{"app.kubernetes.io/managed-by": "luncur"},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"admin": email + ":" + password},
	})
	if err != nil {
		return "", "", false, err
	}
	if err := kc.Apply(ctx, "luncur-system", []render.Object{{Kind: "Secret", JSON: secJSON}}); err != nil {
		return "", "", false, err
	}
	return email, password, true, nil
}

// mintToken logs in (retrying while ingress propagates) and saves the CLI config.
func mintToken(ctx context.Context, serverURL, email, password string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		tok, err := client.New(serverURL, "").Login(email, password)
		if err == nil {
			return saveConfig(Config{Server: serverURL, Token: tok})
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
