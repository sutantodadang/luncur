package cli

import (
	"fmt"
	"sort"
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
	var project, image, environment string
	var envs []string
	var noWait bool
	cmd := &cobra.Command{
		Use:   "deploy <app>",
		Short: "Deploy an app from an image or from the current directory's source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pairs, err := parseEnvPairs(envs)
			if err != nil {
				return err
			}

			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(environment)

			// Set env vars before deploying so the container boots with them
			// present — e.g. postgres needs POSTGRES_PASSWORD on first start.
			for _, k := range sortedKeys(pairs) {
				if err := c.EnvSet(project, args[0], k, pairs[k]); err != nil {
					return err
				}
			}

			if image != "" {
				result, err := c.Deploy(project, args[0], image)
				if err != nil {
					return err
				}
				return waitRollout(cmd, c, project, args[0], result, noWait, "deployed")
			}

			return deployFromSource(cmd, c, project, args[0])
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&image, "image", "", "image reference (omit to deploy source from the current directory)")
	cmd.Flags().StringArrayVarP(&envs, "env", "e", nil, "set an env var before deploying (KEY=VALUE, repeatable)")
	// --env is already the env-var flag above; the deployment environment
	// selector is --environment here (it is --env on other commands).
	cmd.Flags().StringVar(&environment, "environment", "", "deployment environment (default: the project's default env)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return once the image is applied instead of waiting for the rollout")
	return cmd
}

// waitRollout follows a gated deploy (status "deploying") until the rollout
// gate marks it live or failed, printing new-pod progress. A failed rollout
// prints the 3-line error (what broke · why · next command).
func waitRollout(cmd *cobra.Command, c *client.Client, project, app string, res client.DeployResult, noWait bool, verb string) error {
	if res.Status != "deploying" || noWait {
		if res.Status == "deploying" {
			cmd.Printf("%s %s: deployment #%d rolling out (not waiting)\n", verb, app, res.Seq)
			return nil
		}
		cmd.Printf("%s %s → %s (deployment #%d)\n", verb, app, res.URL, res.Seq)
		return nil
	}
	cmd.Printf("deployment #%d rolling out…\n", res.Seq)
	deadline := time.Now().Add(deployPollTimeout)
	lastProgress := ""
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for deployment #%d to roll out", res.Seq)
		}
		time.Sleep(deployPollInterval)
		d, err := c.GetDeploy(project, app, res.DeploymentID)
		if err != nil {
			return err
		}
		if d.Want > 0 {
			if p := fmt.Sprintf("ready %d/%d", d.Ready, d.Want); p != lastProgress {
				cmd.Printf("  %s\n", p)
				lastProgress = p
			}
		}
		switch d.Status {
		case "live":
			cmd.Printf("%s %s → %s (deployment #%d)\n", verb, app, d.URL, res.Seq)
			return nil
		case "failed":
			return rolloutFailure(cmd, project, app, res.Seq, d)
		}
	}
}

// rolloutFailure prints a gate failure in the 3-line error contract.
func rolloutFailure(cmd *cobra.Command, project, app string, seq int64, d client.DeployResult) error {
	cmd.Printf("✗ deployment #%d failed: %s\n", seq, d.FailReason)
	if d.Why != "" {
		cmd.Printf("  why:  %s\n", d.Why)
	}
	cmd.Printf("  next: luncur logs %s --project %s\n", app, project)
	return fmt.Errorf("deploy failed")
}

// parseEnvPairs turns []string{"KEY=VALUE"} into a map, erroring on any pair
// missing "=" or with an empty key.
func parseEnvPairs(envs []string) (map[string]string, error) {
	out := make(map[string]string, len(envs))
	for _, e := range envs {
		key, value, ok := strings.Cut(e, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid KEY=VALUE pair: %q", e)
		}
		out[key] = value
	}
	return out, nil
}

