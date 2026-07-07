package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

// gpuCmd manages rented GPU cloud VMs (vast.ai): store the API key, browse
// offers, rent a VM that auto-joins the cluster, list, and destroy.
func gpuCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gpu",
		Short: "Rent GPU cloud VMs that auto-join the cluster (admin)",
	}
	cmd.AddCommand(gpuKeyCmd(), gpuOffersCmd(), gpuRentCmd(), gpuLsCmd(), gpuStopCmd())
	return cmd
}

func gpuKeyCmd() *cobra.Command {
	provider := "vastai"
	cmd := &cobra.Command{
		Use:   "key <api-key>",
		Short: "Store the GPU provider API key (sealed at rest)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetGPUKey(provider, args[0]); err != nil {
				return err
			}
			cmd.Printf("stored %s API key\n", provider)
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "vastai", "GPU provider (vastai)")
	return cmd
}

func gpuOffersCmd() *cobra.Command {
	var gpuName string
	var numGPUs, limit int
	cmd := &cobra.Command{
		Use:   "offers",
		Short: "Search rentable VM offers, cheapest first",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			offers, err := c.GPUOffers(gpuName, numGPUs, limit)
			if err != nil {
				return err
			}
			if len(offers) == 0 {
				cmd.Println("no offers matched")
				return nil
			}
			cmd.Printf("%-12s %-16s %5s %10s %8s %s\n", "OFFER", "GPU", "COUNT", "$/HR", "DISK", "WHERE")
			for _, o := range offers {
				cmd.Printf("%-12d %-16s %5d %10.3f %7.0fG %s\n", o.ID, o.GPUName, o.NumGPUs, o.DPHTotal, o.DiskSpace, o.Geolocation)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&gpuName, "gpu", "", "GPU model filter, e.g. \"RTX 4090\"")
	cmd.Flags().IntVar(&numGPUs, "count", 0, "exact GPU count (0 = any)")
	cmd.Flags().IntVar(&limit, "limit", 10, "max offers")
	return cmd
}

func gpuRentCmd() *cobra.Command {
	var diskGB, numGPUs int
	var gpuName string
	cmd := &cobra.Command{
		Use:   "rent <offer-id>",
		Short: "Rent an offer as a VM that auto-joins the cluster as a GPU node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid offer id %q", args[0])
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			g, err := c.RentGPU(id, diskGB, gpuName, numGPUs)
			if err != nil {
				return err
			}
			cmd.Printf("rented: %s (contract %d)\n", g.Label, g.ExternalID)
			cmd.Println("the VM installs a K3s agent at boot; watch it appear with: luncur node ls")
			return nil
		},
	}
	cmd.Flags().IntVar(&diskGB, "disk", 40, "disk size GB")
	cmd.Flags().StringVar(&gpuName, "gpu", "", "GPU model (recorded for the panel)")
	cmd.Flags().IntVar(&numGPUs, "count", 0, "GPU count (recorded for the panel)")
	return cmd
}

func gpuLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List rented GPU instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			list, err := c.ListGPUInstances()
			if err != nil {
				return err
			}
			if len(list) == 0 {
				cmd.Println("no rented GPU instances")
				return nil
			}
			cmd.Printf("%-4s %-22s %-16s %5s %-10s %-10s %s\n", "ID", "LABEL", "GPU", "COUNT", "STATE", "PROVIDER", "$/HR")
			for _, g := range list {
				cmd.Printf("%-4d %-22s %-16s %5d %-10s %-10s %.3f\n", g.ID, g.Label, g.GPUName, g.NumGPUs, g.Status, g.ProviderStatus, g.DPHTotal)
			}
			return nil
		},
	}
}

func gpuStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "Destroy a rented GPU instance (billing stops, data is gone)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid instance id %q", args[0])
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DestroyGPUInstance(id); err != nil {
				return err
			}
			cmd.Println("destroyed")
			return nil
		},
	}
}
