package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// autoscaleCmd configures (or shows, or disables) an app's autoscaling/v2
// HorizontalPodAutoscaler.
func autoscaleCmd() *cobra.Command {
	var project string
	var min, max, cpu int
	var off bool
	cmd := &cobra.Command{
		Use:   "autoscale <app>",
		Short: "Configure HPA autoscaling for an app, or show/disable it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			minSet := cmd.Flags().Changed("min")
			maxSet := cmd.Flags().Changed("max")
			cpuSet := cmd.Flags().Changed("cpu")

			c, err := apiClient()
			if err != nil {
				return err
			}

			if off {
				if err := c.Autoscale(project, args[0], 0, 0, 0); err != nil {
					return err
				}
				cmd.Println("autoscale off")
				return nil
			}

			if !minSet && !maxSet && !cpuSet {
				a, err := c.GetApp(project, args[0])
				if err != nil {
					return err
				}
				if a.Autoscale == nil {
					cmd.Println("autoscale: off")
					return nil
				}
				cmd.Printf("autoscale: %d-%d @%d%% cpu\n", a.Autoscale.Min, a.Autoscale.Max, a.Autoscale.CPU)
				return nil
			}

			if !minSet || !maxSet || !cpuSet {
				return fmt.Errorf("--min, --max, and --cpu are required together (or use --off)")
			}
			if err := c.Autoscale(project, args[0], min, max, cpu); err != nil {
				return err
			}
			cmd.Printf("autoscale set: %d-%d @%d%% cpu\n", min, max, cpu)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().IntVar(&min, "min", 0, "minimum replicas")
	cmd.Flags().IntVar(&max, "max", 0, "maximum replicas")
	cmd.Flags().IntVar(&cpu, "cpu", 0, "target CPU utilization percent")
	cmd.Flags().BoolVar(&off, "off", false, "disable autoscale")
	return cmd
}
