package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// deployPollInterval and deployPollTimeout govern the source-deploy status
// poll loop. Vars (not consts) so tests could shrink them if ever needed.
var (
	deployPollInterval = 2 * time.Second
	deployPollTimeout  = 15 * time.Minute
)

func deployCmd() *cobra.Command {
	var project, image string
	cmd := &cobra.Command{
		Use:   "deploy <app>",
		Short: "Deploy an app from an image or from the current directory's source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}

			if image != "" {
				result, err := c.Deploy(project, args[0], image)
				if err != nil {
					return err
				}
				cmd.Printf("deployed %s → %s (deployment %d)\n", args[0], result.URL, result.DeploymentID)
				return nil
			}

			return deployFromSource(cmd, c, project, args[0])
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&image, "image", "", "image reference (omit to deploy source from the current directory)")
	return cmd
}

// deployFromSource packs the current directory, uploads it, and polls the
// resulting build/deploy until it reaches a terminal state.
func deployFromSource(cmd *cobra.Command, c *client.Client, project, app string) error {
	r, err := packSource(".")
	if err != nil {
		return fmt.Errorf("pack source: %w", err)
	}
	res, err := c.DeploySource(project, app, r)
	if err != nil {
		return err
	}
	cmd.Printf("uploaded source (deployment %d), status: %s\n", res.DeploymentID, res.Status)

	deadline := time.Now().Add(deployPollTimeout)
	lastStatus := res.Status
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("deploy timed out waiting for deployment %d", res.DeploymentID)
		}
		time.Sleep(deployPollInterval)

		d, err := c.GetDeploy(project, app, res.DeploymentID)
		if err != nil {
			return err
		}
		if d.Status != lastStatus {
			cmd.Printf("status: %s\n", d.Status)
			lastStatus = d.Status
		}

		switch d.Status {
		case "live":
			cmd.Printf("deployed %s → %s (deployment %d)\n", app, d.URL, res.DeploymentID)
			return nil
		case "failed":
			logs, logErr := c.DeployLogs(project, app, res.DeploymentID)
			if logErr == nil {
				cmd.Print(tailLines(string(logs), 40))
			}
			return fmt.Errorf("deploy failed")
		}
	}
}

// tailLines returns the last n lines of s (or all of s if it has fewer).
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func scaleCmd() *cobra.Command {
	var project string
	var replicas int
	cmd := &cobra.Command{
		Use:   "scale <app>",
		Short: "Scale an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.Scale(project, args[0], replicas); err != nil {
				return err
			}
			cmd.Printf("scaled %s to %d replicas\n", args[0], replicas)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "number of replicas")
	cmd.MarkFlagRequired("replicas")
	return cmd
}

func destroyCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "destroy <app>",
		Short: "Destroy an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeleteApp(project, args[0]); err != nil {
				return err
			}
			cmd.Printf("destroyed %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
