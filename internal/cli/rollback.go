package cli

import (
	"fmt"

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
			// --deploy is the per-app deploy number shown by `luncur status`
			// and the web UI (#1, #2, ...) — resolve it to the opaque
			// internal id the rollback API expects. Unset (0) means
			// "previous live", which the API resolves itself from "".
			targetID := ""
			if deploy != 0 {
				deploys, err := c.ListDeploys(project, args[0])
				if err != nil {
					return err
				}
				found := false
				for _, d := range deploys {
					if d.Seq == deploy {
						targetID = d.ID
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("no deploy #%d found for %s", deploy, args[0])
				}
			}
			newSeq, err := c.Rollback(project, args[0], targetID)
			if err != nil {
				return err
			}
			cmd.Printf("rolled back (new deploy #%d)\n", newSeq)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().Int64Var(&deploy, "deploy", 0, "deploy number to roll back to, as shown by `luncur status` (default: previous live)")
	return cmd
}
