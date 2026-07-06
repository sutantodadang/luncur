package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sutantodadang/luncur/internal/client"
)

func loginCmd() *cobra.Command {
	var email, password string
	cmd := &cobra.Command{
		Use:   "login <server-url>",
		Short: "Authenticate against a luncur server and store the token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL := args[0]
			if email == "" {
				fmt.Fprint(cmd.OutOrStdout(), "email: ")
				r := bufio.NewReader(cmd.InOrStdin())
				line, err := r.ReadString('\n')
				if err != nil {
					return err
				}
				email = strings.TrimSpace(line)
			}
			if password == "" {
				var err error
				password, err = promptPassword(cmd, "password: ")
				if err != nil {
					return err
				}
			}
			tok, err := client.New(serverURL, "").Login(email, password)
			if err != nil {
				return err
			}
			if err := saveConfig(Config{Server: serverURL, Token: tok}); err != nil {
				return err
			}
			cmd.Printf("logged in to %s as %s\n", serverURL, email)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email (prompted if omitted)")
	cmd.Flags().StringVar(&password, "password", "", "password (prompted if omitted)")
	return cmd
}

// apiClient loads the saved config and returns a ready client.
func apiClient() (*client.Client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("not logged in — run `luncur login <server-url>` first")
	}
	return client.New(cfg.Server, cfg.Token), nil
}

// promptPassword prints label and reads a password from the terminal without
// echoing it — never logged, never returned in any error message.
func promptPassword(cmd *cobra.Command, label string) (string, error) {
	fmt.Fprint(cmd.OutOrStdout(), label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.OutOrStdout())
	if err != nil {
		return "", err
	}
	return string(b), nil
}
