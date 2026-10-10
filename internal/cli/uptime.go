package cli

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// uptimeCmd shows and configures an app's uptime check.
func uptimeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "uptime", Short: "Show or configure an app's uptime check"}
	var project, env, path string
	var external bool
	flags := func(c *cobra.Command) *cobra.Command {
		c.Flags().StringVar(&project, "project", "", "project name")
		c.MarkFlagRequired("project")
		c.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
		return c
	}
	status := flags(&cobra.Command{Use: "status <app>", Short: "Show the app's uptime and check state", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			u, err := c.GetUptime(project, args[0])
			if err != nil {
				return err
			}
			if !u.Enabled {
				cmd.Printf("uptime check off — enable: luncur uptime enable %s --project %s\n", args[0], project)
				return nil
			}
			cmd.Printf("state:   %s\n", u.State)
			cmd.Printf("probes:  %s\n", u.URL)
			cmd.Printf("uptime:  %.2f%% (24h) · %.2f%% (30d) · %.2f%% (90d) · p95 %dms\n", u.Stats.Pct24h, u.Stats.Pct30d, u.Stats.Pct90d, u.Stats.P95MS)
			if u.LastError != "" {
				cmd.Printf("last:    %s\n", u.LastError)
			}
			return nil
		}})
	set := func(use, short string, on bool) *cobra.Command {
		c := flags(&cobra.Command{Use: use + " <app>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := apiClient()
				if err != nil {
					return err
				}
				c.SetEnv(env)
				patch := map[string]any{"enabled": on}
				if cmd.Flags().Changed("external") {
					patch["external"] = external
				}
				if cmd.Flags().Changed("path") {
					patch["path"] = path
				}
				u, err := c.SetUptime(project, args[0], patch)
				if err != nil {
					return err
				}
				if on {
					cmd.Printf("uptime check on: %s every minute\n", u.URL)
				} else {
					cmd.Println("uptime check off")
				}
				return nil
			}})
		if on {
			c.Flags().BoolVar(&external, "external", false, "probe the public URL (TLS/DNS too) instead of the in-cluster Service")
			c.Flags().StringVar(&path, "path", "", "path to probe (default: the health path, or /)")
		}
		return c
	}
	cmd.AddCommand(status, set("enable", "Turn the app's uptime check on", true), set("disable", "Turn the app's uptime check off", false))
	return cmd
}

// statusPageCmd publishes a project's public status page.
func statusPageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "status-page", Short: "Publish a project's public status page"}
	var project, slug, title, apps string
	show := &cobra.Command{Use: "show", Short: "Show the status page config", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			sp, err := c.GetStatusPage(project)
			if err != nil {
				return err
			}
			if !sp.Enabled {
				cmd.Printf("no status page — publish: luncur status-page enable --project %s --apps web\n", project)
				return nil
			}
			cmd.Printf("%s%s  (%s)\napps: %s\n", c.Server(), sp.Path, sp.Title, strings.Join(sp.Apps, ", "))
			return nil
		}}
	enable := &cobra.Command{Use: "enable", Short: "Publish (or update) the status page", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			var list []string
			for _, a := range strings.Split(apps, ",") {
				if a = strings.TrimSpace(a); a != "" {
					list = append(list, a)
				}
			}
			sp, err := c.SetStatusPage(project, map[string]any{"slug": slug, "title": title, "apps": list})
			if err != nil {
				return err
			}
			cmd.Printf("published: %s%s\nbadge:     %s%s/badge.svg\n", c.Server(), sp.Path, c.Server(), sp.Path)
			return nil
		}}
	enable.Flags().StringVar(&slug, "slug", "", "URL slug (default: the project name)")
	enable.Flags().StringVar(&title, "title", "", "page title (default: the project name)")
	enable.Flags().StringVar(&apps, "apps", "", "comma-separated web apps to show")
	disable := &cobra.Command{Use: "disable", Short: "Unpublish the status page", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeleteStatusPage(project); err != nil {
				return err
			}
			cmd.Println("status page unpublished")
			return nil
		}}
	for _, c := range []*cobra.Command{show, enable, disable} {
		c.Flags().StringVar(&project, "project", "", "project name")
		c.MarkFlagRequired("project")
		cmd.AddCommand(c)
	}
	return cmd
}

// incidentCmd manages a project's incidents.
func incidentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "incident", Short: "List, open, update and resolve incidents"}
	var project, app, body string
	list := &cobra.Command{Use: "list", Short: "List incidents", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			ins, err := c.ListIncidents(project)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tAPP\tOPENED\tTITLE")
			for _, in := range ins {
				a := in.App
				if a == "" {
					a = "-"
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", in.ID, in.Status, a, in.OpenedAt, in.Title)
			}
			return tw.Flush()
		}}
	open := &cobra.Command{Use: "open <title>", Short: "Open an incident (e.g. planned maintenance)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			in, err := c.OpenIncident(project, args[0], app, body)
			if err != nil {
				return err
			}
			cmd.Printf("incident #%d opened\n", in.ID)
			return nil
		}}
	open.Flags().StringVar(&app, "app", "", "affected app (default: project-wide)")
	open.Flags().StringVar(&body, "message", "", "first update")
	note := &cobra.Command{Use: "note <id> <message>", Short: "Add an update to an incident", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("incident id must be a number")
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.IncidentNote(project, id, args[1]); err != nil {
				return err
			}
			cmd.Printf("incident #%d updated\n", id)
			return nil
		}}
	resolve := &cobra.Command{Use: "resolve <id>", Short: "Resolve an incident", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("incident id must be a number")
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.ResolveIncident(project, id); err != nil {
				return err
			}
			cmd.Printf("incident #%d resolved\n", id)
			return nil
		}}
	for _, c := range []*cobra.Command{list, open, note, resolve} {
		c.Flags().StringVar(&project, "project", "", "project name")
		c.MarkFlagRequired("project")
		cmd.AddCommand(c)
	}
	return cmd
}
