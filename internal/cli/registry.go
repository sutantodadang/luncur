package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func registryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage the in-cluster image registry (admin only)",
	}

	gc := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim registry storage: delete manifests outside the retention policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			report, err := c.RegistryGC()
			if err != nil {
				return err
			}
			bytes := "unknown"
			if report.BytesReclaimed >= 0 {
				bytes = humanBytes(report.BytesReclaimed)
			}
			cmd.Printf("deleted %d manifest(s), reclaimed %s\n", report.DeletedManifests, bytes)
			for _, w := range report.Warnings {
				cmd.Printf("warning: %s\n", w)
			}
			return nil
		},
	}

	cmd.AddCommand(gc)
	return cmd
}

// humanBytes formats a non-negative byte count as a binary (KiB/MiB/...)
// human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
