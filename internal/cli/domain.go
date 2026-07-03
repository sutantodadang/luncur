package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func domainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domain",
		Short: "Manage custom domains for an app",
	}

	var addProject string
	add := &cobra.Command{
		Use:   "add <app> <hostname>",
		Short: "Attach a custom domain to an app",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			d, err := c.AddDomain(addProject, args[0], args[1])
			if err != nil {
				return err
			}
			cmd.Printf("added %s\n", d.Hostname)
			if d.DNSWarning != "" {
				cmd.Println(d.DNSWarning)
			}
			return nil
		},
	}
	add.Flags().StringVar(&addProject, "project", "", "project name")
	add.MarkFlagRequired("project")

	var listProject string
	list := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's custom domains",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			domains, err := c.ListDomains(listProject, args[0])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "HOSTNAME\tCERT\tEXPIRES\tERROR")
			for _, d := range domains {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Hostname, d.CertStatus, d.CertExpiresAt, d.CertError)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&listProject, "project", "", "project name")
	list.MarkFlagRequired("project")

	var removeProject string
	remove := &cobra.Command{
		Use:   "remove <app> <hostname>",
		Short: "Detach a custom domain from an app",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.DeleteDomain(removeProject, args[0], args[1])
		},
	}
	remove.Flags().StringVar(&removeProject, "project", "", "project name")
	remove.MarkFlagRequired("project")

	var retryProject string
	retry := &cobra.Command{
		Use:   "retry <app> <hostname>",
		Short: "Retry certificate issuance for a domain",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.RetryDomain(retryProject, args[0], args[1])
		},
	}
	retry.Flags().StringVar(&retryProject, "project", "", "project name")
	retry.MarkFlagRequired("project")

	cmd.AddCommand(add, list, remove, retry)
	return cmd
}
