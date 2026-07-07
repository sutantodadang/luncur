package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// runCmd triggers one run of a kind=job app and, unless detached, follows
// its logs and reports the final status.
func runCmd() *cobra.Command {
	var project string
	var detach bool
	cmd := &cobra.Command{
		Use:   "run <app>",
		Short: "Trigger one run of a job app (kind=job)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			run, err := c.CreateRun(project, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("run %d started (job %s)\n", run.ID, run.Job)
			if detach {
				cmd.Printf("follow with: luncur run-logs %s --project %s --id %d\n", args[0], project, run.ID)
				return nil
			}

			// Follow logs; the pod may need a moment to schedule, so retry
			// the stream a few times before giving up on logs (the run
			// itself keeps going either way).
			for attempt := 0; attempt < 20; attempt++ {
				err = c.FollowRunLogs(project, args[0], run.ID, true, cmd.OutOrStdout())
				if err == nil {
					break
				}
				cur, gerr := c.GetRun(project, args[0], run.ID)
				if gerr == nil && cur.Status != "running" {
					break
				}
				time.Sleep(3 * time.Second)
			}

			// Poll to the final status.
			for {
				cur, err := c.GetRun(project, args[0], run.ID)
				if err != nil {
					return err
				}
				if cur.Status != "running" {
					if cur.ExitCode != nil {
						cmd.Printf("run %d %s (exit code %d)\n", cur.ID, cur.Status, *cur.ExitCode)
					} else {
						cmd.Printf("run %d %s\n", cur.ID, cur.Status)
					}
					if cur.Status == "failed" {
						return fmt.Errorf("run failed")
					}
					return nil
				}
				time.Sleep(3 * time.Second)
			}
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().BoolVar(&detach, "detach", false, "start the run and return immediately")
	cmd.AddCommand(runListCmd(), runLogsCmd())
	return cmd
}

func runListCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "ls <app>",
		Short: "List a job app's runs (newest first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(project, args[0])
			if err != nil {
				return err
			}
			for _, r := range runs {
				exit := "-"
				if r.ExitCode != nil {
					exit = fmt.Sprintf("%d", *r.ExitCode)
				}
				cmd.Printf("%d\t%s\texit=%s\t%s\n", r.ID, r.Status, exit, r.StartedAt)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

func runLogsCmd() *cobra.Command {
	var project string
	var id int64
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Print (or follow) one run's logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.FollowRunLogs(project, args[0], id, follow, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().Int64Var(&id, "id", 0, "run id (see: luncur run ls)")
	cmd.MarkFlagRequired("id")
	cmd.Flags().BoolVar(&follow, "follow", false, "keep streaming while the run produces output")
	return cmd
}
