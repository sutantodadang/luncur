package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func inviteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite",
		Short: "Manage registration invites (admin only)",
	}

	var role, email string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a single-use invite link",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			inv, err := c.CreateInvite(role, email)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cmd.Printf("invite created (role %s, expires %s):\n%s%s\n",
				inv.Role, inv.ExpiresAt, cfg.Server, inv.Path)
			if email != "" {
				if inv.Emailed {
					cmd.Printf("emailed to %s\n", email)
				} else {
					cmd.Printf("warning: %s\n", inv.Warning)
				}
			}
			return nil
		},
	}
	create.Flags().StringVar(&role, "role", "member", "role for the invited user (admin|member)")
	create.Flags().StringVar(&email, "email", "", "email the invite link to this address")

	list := &cobra.Command{
		Use:   "list",
		Short: "List invites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			invs, err := c.ListInvites()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TOKEN\tROLE\tEXPIRES\tUSED")
			for _, i := range invs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n", i.Token, i.Role, i.ExpiresAt, i.Used)
			}
			return tw.Flush()
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke <token>",
		Short: "Revoke an invite",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.RevokeInvite(args[0])
		},
	}

	cmd.AddCommand(create, list, revoke)
	return cmd
}
