package cli

import (
	"github.com/spf13/cobra"
)

func rollbackCmd() *cobra.Command {
	var project string
	var deploy int64
	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Redeploy a previous deployment's image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			newID, err := c.Rollback(project, args[0], deploy)
			if err != nil {
				return err
			}
			cmd.Printf("rolled back (new deploy %d)\n", newID)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().Int64Var(&deploy, "deploy", 0, "deployment id to roll back to (default: previous live)")
	return cmd
}
