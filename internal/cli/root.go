package cli

import "github.com/spf13/cobra"

// version is overridden at release time via
// -ldflags "-X github.com/sutantodadang/luncur/internal/cli.version=v0.x.y".
var version = "dev"

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "luncur",
		Short:         "luncur — tiny self-hosted PaaS on K3s",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the luncur version",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Println(version)
		},
	})
	root.AddCommand(loginCmd())
	root.AddCommand(whoamiCmd())
	root.AddCommand(userCmd())
	root.AddCommand(serveCmd())
	root.AddCommand(projectCmd())
	root.AddCommand(appCmd())
	root.AddCommand(deployCmd())
	root.AddCommand(scaleCmd())
	root.AddCommand(destroyCmd())
	root.AddCommand(envCmd())
	root.AddCommand(editCmd())
	root.AddCommand(initCmd())
	root.AddCommand(logsCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(upCmd())
	return root
}

// Execute runs the CLI. It is the only symbol main needs.
func Execute() error {
	return newRoot().Execute()
}
