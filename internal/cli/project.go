package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

func projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage projects",
	}

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			p, err := c.CreateProject(args[0])
			if err != nil {
				return err
			}
			cmd.Printf("created %s (namespace %s)\n", p.Name, p.Namespace)
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			projects, err := c.ListProjects()
			if err != nil {
				return err
			}
			for _, p := range projects {
				cmd.Printf("%s\t%s\n", p.Name, p.Namespace)
			}
			return nil
		},
	}

	addMember := &cobra.Command{
		Use:   "add-member <project> <email>",
		Short: "Add a member to a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.AddMember(args[0], args[1]); err != nil {
				return err
			}
			cmd.Printf("added %s to %s\n", args[1], args[0])
			return nil
		},
	}

	rename := &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.RenameProject(args[0], args[1]); err != nil {
				return err
			}
			cmd.Println("renamed. namespace unchanged.")
			return nil
		},
	}

	var rmYes bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a project and everything in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !rmYes {
				cmd.Printf("this will permanently destroy project %q: all its apps, addons, domains and volumes\n", name)
				cmd.Printf("type %q to confirm: ", name)
				var confirm string
				if _, err := fmt.Fscanln(cmd.InOrStdin(), &confirm); err != nil {
					return fmt.Errorf("read confirmation: %w", err)
				}
				if confirm != name {
					return fmt.Errorf("confirmation %q does not match; aborted", confirm)
				}
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeleteProject(name); err != nil {
				return err
			}
			cmd.Printf("deleted %s\n", name)
			return nil
		},
	}
	rm.Flags().BoolVar(&rmYes, "yes", false, "skip the interactive confirmation")

	removeMember := &cobra.Command{
		Use:   "remove-member <project> <email>",
		Short: "Remove a member from a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.RemoveMember(args[0], args[1]); err != nil {
				return err
			}
			cmd.Printf("removed %s from %s\n", args[1], args[0])
			return nil
		},
	}

	gpuQuota := &cobra.Command{
		Use:   "gpu-quota <project> <n>",
		Short: "Cap total GPUs the project's apps may request (0 = unlimited, admin)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid quota %q", args[1])
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetProjectGPUQuota(args[0], n); err != nil {
				return err
			}
			if n == 0 {
				cmd.Printf("gpu quota cleared for %s (unlimited)\n", args[0])
			} else {
				cmd.Printf("gpu quota for %s: %d\n", args[0], n)
			}
			return nil
		},
	}

	var quotaCPU, quotaMemory int64
	var quotaOff bool
	quota := &cobra.Command{
		Use:   "quota <project>",
		Short: "Cap total CPU/memory the project's namespace may use (0 = unlimited, admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !quotaOff && quotaCPU == 0 && quotaMemory == 0 {
				return fmt.Errorf("nothing to set; pass --cpu and/or --memory, or --off to clear")
			}
			cpu, mem := quotaCPU, quotaMemory
			if quotaOff {
				cpu, mem = 0, 0
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetProjectQuota(args[0], cpu, mem); err != nil {
				return err
			}
			if cpu == 0 && mem == 0 {
				cmd.Printf("quota cleared for %s (unlimited)\n", args[0])
			} else {
				cmd.Printf("quota for %s: cpu=%dm memory=%dMi\n", args[0], cpu, mem)
			}
			return nil
		},
	}
	quota.Flags().Int64Var(&quotaCPU, "cpu", 0, "total CPU millicores the namespace may use (0 = unlimited)")
	quota.Flags().Int64Var(&quotaMemory, "memory", 0, "total memory in Mi the namespace may use (0 = unlimited)")
	quota.Flags().BoolVar(&quotaOff, "off", false, "clear the quota (unlimited)")

	cmd.AddCommand(create, list, addMember, rename, rm, removeMember, projectS3Cmd(), gpuQuota, quota)
	return cmd
}
