package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// psCmd lists an app's live pods with per-pod status and usage.
func psCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "ps <app>",
		Short: "Show an app's running pods",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			pods, err := c.AppPods(project, args[0])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tREADY\tRESTARTS\tCPU\tMEMORY\tNODE\tSTARTED")
			for _, p := range pods {
				status := p.Phase
				if p.Reason != "" {
					status += " (" + p.Reason + ")"
				}
				ready := "no"
				if p.Ready {
					ready = "yes"
				}
				cpu, mem := "-", "-"
				if p.MetricsOK {
					cpu = fmt.Sprintf("%dm", p.CPUMilli)
					mem = fmt.Sprintf("%dMi", p.MemoryMiB)
				}
				started := "-"
				if p.StartedAt != "" {
					if t, err := time.Parse(time.RFC3339, p.StartedAt); err == nil {
						started = humanAge(t)
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
					p.Name, status, ready, p.Restarts, cpu, mem, p.Node, started)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

// humanAge formats the time since t as the largest single unit ("2d", "5h",
// "12m", "30s") for the ps table's STARTED column.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}
