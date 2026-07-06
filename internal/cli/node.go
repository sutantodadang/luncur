package cli

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/up"
)

// nodeCmd groups cluster-node inspection and the join-command helper.
func nodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Inspect and join cluster nodes",
	}
	cmd.AddCommand(nodeLsCmd())
	cmd.AddCommand(nodeJoinCommandCmd())
	return cmd
}

func nodeLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List cluster nodes (admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			nodes, err := c.ListNodes()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tROLE\tSTATUS\tIP\tVERSION")
			for _, n := range nodes {
				status := "NotReady"
				if n.Ready {
					status = "Ready"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.Name, n.Role, status, n.IP, n.Version)
			}
			return tw.Flush()
		},
	}
}

func nodeJoinCommandCmd() *cobra.Command {
	var ip string
	cmd := &cobra.Command{
		Use:   "join-command",
		Short: "Print the command to join a new VPS to this cluster (run on the server)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("luncur node join-command reads the K3s node token and must run on the server machine (linux)")
			}
			ctx := cmd.Context()

			raw, err := os.ReadFile(up.NodeTokenPath)
			if err != nil {
				return fmt.Errorf("read node token: %w (is this the K3s server machine?)", err)
			}
			token := strings.TrimSpace(string(raw))

			if ip == "" {
				kc, err := kube.New(up.K3sKubeconfig)
				if err != nil {
					return fmt.Errorf("connect to kubernetes: %w (use --ip)", err)
				}
				if ip, err = kc.NodeIP(ctx); err != nil {
					return fmt.Errorf("detect IP: %w (use --ip)", err)
				}
			}

			cmd.Println("run this on the new VPS (as root):")
			cmd.Println()
			cmd.Printf("  luncur join https://%s:6443 --token %s\n", ip, token)
			return nil
		},
	}
	cmd.Flags().StringVar(&ip, "ip", "", "server's public IP (default: detect from the node)")
	return cmd
}