// sortedKeys returns m's keys sorted, so env is applied deterministically.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// redeployCmd re-rolls an app's current release: a git app rebuilds from its
// repo, any other app re-applies its latest image. It always restarts the
// pods (the deployment's config hash changes), so it doubles as "restart this
// app" — e.g. to pick up an env change or clear bad in-memory state.
func redeployCmd() *cobra.Command {
	var project string
	var noWait bool
	cmd := &cobra.Command{
		Use:   "redeploy <app>",
		Short: "Re-roll an app's current release (rebuild for git apps; re-apply the latest image otherwise)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			result, err := c.Redeploy(project, args[0])
			if err != nil {
				return err
			}
			if result.Status == "building" {
				cmd.Printf("redeploy started for %s (deployment #%d, building)\n", args[0], result.Seq)
				return nil
			}
			return waitRollout(cmd, c, project, args[0], result, noWait, "redeployed")
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return once applied instead of waiting for the rollout")
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
	cmd.Printf("uploaded source (deployment #%d), status: %s\n", res.Seq, res.Status)

	deadline := time.Now().Add(deployPollTimeout)
	lastStatus := res.Status
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("deploy timed out waiting for deployment #%d", res.Seq)
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
			cmd.Printf("deployed %s → %s (deployment #%d)\n", app, d.URL, res.Seq)
			return nil
		case "failed":
			if d.FailReason != "" {
				// The build succeeded; the rollout gate failed the deploy.
				return rolloutFailure(cmd, project, app, res.Seq, d)
			}
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
	var project, env string
	var replicas int
	var cpu, memory string
	var gpu int64
	cmd := &cobra.Command{
		Use:   "scale <app>",
		Short: "Scale an app, or set/clear its per-app CPU and memory limits",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			replicasSet := cmd.Flags().Changed("replicas")
			cpuSet := cmd.Flags().Changed("cpu")
			memorySet := cmd.Flags().Changed("memory")
			gpuSet := cmd.Flags().Changed("gpu")
			if !replicasSet && !cpuSet && !memorySet && !gpuSet {
				return fmt.Errorf("specify at least one of --replicas, --cpu, --memory, --gpu")
			}

			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			var replicasArg *int
			var cpuArg, memoryArg *string
			var gpuArg *int64
			if gpuSet {
				gpuArg = &gpu
			}
			if replicasSet {
				replicasArg = &replicas
			}
			if cpuSet {
				cpuArg = &cpu
			}
			if memorySet {
				memoryArg = &memory
			}
			if err := c.Scale(project, args[0], replicasArg, cpuArg, memoryArg, gpuArg); err != nil {
				return err
			}

			var parts []string
			if replicasSet {
				parts = append(parts, fmt.Sprintf("replicas=%d", replicas))
			}
			if cpuSet {
				parts = append(parts, "cpu="+cpu)
			}
			if memorySet {
				parts = append(parts, "memory="+memory)
			}
			if gpuSet {
				parts = append(parts, fmt.Sprintf("gpu=%d", gpu))
			}
			cmd.Printf("scaled %s: %s\n", args[0], strings.Join(parts, " "))
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "number of replicas")
	cmd.Flags().StringVar(&cpu, "cpu", "", "CPU request+limit (e.g. 250m, 1); empty string clears")
	cmd.Flags().StringVar(&memory, "memory", "", "memory request+limit (e.g. 256Mi, 1Gi); empty string clears")
	cmd.Flags().Int64Var(&gpu, "gpu", 0, "number of nvidia.com/gpu devices; 0 clears")
	return cmd
}

func destroyCmd() *cobra.Command {
	var project, env string
	cmd := &cobra.Command{
		Use:   "destroy <app>",
		Short: "Destroy an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			c.SetEnv(env)
			if err := c.DeleteApp(project, args[0]); err != nil {
				return err
			}
			cmd.Printf("destroyed %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&env, "env", "", "environment (default: the project's default env)")
	return cmd
}
