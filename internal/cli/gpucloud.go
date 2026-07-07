package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// gpuCmd manages rented GPU cloud VMs (vast.ai, Nebius): store the provider
// credentials, browse offers, rent a VM that auto-joins the cluster, list,
// and destroy.
func gpuCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gpu",
		Short: "Rent GPU cloud VMs that auto-join the cluster (admin)",
	}
	cmd.AddCommand(gpuKeyCmd(), gpuOffersCmd(), gpuRentCmd(), gpuLsCmd(), gpuStopCmd())
	return cmd
}

func gpuKeyCmd() *cobra.Command {
	var provider, saID, pubkeyID, privateKeyFile, parentID, subnetID string
	cmd := &cobra.Command{
		Use:   "key [api-key]",
		Short: "Store the GPU provider credentials (sealed at rest)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			switch provider {
			case "", "vastai":
				if len(args) != 1 {
					return fmt.Errorf("vastai requires the api-key argument")
				}
				if err := c.SetGPUKey("vastai", args[0]); err != nil {
					return err
				}
				cmd.Println("stored vastai API key")
				cmd.Println("$ luncur gpu key <api-key>")
				return nil
			case "nebius":
				var missing []string
				if saID == "" {
					missing = append(missing, "--sa-id")
				}
				if pubkeyID == "" {
					missing = append(missing, "--pubkey-id")
				}
				if privateKeyFile == "" {
					missing = append(missing, "--private-key-file")
				}
				if parentID == "" {
					missing = append(missing, "--parent-id")
				}
				if subnetID == "" {
					missing = append(missing, "--subnet-id")
				}
				if len(missing) > 0 {
					return fmt.Errorf("--provider nebius requires: %s", strings.Join(missing, ", "))
				}
				pemBytes, err := os.ReadFile(privateKeyFile)
				if err != nil {
					return fmt.Errorf("read private key file: %w", err)
				}
				if err := c.SetNebiusCreds(saID, pubkeyID, pemBytes, parentID, subnetID); err != nil {
					return err
				}
				cmd.Println("stored nebius credentials")
				// CLI-echo: never print the PEM contents, only the file path.
				cmd.Printf("$ luncur gpu key --provider nebius --sa-id %s --pubkey-id %s --private-key-file %s --parent-id %s --subnet-id %s\n",
					saID, pubkeyID, privateKeyFile, parentID, subnetID)
				return nil
			default:
				return fmt.Errorf("unsupported provider %q (vastai, nebius)", provider)
			}
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "vastai", "GPU provider (vastai, nebius)")
	cmd.Flags().StringVar(&saID, "sa-id", "", "Nebius service account id")
	cmd.Flags().StringVar(&pubkeyID, "pubkey-id", "", "Nebius public key id")
	cmd.Flags().StringVar(&privateKeyFile, "private-key-file", "", "path to Nebius service-account private key PEM file")
	cmd.Flags().StringVar(&parentID, "parent-id", "", "Nebius parent (project/folder) id")
	cmd.Flags().StringVar(&subnetID, "subnet-id", "", "Nebius subnet id")
	return cmd
}

func gpuOffersCmd() *cobra.Command {
	var gpuName string
	var numGPUs, limit int
	cmd := &cobra.Command{
		Use:   "offers",
		Short: "Search rentable VM offers, cheapest first (vast.ai)",
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
	var provider, gpuName, platform, preset string
	var diskGB, numGPUs int
	cmd := &cobra.Command{
		Use:   "rent [offer-id]",
		Short: "Rent an offer (vast.ai) or platform/preset (Nebius) as a VM that auto-joins the cluster",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := client.GPURentReq{DiskGB: diskGB, GPUName: gpuName, NumGPUs: numGPUs}
			var echo string
			switch provider {
			case "", "vastai":
				if len(args) != 1 {
					return fmt.Errorf("vastai requires the offer-id argument")
				}
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil {
					return fmt.Errorf("invalid offer id %q", args[0])
				}
				req.Provider = "vastai"
				req.OfferID = id
				echo = fmt.Sprintf("$ luncur gpu rent %d --disk %d", id, diskGB)
			case "nebius":
				if strings.TrimSpace(platform) == "" || strings.TrimSpace(preset) == "" {
					return fmt.Errorf("--provider nebius requires --platform and --preset")
				}
				req.Provider = "nebius"
				req.Platform = platform
				req.Preset = preset
				echo = fmt.Sprintf("$ luncur gpu rent --provider nebius --platform %s --preset %s --disk %d", platform, preset, diskGB)
			default:
				return fmt.Errorf("unsupported provider %q (vastai, nebius)", provider)
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			g, err := c.RentGPU(req)
			if err != nil {
				return err
			}
			cmd.Printf("rented: %s (contract %s)\n", g.Label, g.ExternalID)
			cmd.Println("the VM installs a K3s agent at boot; watch it appear with: luncur node ls")
			cmd.Println(echo)
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "vastai", "GPU provider (vastai, nebius)")
	cmd.Flags().IntVar(&diskGB, "disk", 40, "disk size GB")
	cmd.Flags().StringVar(&gpuName, "gpu", "", "GPU model (recorded for the panel)")
	cmd.Flags().IntVar(&numGPUs, "count", 0, "GPU count (recorded for the panel)")
	cmd.Flags().StringVar(&platform, "platform", "", "Nebius platform, e.g. gpu-h100-sxm")
	cmd.Flags().StringVar(&preset, "preset", "", "Nebius preset, e.g. 1gpu-16vcpu-200gb")
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
			cmd.Printf("%-4s %-22s %-16s %5s %-10s %-8s %-10s %s\n", "ID", "LABEL", "GPU", "COUNT", "STATE", "PROVIDER", "PROV-STATE", "$/HR")
			for _, g := range list {
				cmd.Printf("%-4d %-22s %-16s %5d %-10s %-8s %-10s %.3f\n", g.ID, g.Label, g.GPUName, g.NumGPUs, g.Status, g.Provider, g.ProviderStatus, g.DPHTotal)
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
