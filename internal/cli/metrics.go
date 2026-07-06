package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// metricsCmd shows an app's sampled CPU/memory history (the server keeps a
// ~30 minute in-memory window, one sample every 15s).
func metricsCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "metrics <app>",
		Short: "Show an app's sampled CPU/memory history (last ~30m)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			samples, err := c.MetricsHistory(project, args[0])
			if err != nil {
				return err
			}
			if len(samples) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no samples yet — the server collects one every 15s.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tCPU\tMEMORY")
			for _, s := range samples {
				at := s.At
				if t, err := time.Parse(time.RFC3339, s.At); err == nil {
					at = t.Local().Format("15:04:05")
				}
				fmt.Fprintf(tw, "%s\t%dm\t%dMi\n", at, s.CPUMillicores, s.MemoryMiB)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
