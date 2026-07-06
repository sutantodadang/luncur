package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage projects",
	}

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			p, err := c.CreateProject(args[0])
			if err != nil {
				return err
			}
			cmd.Printf("created %s (namespace %s)\n", p.Name, p.Namespace)
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			projects, err := c.ListProjects()
			if err != nil {
				return err
			}
			for _, p := range projects {
				cmd.Printf("%s\t%s\n", p.Name, p.Namespace)
			}
			return nil
		},
	}

	addMember := &cobra.Command{
		Use:   "add-member <project> <email>",
		Short: "Add a member to a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.AddMember(args[0], args[1]); err != nil {
				return err
			}
			cmd.Printf("added %s to %s\n", args[1], args[0])
			return nil
		},
	}

	rename := &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.RenameProject(args[0], args[1]); err != nil {
				return err
			}
			cmd.Println("renamed. namespace unchanged.")
			return nil
		},
	}

	var rmYes bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a project and everything in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !rmYes {
				cmd.Printf("this will permanently destroy project %q: all its apps, addons, domains and volumes\n", name)
				cmd.Printf("type %q to confirm: ", name)
				var confirm string
				if _, err := fmt.Fscanln(cmd.InOrStdin(), &confirm); err != nil {
					return fmt.Errorf("read confirmation: %w", err)
				}
				if confirm != name {
					return fmt.Errorf("confirmation %q does not match; aborted", confirm)
				}
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeleteProject(name); err != nil {
				return err
			}
			cmd.Printf("deleted %s\n", name)
			return nil
		},
	}
	rm.Flags().BoolVar(&rmYes, "yes", false, "skip the interactive confirmation")

	removeMember := &cobra.Command{
		Use:   "remove-member <project> <email>",
		Short: "Remove a member from a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.RemoveMember(args[0], args[1]); err != nil {
				return err
			}
			cmd.Printf("removed %s from %s\n", args[1], args[0])
			return nil
		},
	}

	cmd.AddCommand(create, list, addMember, rename, rm, removeMember)
	return cmd
}
