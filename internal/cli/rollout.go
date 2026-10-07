package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// rolloutCmd inspects and steers canary / blue-green rollouts.
func rolloutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollout",
		Short: "Inspect, promote or abort a canary / blue-green rollout",
	}
	var project, env string
	mk := func(use, short string, run func(cmd *cobra.Command, app string) error) *cobra.Command {
		c := &cobra.Command{Use: use + " <app>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return run(cmd, args[0]) }}
		c.Flags().StringVar(&project, "project", "", "project name")
		c.MarkFlagRequired("project")
		c.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
		return c
	}
	cmd.AddCommand(mk("status", "Show the app's rollout progress", func(cmd *cobra.Command, app string) error {
		c, err := apiClient()
		if err != nil {
			return err
		}
		c.SetEnv(env)
		ro, err := c.GetRollout(project, app)
		if err != nil {
			return err
		}
		if !ro.Active {
			cmd.Println("no canary or blue-green rollout in progress")
			return nil
		}
		steps := make([]string, len(ro.Steps))
		for i, w := range ro.Steps {
			mark := " "
			switch {
			case ro.Phase == "promoting" || i < ro.Step:
				mark = "✓"
			case i == ro.Step && ro.Phase != "starting":
				mark = "▸"
			}
			steps[i] = fmt.Sprintf("%s%d%%", mark, w)
		}
		cmd.Printf("deploy #%d  %s  %s\n", ro.Seq, ro.Strategy, ro.Phase)
		cmd.Printf("steps:   %s → promote\n", strings.Join(steps, " → "))
		cmd.Printf("traffic: %d%% on the new image · probe success %d%% (%d/%d)\n", ro.Weight, ro.SuccessRate, ro.ProbesOK, ro.ProbesTotal)
		if ro.Note != "" {
			cmd.Printf("note:    %s\n", ro.Note)
		}
		return nil
	}))
	for _, action := range []string{"promote", "abort"} {
		action := action
		short := map[string]string{"promote": "Skip the remaining steps and promote the new image now", "abort": "Abort the rollout; traffic returns to the current version"}[action]
		cmd.AddCommand(mk(action, short, func(cmd *cobra.Command, app string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			if err := c.RolloutAction(project, app, action); err != nil {
				return err
			}
			cmd.Printf("%s requested — follow with: luncur rollout status %s --project %s\n", action, app, project)
			return nil
		}))
	}
	return cmd
}
