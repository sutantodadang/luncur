package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Manage state backups (admin only)",
	}

	var noUpload bool
	create := &cobra.Command{
		Use:   "create",
		Short: "Snapshot luncur state (DB, sealer key, addon dumps)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			b, err := c.CreateBackup(noUpload)
			if err != nil {
				return err
			}
			cmd.Printf("backup %d: %s (%d bytes, uploaded: %v)\n", b.ID, b.Path, b.SizeBytes, b.Uploaded)
			for _, w := range b.Warnings {
				cmd.Printf("warning: %s\n", w)
			}
			return nil
		},
	}
	create.Flags().BoolVar(&noUpload, "no-upload", false, "skip the S3 upload even when configured")

	list := &cobra.Command{
		Use:   "list",
		Short: "List backups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			rows, err := c.ListBackups()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tPATH\tSIZE\tUPLOADED\tCREATED")
			for _, b := range rows {
				fmt.Fprintf(tw, "%d\t%s\t%d\t%v\t%s\n", b.ID, b.Path, b.SizeBytes, b.Uploaded, b.CreatedAt)
			}
			return tw.Flush()
		},
	}

	prune := &cobra.Command{
		Use:   "prune",
		Short: "Delete backups beyond the retention count (backup_keep, default 7)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			n, err := c.PruneBackups()
			if err != nil {
				return err
			}
			cmd.Printf("removed %d backup(s)\n", n)
			return nil
		},
	}

	cmd.AddCommand(create, list, prune)
	return cmd
}
