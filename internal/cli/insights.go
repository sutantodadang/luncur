package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// insightsCmd prints cost and right-sizing insights.
func insightsCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "insights",
		Short: "Show cost estimates and right-sizing recommendations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			rep, err := c.Insights(project)
			if err != nil {
				return err
			}
			if !rep.MetricsOK {
				cmd.Println("note: metrics-server isn't reachable — no usage data (run `luncur doctor`)")
			}
			priced := rep.Prices.CPUCore > 0 || rep.Prices.MemGB > 0
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "APP\tPODS\tCPU REQ/P95\tMEM REQ/PEAK\tRECOMMENDED\tMONTHLY\tSTATUS")
			for _, a := range rep.Apps {
				rec := fmt.Sprintf("(%dh of data)", a.Hours)
				if a.RecCPU > 0 {
					rec = fmt.Sprintf("%dm %dMi", a.RecCPU, a.RecMem)
				}
				monthly := "-"
				if priced {
					monthly = fmt.Sprintf("%s%.2f", rep.Prices.Currency, a.Monthly)
				}
				fmt.Fprintf(tw, "%s/%s\t%d\t%dm/%dm\t%dMi/%dMi\t%s\t%s\t%s\n", a.Project, a.App, a.Replicas, a.CPUReq, a.CPUP95, a.MemReq, a.MemMax, rec, monthly, a.Flag)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if priced {
				cmd.Printf("\nestimated monthly: %s%.2f · potential savings: %s%.2f\n", rep.Prices.Currency, rep.MonthlyTotal, rep.Prices.Currency, rep.SavingsTotal)
			} else {
				cmd.Println("\nset prices to see costs: luncur config set cost_cpu_core_month 20 (and cost_mem_gb_month, cost_gpu_month)")
			}
			for _, a := range rep.Apps {
				if a.ScaleCmd != "" {
					cmd.Printf("  %s: %s\n", a.Flag, a.ScaleCmd)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "limit to one project")
	return cmd
}
