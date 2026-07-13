// Package cli implements the luncur command tree: the server (`serve`),
// the installer (`up`), and every client verb, all in one binary.
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
	root.AddCommand(redeployCmd())
	root.AddCommand(scaleCmd())
	root.AddCommand(autoscaleCmd())
	root.AddCommand(healthCmd())
	root.AddCommand(webhookCmd())
	root.AddCommand(destroyCmd())
	root.AddCommand(envCmd())
	root.AddCommand(editCmd())
	root.AddCommand(initCmd())
	root.AddCommand(logsCmd())
	root.AddCommand(psCmd())
	root.AddCommand(metricsCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(upCmd())
	root.AddCommand(downCmd())
	root.AddCommand(updateCmd())
	root.AddCommand(sshKeyCmd())
	root.AddCommand(pushHookCmd())
	root.AddCommand(domainCmd())
	root.AddCommand(volumeCmd())
	root.AddCommand(configCmd())
	root.AddCommand(rollbackCmd())
	root.AddCommand(tokenCmd())
	root.AddCommand(inviteCmd())
	root.AddCommand(addonCmd())
	root.AddCommand(backupCmd())
	root.AddCommand(restoreCmd())
	root.AddCommand(registryCmd())
	root.AddCommand(auditCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(joinCmd())
	root.AddCommand(nodeCmd())
	root.AddCommand(gpuCmd())
	root.AddCommand(runCmd())
	root.AddCommand(sweepCmd())
	root.AddCommand(pipelineCmd())
	root.AddCommand(argoCmd())
	root.AddCommand(accountCmd())
	root.AddCommand(forwardCmd())
	return root
}

// Execute runs the CLI. It is the only symbol main needs.
func Execute() error {
	return newRoot().Execute()
}
