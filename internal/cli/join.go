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
	cmd := &cobra.Command{
		Use:   "join <server-url>",
		Short: "Join this machine to an existing luncur cluster as a K3s agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("luncur join installs a K3s agent and must run on linux")
			}
			runner := up.ExecRunner{}

			cmd.Println("==> writing registries.yaml")
			changed, err := up.WriteRegistriesYAML(up.RegistriesPath)
			if err != nil {
				return err
			}

			cmd.Println("==> ensuring K3s agent")
			installed, err := up.EnsureK3sAgent(runner, args[0], token)
			if err != nil {
				return err
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
	return cmd
}
