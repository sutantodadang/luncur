package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func splitProjectApp(s string) (string, string, error) {
	p, a, ok := strings.Cut(s, "/")
	if !ok || p == "" || a == "" {
		return "", "", fmt.Errorf("expected <project>/<app>, got %q", s)
	}
	return p, a, nil
}

// parseForwardPorts resolves "" | "local" | "local:remote" against the
// app's configured port.
func parseForwardPorts(arg string, appPort int) (local, remote int, err error) {
	local, remote = appPort, appPort
	if arg == "" {
		return local, remote, nil
	}
	ls, rs, has := strings.Cut(arg, ":")
	if local, err = strconv.Atoi(ls); err != nil || local < 1 || local > 65535 {
		return 0, 0, fmt.Errorf("bad local port %q", ls)
	}
	if has {
		if remote, err = strconv.Atoi(rs); err != nil || remote < 1 || remote > 65535 {
			return 0, 0, fmt.Errorf("bad remote port %q", rs)
		}
	} else {
		remote = appPort
	}
	return local, remote, nil
}

func forwardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forward <project>/<app> [local[:remote]]",
		Short: "Forward a local port to an app's in-cluster service",
		Long: "Listens on 127.0.0.1 and tunnels each connection through the " +
			"luncur server to the app's ClusterIP Service. Works for internal " +
			"apps with no public URL. Ctrl-C to stop.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, app, err := splitProjectApp(args[0])
			if err != nil {
				return err
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			info, err := c.GetApp(project, app)
			if err != nil {
				return err
			}
			portArg := ""
			if len(args) == 2 {
				portArg = args[1]
			}
			local, remote, err := parseForwardPorts(portArg, info.Port)
			if err != nil {
				return err
			}

			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", local))
			if err != nil {
				return fmt.Errorf("listen 127.0.0.1:%d: %w (pass an explicit local port, e.g. %d:%d)", local, err, local+1, remote)
			}
			defer ln.Close()
			fmt.Fprintf(cmd.OutOrStdout(), "Forwarding 127.0.0.1:%d -> %s/%s:%d (Ctrl-C to stop)\n", local, project, app, remote)

			// root.go's Execute() doesn't wire a cancellable context (no
			// ExecuteContext + signal.NotifyContext), so this long-running
			// command sets up its own Ctrl-C handling.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			go func() { <-ctx.Done(); ln.Close() }()

			for {
				conn, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				go func(local net.Conn) {
					defer local.Close()
					remoteConn, err := c.Forward(project, app, remote)
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "forward: %v\n", err)
						return
					}
					defer remoteConn.Close()
					done := make(chan struct{}, 2)
					go func() { _, _ = io.Copy(remoteConn, local); done <- struct{}{} }()
					go func() { _, _ = io.Copy(local, remoteConn); done <- struct{}{} }()
					<-done
				}(conn)
			}
		},
	}
}
