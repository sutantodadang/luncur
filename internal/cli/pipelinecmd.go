package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// pipelineEnvHelp is appended to create/update's long help: step env in
// pipeline.yaml is stored in plaintext (see internal/pipeline's Compile doc
// comment), so secrets don't belong there.
const pipelineEnvHelp = "\n\nstep env is stored in plaintext — put secrets in app env instead."

// pipelineCmd is the "luncur pipeline" parent command: create/update/ls/run/
// status/stop/rm for ML pipelines (DAGs of job-app runs, inline image Jobs,
// and deploy/scale/notify actions), driven by luncur's native orchestrator.
func pipelineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipeline",
		Short: "ML pipelines: DAGs of runs, jobs, and deploy/scale/notify actions",
	}
	cmd.AddCommand(
		pipelineCreateCmd(),
		pipelineUpdateCmd(),
		pipelineListCmd(),
		pipelineRunCmd(),
		pipelineStatusCmd(),
		pipelineStopCmd(),
		pipelineRmCmd(),
		pipelineWebhookCmd(),
	)
	return cmd
}

func pipelineCreateCmd() *cobra.Command {
	var project, file, engine, cron string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a pipeline from a pipeline.yaml file",
		Long:  "Create a pipeline from a pipeline.yaml file." + pipelineEnvHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("read %s: %w", file, err)
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			pl, err := c.CreatePipeline(project, args[0], string(b), engine, cron)
			if err != nil {
				return err
			}
			cmd.Printf("pipeline %s created\n", pl.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&file, "file", "", "path to pipeline.yaml")
	cmd.MarkFlagRequired("file")
	cmd.Flags().StringVar(&engine, "engine", "", "orchestrator engine: native|argo (empty = follow the pipeline_engine setting)")
	cmd.Flags().StringVar(&cron, "cron", "", "5-field cron schedule, e.g. \"0 3 * * *\" (empty = manual trigger only)")
	return cmd
}

func pipelineUpdateCmd() *cobra.Command {
	var project, file, engine, cron string
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a pipeline's yaml, engine, and/or cron schedule",
		Long:  "Update a pipeline's yaml, engine, and/or cron schedule. Omitted flags keep their current value; --cron \"\" clears the schedule." + pipelineEnvHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var yamlPtr *string
			if file != "" {
				b, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("read %s: %w", file, err)
				}
				s := string(b)
				yamlPtr = &s
			}
			var enginePtr *string
			if cmd.Flags().Changed("engine") {
				enginePtr = &engine
			}
			var cronPtr *string
			if cmd.Flags().Changed("cron") {
				cronPtr = &cron
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			pl, err := c.UpdatePipeline(project, args[0], yamlPtr, enginePtr, cronPtr)
			if err != nil {
				return err
			}
			cmd.Printf("pipeline %s updated\n", pl.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&file, "file", "", "path to pipeline.yaml (omit to keep the current yaml)")
	cmd.Flags().StringVar(&engine, "engine", "", "orchestrator engine: native|argo (omit to keep the current engine)")
	cmd.Flags().StringVar(&cron, "cron", "", "5-field cron schedule (omit to keep the current schedule; pass \"\" to clear it)")
	return cmd
}

func pipelineListCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List a project's pipelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			pipelines, err := c.ListPipelines(project)
			if err != nil {
				return err
			}
			cmd.Printf("NAME\tENGINE\tCRON\tLAST RUN\tSTATUS\n")
			for _, pl := range pipelines {
				engine := pl.Engine
				if engine == "" {
					engine = "native"
				}
				lastRun, status := "-", "-"
				if pl.LastRun != nil {
					lastRun, status = pl.LastRun.ID, pl.LastRun.Status
				}
				cmd.Printf("%s\t%s\t%s\t%s\t%s\n", pl.Name, engine, dashIfEmpty(pl.Cron), lastRun, status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

func pipelineRunCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Manually trigger a pipeline run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			run, err := c.StartPipelineRun(project, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("run %s started: %d steps\n", run.ID, len(run.Steps))
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

func pipelineStatusCmd() *cobra.Command {
	var project, pipelineName string
	cmd := &cobra.Command{
		Use:   "status <run-id>",
		Short: "Show a pipeline run's steps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			run, err := c.GetPipelineRun(project, pipelineName, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("STEP\tKIND\tSTATE\tATTEMPT\tDETAIL\tDURATION\n")
			for _, st := range run.Steps {
				cmd.Printf("%s\t%s\t%s\t%d\t%s\t%s\n",
					st.Name, st.Kind, st.State, st.Attempt, dashIfEmpty(st.Detail), pipelineStepDuration(st))
			}
			cmd.Printf("run %s: %s\n", run.ID, run.Status)
			if run.Warning != "" {
				cmd.Printf("warning: %s\n", run.Warning)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&pipelineName, "pipeline", "", "pipeline name")
	cmd.MarkFlagRequired("pipeline")
	return cmd
}

func pipelineStopCmd() *cobra.Command {
	var project, pipelineName string
	cmd := &cobra.Command{
		Use:   "stop <run-id>",
		Short: "Stop a running pipeline run (idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			run, err := c.StopPipelineRun(project, pipelineName, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("run %s: %s\n", run.ID, run.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&pipelineName, "pipeline", "", "pipeline name")
	cmd.MarkFlagRequired("pipeline")
	return cmd
}

func pipelineRmCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a pipeline (refuses while a run is in progress)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeletePipeline(project, args[0]); err != nil {
				return err
			}
			cmd.Printf("pipeline %s deleted\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}

func pipelineWebhookCmd() *cobra.Command {
	var project string
	var disable bool
	cmd := &cobra.Command{
		Use:   "webhook <name>",
		Short: "Generate (or rotate) a pipeline's trigger webhook, or disable it",
		Long: "Generate (or, if one already exists, rotate) a pipeline's trigger webhook URL " +
			"and secret. The secret is printed once — store it now. Pass --disable to turn " +
			"the webhook off instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if disable {
				if err := c.DisablePipelineWebhook(project, args[0]); err != nil {
					return err
				}
				cmd.Println("webhook: disabled")
				return nil
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			path, secret, err := c.PipelineWebhookSecret(project, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("webhook URL: %s%s\n", cfg.Server, path)
			cmd.Printf("secret: %s\n", secret)
			cmd.Println("shown once — rotate by re-running")
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().BoolVar(&disable, "disable", false, "disable the pipeline's trigger webhook")
	return cmd
}

// dashIfEmpty renders "-" for an empty table cell (compactParams/other CLI
// tables' convention).
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pipelineTimeLayout is the wire format for PipelineStepInfo's
// started_at/finished_at: SQLite's datetime('now'), UTC, no zone suffix.
const pipelineTimeLayout = "2006-01-02 15:04:05"

// pipelineStepDuration renders a step's elapsed wall time: finished_at -
// started_at once both are set, time.Now() - started_at while it's still
// running, or "-" for a step that never launched (pending/skipped) or a
// timestamp that fails to parse.
func pipelineStepDuration(st client.PipelineStepInfo) string {
	if st.StartedAt == "" {
		return "-"
	}
	started, err := time.Parse(pipelineTimeLayout, st.StartedAt)
	if err != nil {
		return "-"
	}
	end := time.Now().UTC()
	if st.FinishedAt != "" {
		finished, err := time.Parse(pipelineTimeLayout, st.FinishedAt)
		if err != nil {
			return "-"
		}
		end = finished
	}
	return end.Sub(started).Round(time.Second).String()
}
