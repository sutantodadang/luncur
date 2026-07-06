package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// accountCmd groups self-service account actions — change your own
// password or login email, both requiring the current password.
func accountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Manage your account",
	}

	passwd := &cobra.Command{
		Use:   "passwd",
		Short: "Change your password",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			oldPW, err := promptPassword(cmd, "current password: ")
			if err != nil {
				return err
			}
			newPW, err := promptPassword(cmd, "new password: ")
			if err != nil {
				return err
			}
			if err := c.ChangePassword(oldPW, newPW); err != nil {
				return err
			}
			cmd.Println("password changed.")
			return nil
		},
	}

	email := &cobra.Command{
		Use:   "email <new-email>",
		Short: "Change your login email",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			newEmail := strings.TrimSpace(args[0])
			pw, err := promptPassword(cmd, "current password: ")
			if err != nil {
				return err
			}
			if err := c.ChangeEmail(pw, newEmail); err != nil {
				return err
			}
			cmd.Printf("email changed to %s. use it next login.\n", newEmail)
			return nil
		},
	}

	cmd.AddCommand(passwd, email)
	return cmd
}
