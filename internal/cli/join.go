package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/up"
)

// joinCmd installs a K3s agent on this machine and joins it to an existing
// luncur cluster's server. Run luncur node join-command on the server to get
// the exact invocation (server URL + node token).
func joinCmd() *cobra.Command {
	var token string
	var gpu bool
	cmd := &cobra.Command{
		Use:   "join <server-url>",
		Short: "Join this machine to an existing luncur cluster as a K3s agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("luncur join installs a K3s agent and must run on linux")
			}
			runner := up.ExecRunner{}

			if gpu {
				cmd.Println("==> checking NVIDIA driver")
				if err := up.CheckNVIDIADriver(runner); err != nil {
					return err
				}
				cmd.Println("==> ensuring nvidia-container-toolkit")
				tkInstalled, err := up.EnsureNVIDIAToolkit(runner)
				if err != nil {
					return err
				}
				if tkInstalled {
					cmd.Println("    installed nvidia-container-toolkit")
				}
			}

			cmd.Println("==> writing registries.yaml")
			changed, err := up.WriteRegistriesYAML(up.RegistriesPath)
			if err != nil {
				return err
			}

			cmd.Println("==> ensuring K3s agent")
			installed, err := up.EnsureK3sAgent(runner, args[0], token, gpu)
			if err != nil {
				return err
			}
			if gpu && !installed {
				cmd.Println("note: k3s agent was already installed; the GPU label is only set at install time.")
				cmd.Println("      label the node from the server: kubectl label node <name> " + up.GPUNodeLabel)
			}

			if changed && !installed {
				cmd.Println("==> restarting k3s-agent (registry config changed)")
				if out, err := runner.Run("systemctl", "restart", "k3s-agent"); err != nil {
					return fmt.Errorf("restart k3s-agent: %v\n%s", err, out)
				}
			}

			cmd.Println("\njoined")
			cmd.Println("verify from your workstation: luncur node ls")
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "node token from the server (see: luncur node join-command)")
	cmd.MarkFlagRequired("token")
	cmd.Flags().BoolVar(&gpu, "gpu", false, "GPU node: verify the NVIDIA driver, install nvidia-container-toolkit if missing, and label the node luncur.dev/gpu=true")
	return cmd
}
