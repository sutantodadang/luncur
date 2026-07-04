package cli

import (
	"github.com/spf13/cobra"
)

func webhookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Manage a git app's auto-deploy webhook",
	}

	var enableProject string
	enable := &cobra.Command{
		Use:   "enable <app>",
		Short: "Enable (or rotate) the app's deploy webhook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			w, err := c.WebhookEnable(enableProject, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("webhook URL: %s%s\n", cfg.Server, w.Path)
			cmd.Printf("secret: %s\n", w.Secret)
			cmd.Println("shown once — store it now")
			cmd.Println("GitHub/Gitea: paste the secret into the webhook's \"Secret\" field (HMAC)")
			cmd.Println("GitLab: paste the secret into the webhook's \"Secret token\" field")
			return nil
		},
	}
	enable.Flags().StringVar(&enableProject, "project", "", "project name")
	enable.MarkFlagRequired("project")

	var showProject string
	show := &cobra.Command{
		Use:   "show <app>",
		Short: "Show the app's webhook status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			w, err := c.WebhookShow(showProject, args[0])
			if err != nil {
				return err
			}
			if !w.Enabled {
				cmd.Println("webhook: disabled")
				return nil
			}
			cmd.Println("webhook: enabled")
			cmd.Printf("URL: %s%s\n", cfg.Server, w.Path)
			cmd.Println("secret: (set)")
			return nil
		},
	}
	show.Flags().StringVar(&showProject, "project", "", "project name")
	show.MarkFlagRequired("project")

	var disableProject string
	disable := &cobra.Command{
		Use:   "disable <app>",
		Short: "Disable the app's deploy webhook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.WebhookDisable(disableProject, args[0]); err != nil {
				return err
			}
			cmd.Println("webhook: disabled")
			return nil
		},
	}
	disable.Flags().StringVar(&disableProject, "project", "", "project name")
	disable.MarkFlagRequired("project")

	cmd.AddCommand(enable, show, disable)
	return cmd
}
