package cli

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// pushHookCmd is the hidden post-receive hook entrypoint. git runs it inside
// the throwaway push repo; it relays stdin (ref updates) to the gitssh
// server over the unix socket in $LUNCUR_PUSH_SOCK, then streams progress
// lines back to its own stderr (which git shows the pusher as "remote: ").
// Exit code comes from the final "__luncur_exit__ N" line.
func pushHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_push-hook",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sock := os.Getenv("LUNCUR_PUSH_SOCK")
			if sock == "" {
				return fmt.Errorf("_push-hook: LUNCUR_PUSH_SOCK not set")
			}
			conn, err := net.Dial("unix", sock)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := io.Copy(conn, os.Stdin); err != nil {
				return err
			}
			fmt.Fprintln(conn) // blank line = end of refs
			rd := bufio.NewReader(conn)
			for {
				line, err := rd.ReadString('\n')
				if trimmed := strings.TrimRight(line, "\n"); trimmed != "" {
					if code, ok := strings.CutPrefix(trimmed, "__luncur_exit__ "); ok {
						if code != "0" {
							os.Exit(1)
						}
						return nil
					}
					fmt.Fprintln(os.Stderr, trimmed)
				}
				if err != nil {
					return nil
				}
			}
		},
	}
}
