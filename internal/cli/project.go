package cli

import "github.com/spf13/cobra"

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

	cmd.AddCommand(create, list, addMember)
	return cmd
}
