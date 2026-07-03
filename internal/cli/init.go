package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// initCmd writes a minimal luncur.toml describing the app in the current
// directory. It makes no network calls.
func initCmd() *cobra.Command {
	var appName, project string
	var port int
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a luncur.toml for the current directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Create("luncur.toml")
			if err != nil {
				return err
			}
			defer f.Close()
			fmt.Fprintf(f, "app = %q\n", appName)
			fmt.Fprintf(f, "project = %q\n", project)
			fmt.Fprintf(f, "port = %d\n", port)
			cmd.Println("wrote luncur.toml")
			cmd.Printf("tip: deploy with git push — git remote add luncur ssh://git@<server-ip>:30022/%s/%s.git\n", project, appName)
			return nil
		},
	}

	defaultApp := "app"
	if wd, err := os.Getwd(); err == nil {
		defaultApp = filepath.Base(wd)
	}

	cmd.Flags().StringVar(&appName, "app", defaultApp, "app name")
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().IntVar(&port, "port", 8080, "container port")
	return cmd
}
