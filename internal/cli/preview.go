package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// previewCmd is the `luncur preview` group: list, manually create, and
// remove a project's preview environments — the CLI twin of the previews
// REST API (Task 16). Distinct from `luncur envs`, which manages a
// project's standing environments; a preview environment is ephemeral and
// branch-scoped, normally created/torn down by the project webhook
// (routeBranch, preview.go) rather than by hand.
func previewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Manage a project's preview environments",
	}

	var lsProject string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List a project's preview environments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			previews, err := c.ListPreviews(lsProject)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tBRANCH\tLAST-ACTIVE\tAPPS")
			for _, pv := range previews {
				apps := make([]string, 0, len(pv.Apps))
				for _, a := range pv.Apps {
					apps = append(apps, a.Name+"="+a.URL)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", pv.Name, pv.SourceBranch, pv.LastActiveAt, strings.Join(apps, ","))
			}
			return tw.Flush()
		},
	}
	ls.Flags().StringVar(&lsProject, "project", "", "project name")
	ls.MarkFlagRequired("project")

	var createProject, createFrom string
	create := &cobra.Command{
		Use:   "create <branch>",
		Short: "Manually create (or re-resolve) a preview environment for a branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			pv, err := c.CreatePreview(createProject, args[0], createFrom)
			if err != nil {
				return err
			}
			cmd.Printf("created preview %s (branch %s)\n", pv.Name, pv.SourceBranch)
			return nil
		},
	}
	create.Flags().StringVar(&createProject, "project", "", "project name")
	create.MarkFlagRequired("project")
	create.Flags().StringVar(&createFrom, "from", "", "standing environment to clone from (default: the project's preview base env)")

	var rmProject string
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a preview environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeletePreview(rmProject, args[0]); err != nil {
				return err
			}
			cmd.Printf("removed %s\n", args[0])
			return nil
		},
	}
	rm.Flags().StringVar(&rmProject, "project", "", "project name")
	rm.MarkFlagRequired("project")

	cmd.AddCommand(ls, create, rm)
	return cmd
}
