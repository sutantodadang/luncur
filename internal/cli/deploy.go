package cli

import "github.com/spf13/cobra"

func deployCmd() *cobra.Command {
	var project, image string
	cmd := &cobra.Command{
		Use:   "deploy <app>",
		Short: "Deploy an image to an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			result, err := c.Deploy(project, args[0], image)
			if err != nil {
				return err
			}
			cmd.Printf("deployed %s → %s (deployment %d)\n", args[0], result.URL, result.DeploymentID)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().StringVar(&image, "image", "", "image reference")
	cmd.MarkFlagRequired("image")
	return cmd
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
