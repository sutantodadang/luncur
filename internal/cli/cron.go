package cli

import (
	"github.com/spf13/cobra"
)

// pauseCmd is `app pause`: suspends a cron app's schedule. Rides the `app`
// command tree (see app.go) rather than being a top-level command.
func pauseCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "pause <name>",
		Short: "Suspend a cron app's schedule (kind=cron only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.PauseCron(project, args[0]); err != nil {
				return err
			}
			cmd.Printf("%s paused\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

// resumeCmd is `app resume`: reverses pause.
func resumeCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "resume <name>",
		Short: "Resume a paused cron app's schedule (kind=cron only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.ResumeCron(project, args[0]); err != nil {
				return err
			}
			cmd.Printf("%s resumed\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

// runNowCmd is `app run-now`: manually fires a cron app's CronJob. Named
// run-now (not "run") to avoid clashing with the top-level `luncur run`
// command, which triggers kind=job apps.
func runNowCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "run-now <name>",
		Short: "Manually trigger one run of a cron app (kind=cron only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			job, err := c.TriggerCron(project, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("run started (job %s)\n", job)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

// cronRunsCmd is `app runs`: lists a cron app's recent Jobs (newest first).
func cronRunsCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "runs <name>",
		Short: "List a cron app's recent runs (kind=cron only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			runs, err := c.CronRuns(project, args[0])
			if err != nil {
				return err
			}
			for _, r := range runs {
				finished := r.CompletionTime
				if finished == "" {
					finished = "-"
				}
				cmd.Printf("%s\t%s\t%s\t%s\n", r.Name, r.Status, r.StartTime, finished)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
