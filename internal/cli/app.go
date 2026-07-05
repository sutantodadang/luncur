package cli

import (
	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

func appCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Manage apps",
	}

	var project string
	var port int
	var gitURL, branch string
	var kind, schedule string
	var buildPath string
	var internal bool

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			var a client.AppInfo
			if gitURL != "" {
				a, err = c.CreateGitApp(project, args[0], port, gitURL, branch, kind, schedule, buildPath, internal)
			} else {
				a, err = c.CreateApp(project, args[0], port, kind, schedule, buildPath, internal)
			}
			if err != nil {
				return err
			}
			cmd.Printf("created %s (kind %s, port %d)\n", a.Name, a.Kind, a.Port)
			return nil
		},
	}
	create.Flags().StringVar(&project, "project", "", "project name")
	create.MarkFlagRequired("project")
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

	var listProject string
	list := &cobra.Command{
		Use:   "list",
		Short: "List apps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
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

	var infoProject string
	info := &cobra.Command{
		Use:   "info <name>",
		Short: "Show app details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
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

	cmd.AddCommand(create, list, info, raw, ejectCmd(), adoptCmd())
	return cmd
}
