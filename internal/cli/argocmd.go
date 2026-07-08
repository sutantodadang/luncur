package cli

import "github.com/spf13/cobra"

// argoCmd groups the argo-engine operator commands (currently just
// install; C4's UI covers day-to-day pipeline monitoring).
func argoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "argo",
		Short: "Manage the Argo Workflows pipeline engine",
	}
	cmd.AddCommand(argoInstallCmd())
	return cmd
}

// argoInstallCmd installs the pinned Argo Workflows controller (no
// argo-server UI — spec scope is the workflow controller only) and reminds
// the operator how to switch pipelines onto it.
func argoInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the pinned Argo Workflows controller",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			version, err := c.ArgoInstall()
			if err != nil {
				return err
			}
			cmd.Printf("argo workflows %s installed\n", version)
			cmd.Println("argo engine: set pipeline_engine=argo or per-pipeline --engine argo")
			return nil
		},
	}
}
