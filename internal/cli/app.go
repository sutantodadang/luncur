package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

func appCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Manage apps",
	}

	var project string
	var env string
	var port int
	var gitURL, branch string
	var kind, schedule string
	var buildPath string
	var internal bool
	var gpu int64
	var modelSource, runtime string
	var cpu, memory string
	var gitToken string

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			var a client.AppInfo
			switch {
			case kind == "model":
				a, err = c.CreateModelApp(project, args[0], modelSource, runtime, gpu)
			case gitURL != "":
				a, err = c.CreateGitApp(project, args[0], port, gitURL, branch, kind, schedule, buildPath, internal, gpu)
			default:
				a, err = c.CreateApp(project, args[0], port, kind, schedule, buildPath, internal, gpu)
			}
			if err != nil {
				return err
			}
			if cpu != "" || memory != "" {
				var cpuArg, memArg *string
				if cpu != "" {
					cpuArg = &cpu
				}
				if memory != "" {
					memArg = &memory
				}
				if err := c.Scale(project, args[0], nil, cpuArg, memArg, nil); err != nil {
					return err
				}
			}
			if gitToken != "" && gitURL != "" {
				if err := c.SetGitToken(project, args[0], gitToken); err != nil {
					return err
				}
			}
			if kind == "model" {
				cmd.Printf("created %s (kind model, runtime %s)\n", a.Name, a.Runtime)
				if a.Status != "" {
					cmd.Printf("deploying — endpoint will serve OpenAI-compatible /v1 at %s\n", a.URL)
				}
				return nil
			}
			cmd.Printf("created %s (kind %s, port %d)\n", a.Name, a.Kind, a.Port)
			return nil
		},
	}
	create.Flags().StringVar(&project, "project", "", "project name")
	create.MarkFlagRequired("project")
	create.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
	// port is validated server-side: required for web, must be 0 for
	// worker/cron. Not marked required here so worker/cron creation doesn't
	// need a throwaway --port.
	create.Flags().IntVar(&port, "port", 0, "container port (web apps only)")
	create.Flags().StringVar(&gitURL, "git-url", "", "git repo URL (creates a git-source app)")
	create.Flags().StringVar(&branch, "branch", "", "git branch (default: main)")
	create.Flags().StringVar(&kind, "kind", "web", "app kind: web, worker, or cron")
	create.Flags().StringVar(&schedule, "schedule", "", "cron schedule, 5-field (cron kind only)")
	create.Flags().StringVar(&buildPath, "path", "", "subdirectory to build (monorepo)")
	create.Flags().BoolVar(&internal, "internal", false, "cluster-only web app: ClusterIP Service, no Ingress, no public URL (web kind only)")
	create.Flags().Int64Var(&gpu, "gpu", 0, "number of nvidia.com/gpu devices (schedules on GPU nodes only)")
	create.Flags().StringVar(&modelSource, "source", "", "model source: hf:<org>/<name>[/<file>] or s3:<key> (model kind only)")
	create.Flags().StringVar(&runtime, "runtime", "auto", "model runtime: auto, llamacpp, vllm, or custom (model kind only)")
	create.Flags().StringVar(&cpu, "cpu", "", "CPU request+limit applied after create (e.g. 4000m, 4)")
	create.Flags().StringVar(&memory, "memory", "", "memory request+limit applied after create (e.g. 8192, 8Gi)")
	create.Flags().StringVar(&gitToken, "git-token", "", "access token to clone a private git repo (git-source apps; e.g. a GitHub PAT)")

	var listProject, listEnv string
	list := &cobra.Command{
		Use:   "list",
		Short: "List apps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(listEnv)
			apps, err := c.ListApps(listProject)
			if err != nil {
				return err
			}
			for _, a := range apps {
				url := a.URL
				switch {
				case a.Kind != "web":
					url = "-"
				case a.Internal:
					url = a.InternalURL
				}
				cmd.Printf("%s\t%s\t%s\n", a.Name, a.Kind, url)
			}
			return nil
		},
	}
	list.Flags().StringVar(&listProject, "project", "", "project name")
	list.MarkFlagRequired("project")
	list.Flags().StringVar(&listEnv, "env", "", "environment (default: the project's default env)")

	var infoProject, infoEnv string
	info := &cobra.Command{
		Use:   "info <name>",
		Short: "Show app details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(infoEnv)
			a, err := c.GetApp(infoProject, args[0])
			if err != nil {
				return err
			}
			url := a.URL
			if a.Internal {
				url = a.InternalURL
			}
			cmd.Printf("%s\tkind=%s\tstatus=%s\turl=%s\timage=%s", a.Name, a.Kind, a.Status, url, a.Image)
			if a.Kind == "cron" {
				cmd.Printf("\tschedule=%s", a.Schedule)
			}
			cmd.Println()
			return nil
		},
	}
	info.Flags().StringVar(&infoProject, "project", "", "project name")
	info.MarkFlagRequired("project")
	info.Flags().StringVar(&infoEnv, "env", "", "environment (default: the project's default env)")

	var rawProject string
	raw := &cobra.Command{
		Use:   "raw <name>",
		Short: "Print the rendered manifest for an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			out, err := c.Raw(rawProject, args[0], false)
			if err != nil {
				return err
			}
			cmd.Print(string(out))
			return nil
		},
	}
	raw.Flags().StringVar(&rawProject, "project", "", "project name")
	raw.MarkFlagRequired("project")

	var trainProject string
	var trainNodes int
	var trainFramework string
	training := &cobra.Command{
		Use:   "training <app>",
		Short: "Set a job app's default multi-node run shape (nodes/framework)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetTraining(trainProject, args[0], trainNodes, trainFramework); err != nil {
				return err
			}
			cmd.Printf("training defaults for %s: nodes=%d framework=%q\n", args[0], trainNodes, trainFramework)
			cmd.Printf("$ luncur app training %s --project %s --nodes %d --framework %s\n", args[0], trainProject, trainNodes, trainFramework)
			return nil
		},
	}
	training.Flags().StringVar(&trainProject, "project", "", "project name")
	training.MarkFlagRequired("project")
	training.Flags().IntVar(&trainNodes, "nodes", 1, "default number of nodes a run spans")
	training.MarkFlagRequired("nodes")
	training.Flags().StringVar(&trainFramework, "framework", "", "rendezvous env preset: torchrun|torch (empty = LUNCUR_* contract only)")

	cmd.AddCommand(create, list, info, raw, training, gitTokenCmd(), ejectCmd(), adoptCmd(), appS3EnvCmd(),
		pauseCmd(), resumeCmd(), runNowCmd(), cronRunsCmd())
	return cmd
}

