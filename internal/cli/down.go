package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/up"
)

// downSystemNamespace, downSystemApp, and downDataPVC name the objects
// up.go's LuncurObjects (internal/up/manifests.go) and build.EnsureSystem
// (via internal/cli/up.go's call to build.EnsureSystem(ctx, kc,
// "luncur-system", "luncur-data", ...)) actually create. down must reverse
// exactly those, not a hypothetical systemd/host-dir install.
const (
	downSystemNamespace = "luncur-system" // up.go: build.EnsureSystem's systemNS, up.LuncurObjects' namespace
	downSystemApp       = "luncur"        // up.LuncurObjects: Deployment/Service/ServiceAccount/Ingress name
	downSSHService      = "luncur-ssh"    // up.LuncurObjects: NodePort Service for git-ssh
	downClusterRole     = "luncur"        // up.LuncurObjects: ClusterRole name (cluster-scoped)
	downClusterBinding  = "luncur-admin"  // up.LuncurObjects: ClusterRoleBinding name (cluster-scoped)
	downDataPVC         = "luncur-data"   // up.go/serve.go: build.EnsureSystem's dataPVC
	downDBPathInPod     = "/var/lib/luncur/luncur.db" // up/manifests.go: LuncurObjects' --db arg

	// downManagedByLabel matches kube.Client.EnsureNamespace's stamped
	// label, applied both to luncur-system (up.go) and to every project
	// namespace (server/*.go: EnsureNamespace(ctx, p.Namespace)). Listing by
	// this label is how down finds every namespace to tear down without
	// needing offline access to the DB — which, unlike restore's bare-host
	// dataDir, lives inside a PersistentVolumeClaim in the cluster, not on
	// this host's filesystem.
	downManagedByLabel = "app.kubernetes.io/managed-by=luncur"

	// downK3sUninstallScript is written by the official K3s install script
	// (curl -sfL https://get.k3s.io | ...), the same one up.go's
	// up.EnsureK3s runs. Not part of this repo; a standard K3s convention.
	downK3sUninstallScript = "/usr/local/bin/k3s-uninstall.sh"
)

// downStep is one unit of teardown work: a human-readable description
// (printed as-is by --dry-run) and the action to take when actually
// executing. buildDownPlan is a pure function — constructing a []downStep
// does no I/O; only calling a step's Run does.
type downStep struct {
	Desc string
	Run  func() error
}

// downOpts carries every resolved flag/path buildDownPlan needs. KubeClient
// and Runner are nil when only planning (--dry-run, tests) — any step that
// dereferences them will error (or, if truly nil, panic), which is exactly
// why RunE never calls a step's Run in --dry-run mode.
type downOpts struct {
	All      bool
	NoBackup bool

	Kubeconfig     string
	RegistriesPath string
	K3sUninstall   string
	BackupPath     string

	KubeClient *kube.Client
	Runner     up.Runner
}

// backupPath returns the deterministic ~/luncur-final-backup-<unix-ts>.db
// path for a given home dir and clock, so buildDownPlan's output (via
// downOpts.BackupPath) stays a pure function of its inputs.
func backupPath(homeDir string, now time.Time) string {
	return filepath.Join(homeDir, fmt.Sprintf("luncur-final-backup-%d.db", now.Unix()))
}

