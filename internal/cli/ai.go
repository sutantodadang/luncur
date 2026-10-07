package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// aiCmd is the built-in AI assistant: diagnose failures, run ops in plain
// language, and generate validated config. Configure it with `luncur config
// set ai_provider claude` + `luncur config set ai_api_key …` (or an
// OpenAI-compatible endpoint, including a luncur model app).
func aiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ai",
		Short: "AI assistant: explain failures, run ops in plain language, generate config",
	}
	cmd.AddCommand(aiExplainCmd(), aiAskCmd(), aiGenCmd(), aiUsageCmd(), aiStatusCmd())
	return cmd
}

func aiExplainCmd() *cobra.Command {
	var project, env, deploy, run string
	cmd := &cobra.Command{
		Use:   "explain <app>",
		Short: "Explain why an app's latest (or a given) deploy or job run failed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			d, err := c.AIExplain(project, args[0], deploy, run)
			if err != nil {
				return err
			}
			printDiagnosis(cmd, d)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
	cmd.Flags().StringVar(&deploy, "deploy", "", "deploy id (default: the latest failed deploy)")
	cmd.Flags().StringVar(&run, "run", "", "job run id to explain instead of a deploy")
	return cmd
}

// printDiagnosis renders the DESIGN.md error contract.
func printDiagnosis(cmd *cobra.Command, d client.AIDiagnosis) {
	cmd.Printf("what broke   %s\n", d.Broke)
	cmd.Printf("likely why   %s\n", d.Why)
	cmd.Printf("next         $ %s\n", d.NextCommand)
	if d.Confidence != "" {
		cmd.Printf("confidence   %s\n", d.Confidence)
	}
}

func aiAskCmd() *cobra.Command {
	var project, env, conversation string
	cmd := &cobra.Command{
		Use:   "ask <request>",
		Short: "Ask the assistant to inspect or change things (acts with your role)",
		Long: `Ask the assistant to inspect or change things in plain language, e.g.

  luncur ai ask "why is web restarting?" --project shop
  luncur ai ask "add a postgres to api and scale web to 3" --project shop --env staging

It acts through luncur's API as you: anything your role can't do fails, and
every change is audited under your name. Pass --conversation to continue.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			res, err := c.AIChat(conversation, project, strings.Join(args, " "))
			if err != nil {
				return err
			}
			for _, a := range res.Actions {
				mark := "ok "
				if !a.OK {
					mark = "err"
				}
				line := a.CLI
				if line == "" {
					line = a.Tool
				}
				cmd.Printf("[%s] $ %s\n", mark, line)
				if a.Error != "" {
					cmd.Printf("      %s\n", a.Error)
				}
			}
			if len(res.Actions) > 0 {
				cmd.Println()
			}
			cmd.Println(strings.TrimSpace(res.Reply))
			if res.Error != "" {
				cmd.Printf("\nstopped: %s\n", res.Error)
			}
			cmd.Printf("\n(continue: luncur ai ask --conversation %s \"…\")\n", res.ConversationID)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "default project")
	cmd.Flags().StringVar(&env, "env", "", "default environment")
	cmd.Flags().StringVar(&conversation, "conversation", "", "continue an earlier conversation")
	return cmd
}

func aiGenCmd() *cobra.Command {
	var project, env, app, kind, from, output string
	cmd := &cobra.Command{
		Use:   "gen <pipeline|params|override|dockerfile> <description>",
		Short: "Generate a validated pipeline.yaml, sweep params.yaml, override patch, or Dockerfile",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			req := client.AIGenerateRequest{
				Kind: args[0], Description: strings.Join(args[1:], " "),
				Project: project, App: app, OverrideKind: kind,
			}
			if from != "" {
				b, err := os.ReadFile(from)
				if err != nil {
					return err
				}
				req.Current = string(b)
			}
			content, attempts, err := c.AIGenerate(req)
			if err != nil {
				return err
			}
			if output != "" {
				if err := os.WriteFile(output, []byte(content), 0o644); err != nil {
					return err
				}
				cmd.Printf("wrote %s (validated by luncur, %d attempt(s))\n", output, attempts)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), content)
			if !strings.HasSuffix(content, "\n") {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project (lets the generator use real app names and validate app references)")
	cmd.Flags().StringVar(&env, "env", "", "environment")
	cmd.Flags().StringVar(&app, "app", "", "app (override: start from its current manifest)")
	cmd.Flags().StringVar(&kind, "kind", "", "manifest kind for override: Deployment, Service, Ingress or CronJob")
	cmd.Flags().StringVar(&from, "from", "", "existing file to modify")
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout")
	return cmd
}

func aiUsageCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show AI assistant token usage (admins: everyone's; others: their own)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			u, err := c.AIUsage(days)
			if err != nil {
				return err
			}
			budget := "unlimited"
			if u.DailyBudget > 0 {
				budget = fmt.Sprintf("%d", u.DailyBudget)
			}
			cmd.Printf("today: %d tokens (budget %s)\n\n", u.TodayTokens, budget)
			cmd.Printf("%-10s  %-24s  %-9s  %8s  %10s  %10s\n", "DAY", "USER", "WORKFLOW", "REQUESTS", "INPUT", "OUTPUT")
			for _, r := range u.Rows {
				user := r.User
				if user == "" {
					user = "(system)"
				}
				cmd.Printf("%-10s  %-24s  %-9s  %8d  %10d  %10d\n", r.Day, user, r.Workflow, r.Requests, r.InputTokens, r.OutputTokens)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "how many days back")
	return cmd
}

func aiStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the AI assistant is configured",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			st, err := c.AIStatus()
			if err != nil {
				return err
			}
			if enabled, _ := st["enabled"].(bool); !enabled {
				cmd.Printf("AI assistant: off — %v\n", st["reason"])
				return nil
			}
			cmd.Printf("AI assistant: on (%v), notification summaries: %v\n", st["provider"], st["notify"])
			return nil
		},
	}
}
