package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// appSetCmd shows or changes an app's reliability and rollout policy:
// auto-rollback, rollout timeout, default probe, pod security level and
// deploy strategy (rolling, canary, blue-green).
func appSetCmd() *cobra.Command {
	var project, env string
	var autoRollback, probe, security, strategy, canarySteps string
	var rolloutTimeout, canaryInterval, bluegreenKeep time.Duration
	var canaryMinSuccess int
	cmd := &cobra.Command{
		Use:   "set <app>",
		Short: "Show or change an app's rollout policy (auto-rollback, probe, security, strategy)",
		Long: `Show or change an app's rollout policy. With no flags, prints it.

  luncur app set web --project shop --auto-rollback off
  luncur app set web --project shop --rollout-timeout 10m
  luncur app set web --project shop --security relaxed
  luncur app set web --project shop --strategy canary --canary-steps 10,50 --canary-interval 2m
  luncur app set web --project shop --strategy bluegreen --bluegreen-keep 10m`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			patch := map[string]any{}
			f := cmd.Flags()
			if f.Changed("auto-rollback") {
				on, err := parseOnOff(autoRollback)
				if err != nil {
					return fmt.Errorf("--auto-rollback: %w", err)
				}
				patch["auto_rollback"] = on
			}
			if f.Changed("rollout-timeout") {
				patch["rollout_timeout"] = int(rolloutTimeout.Seconds())
			}
			if f.Changed("probe") {
				patch["probe"] = probe
			}
			if f.Changed("security") {
				patch["security"] = security
			}
			if f.Changed("strategy") {
				patch["strategy"] = strategy
			}
			if f.Changed("canary-steps") {
				patch["canary_steps"] = canarySteps
			}
			if f.Changed("canary-interval") {
				patch["canary_interval"] = int(canaryInterval.Seconds())
			}
			if f.Changed("canary-min-success") {
				patch["canary_min_success"] = canaryMinSuccess
			}
			if f.Changed("bluegreen-keep") {
				patch["bluegreen_keep"] = int(bluegreenKeep.Seconds())
			}
			var pol client.AppPolicy
			if len(patch) == 0 {
				pol, err = c.GetPolicy(project, args[0])
			} else {
				pol, err = c.SetPolicy(project, args[0], patch)
			}
			if err != nil {
				return err
			}
			printPolicy(cmd, pol)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	f.StringVar(&env, "env", "", "environment (default: the project's default env)")
	f.StringVar(&autoRollback, "auto-rollback", "", "roll back automatically when a rollout fails: on|off")
	f.DurationVar(&rolloutTimeout, "rollout-timeout", 0, "how long new pods may take to become ready (30s-1h)")
	f.StringVar(&probe, "probe", "", "default readiness probe when no health path is set: auto|off")
	f.StringVar(&security, "security", "", "pod security level: baseline|restricted|relaxed")
	f.StringVar(&strategy, "strategy", "", "deploy strategy: rolling|canary|bluegreen")
	f.StringVar(&canarySteps, "canary-steps", "", "canary traffic weights in percent, e.g. 10,50")
	f.DurationVar(&canaryInterval, "canary-interval", 0, "how long each canary step holds")
	f.IntVar(&canaryMinSuccess, "canary-min-success", 0, "minimum probe success rate (percent) a canary step must keep")
	f.DurationVar(&bluegreenKeep, "bluegreen-keep", 0, "how long the old track stays up after a blue-green switch")
	return cmd
}

func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "true", "yes":
		return true, nil
	case "off", "false", "no":
		return false, nil
	}
	return false, fmt.Errorf("want on or off, got %q", s)
}

func printPolicy(cmd *cobra.Command, p client.AppPolicy) {
	onOff := map[bool]string{true: "on", false: "off"}
	secs := func(n int) string { return (time.Duration(n) * time.Second).String() }
	cmd.Printf("auto-rollback:   %s\n", onOff[p.AutoRollback])
	cmd.Printf("rollout-timeout: %s\n", secs(p.RolloutTimeout))
	cmd.Printf("probe:           %s\n", p.Probe)
	cmd.Printf("security:        %s\n", p.Security)
	cmd.Printf("strategy:        %s\n", p.Strategy)
	switch p.Strategy {
	case "canary":
		steps := make([]string, len(p.CanarySteps))
		for i, n := range p.CanarySteps {
			steps[i] = fmt.Sprintf("%d%%", n)
		}
		cmd.Printf("canary:          %s, %s per step, min success %d%%\n", strings.Join(steps, " → "), secs(p.CanaryInterval), p.CanaryMinSuccess)
	case "bluegreen":
		cmd.Printf("bluegreen-keep:  %s\n", secs(p.BlueGreenKeep))
	}
}
