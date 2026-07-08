package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// sweepCmd is the "luncur sweep" parent command: start/ls/status/stop for
// hyperparameter sweeps over a kind=job app.
func sweepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Hyperparameter sweeps over a job app",
	}
	cmd.AddCommand(sweepStartCmd(), sweepListCmd(), sweepStatusCmd(), sweepStopCmd())
	return cmd
}

func sweepStartCmd() *cobra.Command {
	var project, params, metric, direction, framework string
	var maxTrials, parallel, nodes int
	var earlyStop bool
	cmd := &cobra.Command{
		Use:   "start <app>",
		Short: "Start a hyperparameter sweep for a job app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(params)
			if err != nil {
				return fmt.Errorf("read %s: %w", params, err)
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			sw, err := c.StartSweep(project, args[0], string(b), metric, direction, maxTrials, parallel, earlyStop, nodes, framework)
			if err != nil {
				return err
			}
			cmd.Printf("sweep %s started: %d trials (parallel %d)\n", sw.ID, len(sw.Trials), sw.Parallel)
			if sw.Truncated {
				cmd.Printf("warning: param space truncated to %d trials (--max-trials)\n", sw.MaxTrials)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&params, "params", "", "path to params.yaml (grid/random search space)")
	cmd.MarkFlagRequired("params")
	cmd.Flags().StringVar(&metric, "metric", "", "metric name a trial reports (MLflow or luncur-metric log line)")
	cmd.MarkFlagRequired("metric")
	cmd.Flags().StringVar(&direction, "direction", "min", "optimize direction: min|max")
	cmd.Flags().IntVar(&maxTrials, "max-trials", 20, "maximum number of trials")
	cmd.Flags().IntVar(&parallel, "parallel", 2, "trials to run concurrently")
	cmd.Flags().IntVar(&nodes, "nodes", 0, "nodes per trial (0 = app default)")
	cmd.Flags().StringVar(&framework, "framework", "", "rendezvous env preset: torchrun|torch (empty = app default)")
	cmd.Flags().BoolVar(&earlyStop, "early-stop", false, "prune trials worse than the running median once >=3 trials finish")
	return cmd
}

func sweepListCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "ls <app>",
		Short: "List a job app's sweeps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			sweeps, err := c.ListSweeps(project, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("ID\tSTATUS\tMETRIC\tDONE/TOTAL\tBEST\n")
			for _, sw := range sweeps {
				best := "-"
				if sw.BestValue != nil {
					best = fmt.Sprintf("%g", *sw.BestValue)
				}
				cmd.Printf("%s\t%s\t%s\t%d/%d\t%s\n", sw.ID, sw.Status, sw.Metric, sw.Counts["done"], sweepTrialTotal(sw.Counts), best)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

// sweepTrialTotal sums a sweep's per-state trial counts into its total
// trial count.
func sweepTrialTotal(counts map[string]int) int {
	n := 0
	for _, v := range counts {
		n += v
	}
	return n
}

func sweepStatusCmd() *cobra.Command {
	var project, app string
	cmd := &cobra.Command{
		Use:   "status <id>",
		Short: "Show a sweep's trials and best result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			sw, err := c.GetSweep(project, app, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("sweep %s: %s (metric %s, direction %s)\n", sw.ID, sw.Status, sw.Metric, sw.Direction)
			if sw.Warning != "" {
				cmd.Printf("warning: %s\n", sw.Warning)
			}
			cmd.Printf("TRIAL\tSTATE\tMETRIC\tPARAMS\n")
			for _, tr := range sw.Trials {
				metricStr := "-"
				if tr.MetricValue != nil {
					metricStr = fmt.Sprintf("%g", *tr.MetricValue)
				}
				cmd.Printf("%s\t%s\t%s\t%s\n", tr.ID, tr.State, metricStr, compactParams(tr.Params))
			}
			if sw.BestTrialID != "" {
				best := "-"
				if sw.BestValue != nil {
					best = fmt.Sprintf("%g", *sw.BestValue)
				}
				cmd.Printf("best: trial %s (%s=%s)\n", sw.BestTrialID, sw.Metric, best)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&app, "app", "", "job app name")
	cmd.MarkFlagRequired("app")
	return cmd
}

// compactParams renders a trial's params as a stable, space-joined k=v
// list (sorted by key) for the status table's PARAMS column.
func compactParams(params map[string]string) string {
	if len(params) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + params[k]
	}
	return strings.Join(parts, " ")
}

func sweepStopCmd() *cobra.Command {
	var project, app string
	cmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a running sweep (idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			sw, err := c.StopSweep(project, app, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("sweep %s: %s\n", sw.ID, sw.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&app, "app", "", "job app name")
	cmd.MarkFlagRequired("app")
	return cmd
}
