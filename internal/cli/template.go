package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// templateCmd lists and installs one-click app templates.
func templateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "template", Short: "Install one-click app templates (app + addons + env)"}
	list := &cobra.Command{Use: "list", Short: "List the template gallery", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			ts, err := c.ListTemplates()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCATEGORY\tADDONS\tIMAGE\tDESCRIPTION")
			for _, t := range ts {
				addons := strings.Join(t.Addons, ",")
				if addons == "" {
					addons = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Category, addons, t.Image, t.Description)
			}
			return tw.Flush()
		}}
	var project, env, name string
	var envs []string
	install := &cobra.Command{Use: "install <template>", Short: "Install a template as a new app", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pairs, err := parseEnvPairs(envs)
			if err != nil {
				return err
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			res, err := c.InstallTemplate(project, args[0], name, pairs)
			for _, st := range res.Steps {
				mark := "✓"
				if !st.OK {
					mark = "✗"
				}
				cmd.Printf("%s %s  %s\n", mark, st.Step, st.Detail)
			}
			if err != nil {
				return err
			}
			cmd.Printf("installed %s → %s (deployment #%d starts once its addons are ready)\n", res.App, res.URL, res.Seq)
			cmd.Printf("follow: luncur status %s --project %s\n", res.App, project)
			return nil
		}}
	install.Flags().StringVar(&project, "project", "", "project name")
	install.MarkFlagRequired("project")
	install.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
	install.Flags().StringVar(&name, "name", "", "app name (default: the template name)")
	install.Flags().StringArrayVarP(&envs, "set", "e", nil, "extra env var KEY=VALUE (repeatable, wins over the template)")
	cmd.AddCommand(list, install)
	return cmd
}
