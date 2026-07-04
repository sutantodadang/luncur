package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// auditCmd lists the audit log of successful mutating requests (admin only).
func auditCmd() *cobra.Command {
	var limit int
	var user, contains string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the audit log of mutating requests (admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			entries, err := c.AuditList(limit, user, contains)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTIME\tUSER\tACTION\tTARGET")
			for _, e := range entries {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", e.ID, e.CreatedAt, e.UserEmail, e.Action, e.Target)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to show (server caps at 200)")
	cmd.Flags().StringVar(&user, "user", "", "filter by exact user email")
	cmd.Flags().StringVar(&contains, "contains", "", "filter by substring match on action/target")
	return cmd
}
