package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// logsCmd prints or follows logs. With --deploy it reads that deployment's
// build log (add -f to follow a build in progress); without it, it streams
// the app's runtime pod logs.
func logsCmd() *cobra.Command {
	var project, env string
	var deploy string
	var follow bool
	var tail int64
	var since string
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Print build or runtime logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			if deploy != "" && (tail > 0 || since != "") {
				return fmt.Errorf("--tail/--since apply to runtime logs only, not --deploy build logs")
			}
			switch {
			case deploy != "" && follow:
				return c.FollowDeployLogs(project, args[0], deploy, cmd.OutOrStdout())
			case deploy != "":
				b, err := c.DeployLogs(project, args[0], deploy)
				if err != nil {
					return err
				}
				cmd.Print(string(b))
				return nil
			default:
				return c.RuntimeLogs(project, args[0], follow, tail, since, cmd.OutOrStdout())
			}
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
	cmd.Flags().StringVar(&deploy, "deploy", "", "deployment id (build log; omit for runtime logs)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream live")
	cmd.Flags().Int64Var(&tail, "tail", 0, "only the last N log lines (0 = all)")
	cmd.Flags().StringVar(&since, "since", "", "only logs newer than this duration, e.g. 15m, 2h")
	return cmd
}
