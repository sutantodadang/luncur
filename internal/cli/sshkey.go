package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// defaultPubKeyPath returns the first standard public key found in ~/.ssh.
func defaultPubKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, name := range []string{"id_ed25519.pub", "id_rsa.pub", "id_ecdsa.pub"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no public key found in ~/.ssh (looked for id_ed25519.pub, id_rsa.pub, id_ecdsa.pub); pass a path explicitly")
}

func sshKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh-key",
		Short: "Manage SSH public keys for git push",
	}

	var name string
	add := &cobra.Command{
		Use:   "add [path-to-public-key]",
		Short: "Register a public key",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			path := ""
			if len(args) == 1 {
				path = args[0]
			} else if path, err = defaultPubKeyPath(); err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if name == "" {
				if h, err := os.Hostname(); err == nil {
					name = h
				} else {
					name = "key"
				}
			}
			fp, err := c.AddSSHKey(name, string(b))
			if err != nil {
				return err
			}
			cmd.Printf("added %s (%s)\n", name, fp)
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "", "key name (default: hostname)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List registered keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			keys, err := c.ListSSHKeys()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tFINGERPRINT\tADDED")
			for _, k := range keys {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", k.ID, k.Name, k.Fingerprint, k.CreatedAt)
			}
			return tw.Flush()
		},
	}

	remove := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid id %q", args[0])
			}
			return c.DeleteSSHKey(id)
		},
	}

	cmd.AddCommand(add, list, remove)
	return cmd
}