// buildDownPlan is the pure planner: given fully-resolved options, it
// returns the ordered steps `luncur down` will take. No I/O happens here —
// only inside a step's Run, and only for steps RunE actually invokes.
func buildDownPlan(opts downOpts) []downStep {
	var plan []downStep

	if !opts.NoBackup {
		plan = append(plan, downStep{
			Desc: fmt.Sprintf("backup SQLite DB to %s", opts.BackupPath),
			Run: func() error {
				if opts.KubeClient == nil {
					return fmt.Errorf("kubernetes client unavailable")
				}
				ctx := context.Background()
				pods, err := opts.KubeClient.AppPods(ctx, downSystemNamespace, downSystemApp)
				if err != nil {
					return err
				}
				if len(pods) == 0 {
					return fmt.Errorf("no %s pod found in %s", downSystemApp, downSystemNamespace)
				}
				f, err := os.Create(opts.BackupPath)
				if err != nil {
					return err
				}
				defer f.Close()
				return opts.KubeClient.ExecPod(ctx, downSystemNamespace, pods[0], downSystemApp,
					[]string{"cat", downDBPathInPod}, nil, f, io.Discard)
			},
		})
	}

	plan = append(plan, downStep{
		Desc: fmt.Sprintf("stop luncur (delete Deployment/Services/Ingress/ServiceAccount and RBAC in %s)", downSystemNamespace),
		Run: func() error {
			if opts.KubeClient == nil {
				return fmt.Errorf("kubernetes client unavailable")
			}
			ctx := context.Background()
			targets := []struct{ kind, ns, name string }{
				{"Deployment", downSystemNamespace, downSystemApp},
				{"Service", downSystemNamespace, downSystemApp},
				{"Service", downSystemNamespace, downSSHService},
				{"Ingress", downSystemNamespace, downSystemApp},
				{"ServiceAccount", downSystemNamespace, downSystemApp},
				{"ClusterRoleBinding", "", downClusterBinding},
				{"ClusterRole", "", downClusterRole},
			}
			var errs []error
			for _, t := range targets {
				if err := opts.KubeClient.DeleteObject(ctx, t.ns, t.kind, t.name); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	})

	plan = append(plan, downStep{
		Desc: "delete luncur-managed namespaces (luncur-system + every project namespace)",
		Run: func() error {
			if opts.KubeClient == nil {
				return fmt.Errorf("kubernetes client unavailable")
			}
			ctx := context.Background()
			names, err := opts.KubeClient.ListNamespacesByLabel(ctx, downManagedByLabel)
			if err != nil {
				return err
			}
			var errs []error
			for _, ns := range names {
				if err := opts.KubeClient.DeleteNamespace(ctx, ns); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	})

	plan = append(plan, downStep{
		Desc: fmt.Sprintf("remove luncur data volume (PersistentVolumeClaim %s in %s)", downDataPVC, downSystemNamespace),
		Run: func() error {
			if opts.KubeClient == nil {
				return fmt.Errorf("kubernetes client unavailable")
			}
			return opts.KubeClient.DeletePVC(context.Background(), downSystemNamespace, downDataPVC)
		},
	})

	plan = append(plan, downStep{
		Desc: fmt.Sprintf("remove registries config written by `luncur up` (%s)", opts.RegistriesPath),
		Run: func() error {
			if err := os.Remove(opts.RegistriesPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		},
	})

	if opts.All {
		plan = append(plan, downStep{
			Desc: fmt.Sprintf("uninstall K3s (%s)", opts.K3sUninstall),
			Run: func() error {
				if _, err := os.Stat(opts.K3sUninstall); err != nil {
					if os.IsNotExist(err) {
						fmt.Printf("%s not found; K3s appears already uninstalled\n", opts.K3sUninstall)
						return nil
					}
					return err
				}
				if opts.Runner == nil {
					return fmt.Errorf("runner unavailable")
				}
				out, err := opts.Runner.Run("sh", "-c", opts.K3sUninstall)
				if err != nil {
					return fmt.Errorf("k3s-uninstall: %v\n%s", err, out)
				}
				return nil
			},
		})
	}

	return plan
}

// downConfirmationSummary is what a human must read (and type "luncur"
// past) before a non---dry-run, non---yes teardown proceeds.
func downConfirmationSummary(opts downOpts) string {
	if opts.All {
		return fmt.Sprintf(
			"this will permanently remove: luncur apps, namespaces, data dir (DB backed up to %s); "+
				"and --all: K3s AND ALL CLUSTER DATA", opts.BackupPath)
	}
	return fmt.Sprintf(
		"this will permanently remove: luncur apps, namespaces, data dir (DB backed up to %s); K3s stays",
		opts.BackupPath)
}

func downCmd() *cobra.Command {
	var all, dryRun, yes, noBackup bool
	var kubeconfig string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Uninstall luncur from this machine (add --all to also remove K3s)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				home = "."
			}
			opts := downOpts{
				All:            all,
				NoBackup:       noBackup,
				Kubeconfig:     kubeconfig,
				RegistriesPath: up.RegistriesPath,
				K3sUninstall:   downK3sUninstallScript,
				BackupPath:     backupPath(home, time.Now()),
			}

			if dryRun {
				plan := buildDownPlan(opts)
				cmd.Println("dry run — no changes will be made:")
				for i, s := range plan {
					cmd.Printf("%d. %s\n", i+1, s.Desc)
				}
				return nil
			}

			// Host-side steps (registries.yaml, k3s-uninstall.sh) are
			// Linux-only, same guard as `luncur up`.
			if runtime.GOOS != "linux" {
				return fmt.Errorf("luncur down manages a host K3s install and must run on linux")
			}

			if !yes {
				cmd.Println(downConfirmationSummary(opts))
				cmd.Print("type \"luncur\" to confirm: ")
				var confirm string
				if _, err := fmt.Fscanln(cmd.InOrStdin(), &confirm); err != nil {
					return fmt.Errorf("read confirmation: %w", err)
				}
				if confirm != "luncur" {
					return fmt.Errorf("confirmation %q does not match; aborted", confirm)
				}
			}

			kubeconfigPath := kubeconfig
			if kubeconfigPath == "" {
				kubeconfigPath = up.K3sKubeconfig
			}
			kc, err := kube.New(kubeconfigPath)
			if err != nil {
				cmd.Printf("warning: kubernetes unavailable (%v); cluster-side steps will fail\n", err)
			} else {
				opts.KubeClient = kc
			}
			opts.Runner = up.ExecRunner{}

			plan := buildDownPlan(opts)
			failed := false
			for _, s := range plan {
				cmd.Printf("==> %s\n", s.Desc)
				if err := s.Run(); err != nil {
					cmd.Printf("step failed: %s: %v\n", s.Desc, err)
					failed = true
				}
			}
			if failed {
				return fmt.Errorf("one or more teardown steps failed; see above")
			}
			cmd.Println("luncur down: complete")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also uninstall K3s (and all cluster data)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the teardown plan and exit; execute nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip the final DB backup")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "target an existing cluster (default: the K3s install luncur up created)")
	return cmd
}
