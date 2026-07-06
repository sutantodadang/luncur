package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage app environment variables",
	}

	var setProject string
	set := &cobra.Command{
		Use:   "set <app> KEY=VALUE",
		Short: "Set an environment variable",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			parts := strings.SplitN(args[1], "=", 2)
			if len(parts) != 2 || parts[0] == "" {
				return fmt.Errorf("invalid KEY=VALUE pair: %q", args[1])
			}
			key, value := parts[0], parts[1]
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.EnvSet(setProject, args[0], key, value); err != nil {
				return err
			}
			cmd.Printf("set %s on %s\n", key, args[0])
			return nil
		},
	}
	set.Flags().StringVar(&setProject, "project", "", "project name")
	set.MarkFlagRequired("project")

	var unsetProject string
	unset := &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Unset an environment variable",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.EnvUnset(unsetProject, args[0], args[1]); err != nil {
				return err
			}
			cmd.Printf("unset %s on %s\n", args[1], args[0])
			return nil
		},
	}
	unset.Flags().StringVar(&unsetProject, "project", "", "project name")
	unset.MarkFlagRequired("project")

	var listProject string
	list := &cobra.Command{
		Use:   "list <app>",
		Short: "List environment variables",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			env, err := c.EnvList(listProject, args[0])
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				cmd.Printf("%s=%s\n", k, env[k])
			}
			return nil
		},
	}
	list.Flags().StringVar(&listProject, "project", "", "project name")
	list.MarkFlagRequired("project")

	var pushProject string
	push := &cobra.Command{
		Use:   "push <app> <file>",
		Short: "Bulk-set environment variables from a .env file (use - for stdin)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw []byte
			var err error
			if args[1] == "-" {
				raw, err = io.ReadAll(cmd.InOrStdin())
			} else {
				raw, err = os.ReadFile(args[1])
			}
			if err != nil {
				return err
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			n, err := c.EnvPush(pushProject, args[0], string(raw))
			if err != nil {
				return err
			}
			cmd.Printf("set %d vars on %s\n", n, args[0])
			return nil
		},
	}
	push.Flags().StringVar(&pushProject, "project", "", "project name")
	push.MarkFlagRequired("project")

	cmd.AddCommand(set, unset, list, push)
	return cmd
}
