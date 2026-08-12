package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// envsCmd is the `luncur envs` group: CRUD for a project's deployment
// environments. It is distinct from `luncur env`, which manages an app's
// environment variables. The shared `--env` flag on app/addon/domain/scale/
// logs commands selects which of these environments a call targets.
func envsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "envs",
		Short: "Manage a project's deployment environments",
	}

	var listProject string
	list := &cobra.Command{
		Use:   "list",
		Short: "List a project's environments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			envs, err := c.ListEnvs(listProject)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tDEFAULT\tBASE-BRANCH\tNAMESPACE")
			for _, e := range envs {
				fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\n", e.Name, e.Kind, e.IsDefault, e.BaseBranch, e.Namespace)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&listProject, "project", "", "project name")
	list.MarkFlagRequired("project")

	var createProject, createBaseBranch string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new standing environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			e, err := c.CreateEnv(createProject, args[0], createBaseBranch)
			if err != nil {
				return err
			}
			cmd.Printf("created %s (namespace %s)\n", e.Name, e.Namespace)
			return nil
		},
	}
	create.Flags().StringVar(&createProject, "project", "", "project name")
	create.MarkFlagRequired("project")
	create.Flags().StringVar(&createBaseBranch, "base-branch", "", "git branch that drives webhook deploys for this env")

	var rmProject string
	var rmForce bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeleteEnv(rmProject, args[0], rmForce); err != nil {
				return err
			}
			cmd.Printf("removed %s\n", args[0])
			return nil
		},
	}
	rm.Flags().StringVar(&rmProject, "project", "", "project name")
	rm.MarkFlagRequired("project")
	rm.Flags().BoolVar(&rmForce, "force", false, "remove even if the environment has live apps")

	var setDefaultProject string
	setDefault := &cobra.Command{
		Use:   "set-default <name>",
		Short: "Make an environment the project's default (env-less calls resolve here)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetDefaultEnv(setDefaultProject, args[0]); err != nil {
				return err
			}
			cmd.Printf("default environment is now %s\n", args[0])
			return nil
		},
	}
	setDefault.Flags().StringVar(&setDefaultProject, "project", "", "project name")
	setDefault.MarkFlagRequired("project")

	var setBaseProject string
	setBase := &cobra.Command{
		Use:   "set-base <name>",
		Short: "Set the environment new preview environments clone from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetPreviewBase(setBaseProject, args[0]); err != nil {
				return err
			}
			cmd.Printf("preview base environment is now %s\n", args[0])
			return nil
		},
	}
	setBase.Flags().StringVar(&setBaseProject, "project", "", "project name")
	setBase.MarkFlagRequired("project")

	var copyProject, copyFrom, copyTo string
	var copyYes bool
	copyC := &cobra.Command{
		Use:   "copy",
		Short: "Copy one environment's setup (app config, env vars, missing addons) into another",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			// Overwriting the default (production) environment's setup is
			// the risky direction — require an explicit --yes for it.
			if !copyYes {
				envs, err := c.ListEnvs(copyProject)
				if err != nil {
					return err
				}
				for _, e := range envs {
					if e.Name == copyTo && e.IsDefault {
						return fmt.Errorf("%q is the project's default environment; re-run with --yes to overwrite its setup", copyTo)
					}
				}
			}
			sum, err := c.CopyEnvSetup(copyProject, copyFrom, copyTo)
			if err != nil {
				return err
			}
			cmd.Printf("copied %s -> %s: %d apps created, %d updated, %d addons cloned\n",
				copyFrom, copyTo, sum.AppsCreated, sum.AppsUpdated, sum.AddonsCloned)
			for _, warn := range sum.Warnings {
				cmd.Printf("warning: %s\n", warn)
			}
			return nil
		},
	}
	copyC.Flags().StringVar(&copyProject, "project", "", "project name")
	copyC.MarkFlagRequired("project")
	copyC.Flags().StringVar(&copyFrom, "from", "", "source environment")
	copyC.MarkFlagRequired("from")
	copyC.Flags().StringVar(&copyTo, "to", "", "target environment")
	copyC.MarkFlagRequired("to")
	copyC.Flags().BoolVar(&copyYes, "yes", false, "allow overwriting the default environment's setup")

	cmd.AddCommand(list, create, rm, setDefault, setBase, copyC)
	return cmd
}
