package cli

import "github.com/spf13/cobra"

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the logged-in user",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			me, err := c.Me()
			if err != nil {
				return err
			}
			cmd.Printf("%s (%s)\n", me.Email, me.Role)
			return nil
		},
	}
}
