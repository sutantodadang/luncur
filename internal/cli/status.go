package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// statusCmd shows app status. With no app argument it lists the project's
// apps (name + URL); with one it shows the latest deployment detail.
func statusCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "status [app]",
		Short: "Show app / deployment status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				a, err := c.GetApp(project, args[0])
				if err != nil {
					return err
				}
				m, err := c.AppMetrics(project, args[0])
				if err != nil {
					return err
				}
				cmd.Printf("app:      %s\nstatus:   %s\nreplicas: %d\nimage:    %s\nurl:      %s\n",
					a.Name, a.Status, a.Replicas, a.Image, a.URL)
				if m.Available {
					cmd.Printf("cpu:      %dm\nmemory:   %dMi\n", m.CPUMillicores, m.MemoryMiB)
				} else {
					cmd.Printf("metrics:  unavailable\n")
				}
				cmd.Printf("deploys:  %d\n", m.DeployCount)
				return nil
			}
			apps, err := c.ListApps(project)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tREPLICAS\tURL")
			for _, a := range apps {
				fmt.Fprintf(tw, "%s\t%d\t%s\n", a.Name, a.Replicas, a.URL)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
