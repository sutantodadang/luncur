package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func volumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage persistent volumes for an app",
	}

	var addProject, addName string
	var addSize int
	add := &cobra.Command{
		Use:   "add <app> <path>",
		Short: "Attach a persistent volume mounted at <path> (RWO; forces max 1 replica)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			v, err := c.AddVolume(addProject, args[0], addName, args[1], addSize)
			if err != nil {
				return err
			}
			cmd.Printf("created %s (%dGB at %s)\n", v.Name, v.SizeGB, v.Path)
			if v.Warning != "" {
				cmd.Println(v.Warning)
			}
			return nil
		},
	}
	add.Flags().StringVar(&addProject, "project", "", "project name")
	add.MarkFlagRequired("project")
	add.Flags().IntVar(&addSize, "size", 0, "volume size in GB (1-1000)")
	add.MarkFlagRequired("size")
	add.Flags().StringVar(&addName, "name", "", "volume name (default: last path segment)")

	var listProject string
	list := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's persistent volumes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			volumes, err := c.ListVolumes(listProject, args[0])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPATH\tSIZE")
			for _, v := range volumes {
				fmt.Fprintf(tw, "%s\t%s\t%dGB\n", v.Name, v.Path, v.SizeGB)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&listProject, "project", "", "project name")
	list.MarkFlagRequired("project")

	var removeProject string
	var removePurge bool
	remove := &cobra.Command{
		Use:   "remove <app> <name>",
		Short: "Detach a volume (data is kept unless --purge)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.RemoveVolume(removeProject, args[0], args[1], removePurge); err != nil {
				return err
			}
			if removePurge {
				cmd.Printf("removed %s (data purged)\n", args[1])
			} else {
				cmd.Printf("removed %s (PVC and data kept in cluster)\n", args[1])
			}
			return nil
		},
	}
	remove.Flags().StringVar(&removeProject, "project", "", "project name")
	remove.MarkFlagRequired("project")
	remove.Flags().BoolVar(&removePurge, "purge", false, "also delete the cluster PVC and its data")

	cmd.AddCommand(add, list, remove)
	return cmd
}
