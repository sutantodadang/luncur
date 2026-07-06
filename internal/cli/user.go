package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

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

	passwd := &cobra.Command{
		Use:   "passwd <email>",
		Short: "Reset a user's password",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			email := strings.ToLower(strings.TrimSpace(args[0]))
			users, err := c.ListUsers()
			if err != nil {
				return err
			}
			var id int64
			found := false
			for _, u := range users {
				if strings.ToLower(u.Email) == email {
					id, found = u.ID, true
					break
				}
			}
			if !found {
				return fmt.Errorf("no such user")
			}
			pw, err := promptPassword(cmd, "new password: ")
			if err != nil {
				return err
			}
			if err := c.SetUserPassword(id, pw); err != nil {
				return err
			}
			cmd.Printf("password updated for %s.\n", email)
			return nil
		},
	}

	cmd.AddCommand(add, passwd)
	return cmd
}
