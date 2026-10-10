package cli

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

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
	cmd.AddCommand(nodeCordonCmd("cordon", "Mark a node unschedulable (running pods stay)"))
	cmd.AddCommand(nodeCordonCmd("uncordon", "Mark a node schedulable again"))
	cmd.AddCommand(nodeDrainCmd())
	return cmd
}

func nodeCordonCmd(action, short string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <name>",
		Short: short + " (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.NodeAction(args[0], action, false, 0); err != nil {
				return err
			}
			cmd.Printf("%s %sed\n", args[0], action)
			return nil
		},
	}
}

// nodeDrainCmd cordons a node and evicts its pods (disruption budgets
// respected), following progress until the drain finishes.
func nodeDrainCmd() *cobra.Command {
	var force, noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "drain <name>",
		Short: "Cordon a node and evict its pods for maintenance (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.NodeAction(args[0], "drain", force, int(timeout.Seconds())); err != nil {
				return err
			}
			cmd.Printf("draining %s…\n", args[0])
			if noWait {
				return nil
			}
			last := ""
			deadline := time.Now().Add(timeout + 2*time.Minute)
			for time.Now().Before(deadline) {
				time.Sleep(2 * time.Second)
				nodes, err := c.ListNodes()
				if err != nil {
					return err
				}
				for _, n := range nodes {
					if n.Name != args[0] || n.Drain == nil {
						continue
					}
					line := fmt.Sprintf("  evicted %d/%d", n.Drain.Evicted, n.Drain.Total)
					for _, b := range n.Drain.Blocked {
						line += "\n  blocked: " + b
					}
					if line != last {
						cmd.Println(line)
						last = line
					}
					switch n.Drain.State {
					case "done":
						cmd.Printf("%s drained — run `luncur node uncordon %s` when maintenance is over\n", args[0], args[0])
						return nil
					case "failed":
						return fmt.Errorf("drain failed: %s", n.Drain.Error)
					}
				}
			}
			return fmt.Errorf("timed out following the drain of %s", args[0])
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "drain even the only schedulable node (evicts luncur itself)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "start the drain and return")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to retry evictions blocked by disruption budgets")
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
			fmt.Fprintln(tw, "NAME\tROLE\tSTATUS\tIP\tGPU\tVERSION")
			for _, n := range nodes {
				status := "NotReady"
				if n.Ready {
					status = "Ready"
				}
				if n.Cordoned {
					status += ",SchedulingDisabled"
				}
				gpuCol := "-"
				if n.GPU {
					gpuCol = fmt.Sprintf("%d", n.GPUCapacity)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.Role, status, n.IP, gpuCol, n.Version)
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
