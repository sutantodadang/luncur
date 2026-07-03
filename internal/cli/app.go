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
				a, err = c.CreateGitApp(project, args[0], port, gitURL, branch)
			} else {
				a, err = c.CreateApp(project, args[0], port)
			}
			if err != nil {
				return err
			}
			cmd.Printf("created %s (port %d)\n", a.Name, a.Port)
			return nil
		},
	}
	create.Flags().StringVar(&project, "project", "", "project name")
	create.MarkFlagRequired("project")
	create.Flags().IntVar(&port, "port", 0, "container port")
	create.MarkFlagRequired("port")
	create.Flags().StringVar(&gitURL, "git-url", "", "git repo URL (creates a git-source app)")
	create.Flags().StringVar(&branch, "branch", "", "git branch (default: main)")

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
				cmd.Printf("%s\t%s\n", a.Name, a.URL)
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
			cmd.Printf("%s\tstatus=%s\turl=%s\timage=%s\n", a.Name, a.Status, a.URL, a.Image)
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

	cmd.AddCommand(create, list, info, raw)
	return cmd
}
