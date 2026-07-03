package cli

import "github.com/spf13/cobra"

// logsCmd prints the build log for a specific deployment. Live follow is
// deferred to Plan D.
func logsCmd() *cobra.Command {
	var project string
	var deploy int64
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Print the build log for a deployment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			b, err := c.DeployLogs(project, args[0], deploy)
			if err != nil {
				return err
			}
			cmd.Print(string(b))
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().Int64Var(&deploy, "deploy", 0, "deployment id")
	cmd.MarkFlagRequired("deploy")
	return cmd
}
