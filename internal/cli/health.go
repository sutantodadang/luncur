package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func healthCmd() *cobra.Command {
	var project, path string
	var off bool
	cmd := &cobra.Command{
		Use:   "health <app>",
		Short: "Set or clear an app's HTTP health check path (readiness+liveness probes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pathSet := cmd.Flags().Changed("path")
			if pathSet == off {
				return fmt.Errorf("specify exactly one of --path or --off")
			}

			c, err := apiClient()
			if err != nil {
				return err
			}

			value := path
			if off {
				value = ""
			}
			if err := c.SetHealth(project, args[0], value); err != nil {
				return err
			}

			if value == "" {
				cmd.Println("health check: off")
			} else {
				cmd.Printf("health check: %s\n", value)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&path, "path", "", "HTTP path probed for readiness/liveness (e.g. /healthz)")
	cmd.Flags().BoolVar(&off, "off", false, "clear the health check path")
	return cmd
}
