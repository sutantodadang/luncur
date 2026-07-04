package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// doctorCmd runs the server's one-shot diagnostics and appends a client-side
// version-match row. Any failing check makes the command exit non-zero
// (after printing the full table) so it composes with scripts/monitoring;
// warn-only results still exit 0.
func doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run one-shot server diagnostics (admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			serverVersion, checks, err := c.Doctor()
			if err != nil {
				return err
			}
			checks = append(checks, versionCheck(serverVersion))

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
			failed := false
			for _, ch := range checks {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", ch.Name, ch.Status, ch.Detail)
				if ch.Status == "fail" {
					failed = true
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if failed {
				return fmt.Errorf("doctor found failing checks")
			}
			return nil
		},
	}
	return cmd
}

// versionCheck compares the CLI's own build version against the server's
// reported version.
func versionCheck(serverVersion string) client.DoctorCheck {
	if version == serverVersion {
		return client.DoctorCheck{Name: "version", Status: "ok",
			Detail: fmt.Sprintf("client %s == server %s", version, serverVersion)}
	}
	return client.DoctorCheck{Name: "version", Status: "warn",
		Detail: fmt.Sprintf("client %s != server %s", version, serverVersion)}
}