// gitTokenCmd manages an app's private-repo clone token. The token is
// write-only server-side (sealed at rest, never read back), so there is no
// "get" — only set and clear.
func gitTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git-token",
		Short: "Manage the access token used to clone a private git repo",
	}

	var setProject, token string
	set := &cobra.Command{
		Use:   "set <app>",
		Short: "Set the private-repo clone token (--token, or piped on stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t := token
			if t == "" {
				raw, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				t = strings.TrimSpace(string(raw))
			}
			if t == "" {
				return fmt.Errorf("token is required (--token or pipe it on stdin)")
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetGitToken(setProject, args[0], t); err != nil {
				return err
			}
			cmd.Printf("git token set on %s\n", args[0])
			return nil
		},
	}
	set.Flags().StringVar(&setProject, "project", "", "project name")
	set.MarkFlagRequired("project")
	set.Flags().StringVar(&token, "token", "", "access token (omit to read from stdin)")

	var clearProject string
	clear := &cobra.Command{
		Use:   "clear <app>",
		Short: "Remove the stored private-repo clone token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.ClearGitToken(clearProject, args[0]); err != nil {
				return err
			}
			cmd.Printf("git token cleared on %s\n", args[0])
			return nil
		},
	}
	clear.Flags().StringVar(&clearProject, "project", "", "project name")
	clear.MarkFlagRequired("project")

	cmd.AddCommand(set, clear)
	return cmd
}
