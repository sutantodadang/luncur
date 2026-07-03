package cli

import "github.com/spf13/cobra"

// logsCmd prints or follows logs. With --deploy it reads that deployment's
// build log (add -f to follow a build in progress); without it, it streams
// the app's runtime pod logs.
func logsCmd() *cobra.Command {
	var project string
	var deploy int64
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Print build or runtime logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			switch {
			case deploy != 0 && follow:
				return c.FollowDeployLogs(project, args[0], deploy, cmd.OutOrStdout())
			case deploy != 0:
				b, err := c.DeployLogs(project, args[0], deploy)
				if err != nil {
					return err
				}
				cmd.Print(string(b))
				return nil
			default:
				return c.RuntimeLogs(project, args[0], follow, cmd.OutOrStdout())
			}
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().Int64Var(&deploy, "deploy", 0, "deployment id (build log; omit for runtime logs)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream live")
	return cmd
}
