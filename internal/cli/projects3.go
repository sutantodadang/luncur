package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// projectS3Cmd manages a project's external S3 configuration (the
// alternative to an in-cluster minio addon).
func projectS3Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "s3",
		Short: "Manage the project's external S3 storage",
	}

	var project, endpoint, region, bucket, accessKey, secretKey string
	set := &cobra.Command{
		Use:   "set",
		Short: "Store external S3 credentials for the project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetProjectS3(project, client.S3Config{
				Endpoint: endpoint, Region: region, Bucket: bucket,
				AccessKey: accessKey, SecretKey: secretKey,
			}); err != nil {
				return err
			}
			cmd.Printf("s3 configured for %s: %s (bucket %s)\n", project, endpoint, bucket)
			cmd.Println("opt apps in with: luncur app s3env <app> --project " + project + " --enable")
			return nil
		},
	}
	set.Flags().StringVar(&project, "project", "", "project name")
	set.MarkFlagRequired("project")
	set.Flags().StringVar(&endpoint, "endpoint", "", "S3 endpoint URL (e.g. https://s3.us-east-1.amazonaws.com)")
	set.MarkFlagRequired("endpoint")
	set.Flags().StringVar(&region, "region", "", "S3 region (default us-east-1 at request time)")
	set.Flags().StringVar(&bucket, "bucket", "", "bucket name")
	set.MarkFlagRequired("bucket")
	set.Flags().StringVar(&accessKey, "access-key", "", "access key id")
	set.MarkFlagRequired("access-key")
	set.Flags().StringVar(&secretKey, "secret-key", "", "secret access key")
	set.MarkFlagRequired("secret-key")

	var showProject string
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the project's external S3 configuration (no secret)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			cfg, err := c.GetProjectS3(showProject)
			if err != nil {
				return err
			}
			cmd.Printf("endpoint=%s region=%s bucket=%s access_key=%s\n", cfg.Endpoint, cfg.Region, cfg.Bucket, cfg.AccessKey)
			return nil
		},
	}
	show.Flags().StringVar(&showProject, "project", "", "project name")
	show.MarkFlagRequired("project")

	var clearProject string
	clear := &cobra.Command{
		Use:   "clear",
		Short: "Remove the project's external S3 configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeleteProjectS3(clearProject); err != nil {
				return err
			}
			cmd.Println("s3 configuration removed")
			return nil
		},
	}
	clear.Flags().StringVar(&clearProject, "project", "", "project name")
	clear.MarkFlagRequired("project")

	cmd.AddCommand(set, show, clear)
	return cmd
}

// appS3EnvCmd toggles LUNCUR_S3_* injection for one app.
func appS3EnvCmd() *cobra.Command {
	var project string
	var enable, disable bool
	cmd := &cobra.Command{
		Use:   "s3env <app>",
		Short: "Enable or disable LUNCUR_S3_* env injection for an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if enable == disable {
				return fmt.Errorf("pass exactly one of --enable or --disable")
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.SetAppS3Env(project, args[0], enable); err != nil {
				return err
			}
			state := "disabled"
			if enable {
				state = "enabled"
			}
			cmd.Printf("s3 env %s for %s\n", state, args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	cmd.Flags().BoolVar(&enable, "enable", false, "turn injection on")
	cmd.Flags().BoolVar(&disable, "disable", false, "turn injection off")
	return cmd
}
