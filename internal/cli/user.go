package cli

import "github.com/spf13/cobra"

func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users (admin only)",
	}

	var role, password string
	add := &cobra.Command{
		Use:   "add <email>",
		Short: "Create a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			u, err := c.CreateUser(args[0], password, role)
			if err != nil {
				return err
			}
			cmd.Printf("created %s (%s)\n", u.Email, u.Role)
			return nil
		},
	}
	add.Flags().StringVar(&role, "role", "member", "role: admin or member")
	add.Flags().StringVar(&password, "password", "", "initial password")
	add.MarkFlagRequired("password")

	cmd.AddCommand(add)
	return cmd
}
