package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// ejectCmd is `app eject`: a one-way detach that stops luncur from managing
// the named app. It rides the `app` command tree (see app.go) rather than
// being a top-level command.
func ejectCmd() *cobra.Command {
	var project string
	var yes bool
	cmd := &cobra.Command{
		Use:   "eject <name>",
		Short: "Detach an app from luncur's management (one-way)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(), "this is one-way; luncur will stop managing %s. type the app name to confirm: ", name)
				var confirm string
				if _, err := fmt.Fscanln(cmd.InOrStdin(), &confirm); err != nil {
					return fmt.Errorf("read confirmation: %w", err)
				}
				if confirm != name {
					return fmt.Errorf("confirmation %q does not match app name %q; aborted", confirm, name)
				}
			}

			c, err := apiClient()
			if err != nil {
				return err
			}
			yamlText, savedTo, err := c.EjectApp(project, name)
			if err != nil {
				return err
			}
			cmd.Print(yamlText)
			cmd.Printf("saved to: %s\n", savedTo)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return cmd
}

// adoptCmd is `app adopt`: reverses eject — luncur reclaims management of
// the app and re-applies its rendered state (winning any manual drift).
func adoptCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "adopt <name>",
		Short: "Re-adopt an ejected app under luncur's management",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			warning, err := c.AdoptApp(project, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("adopted %s — luncur manages it again\n", args[0])
			if warning != "" {
				cmd.Printf("warning: %s\n", warning)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
