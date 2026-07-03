package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath() (string, error) {
	if p := os.Getenv("LUNCUR_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "luncur", "config.json"), nil
}

func loadConfig() (Config, error) {
	var c Config
	p, err := configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func saveConfig(c Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// configCmd manages install-level server settings (admin), distinct from the
// local CLI config file above.
func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read or change install settings (admin)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "Read a setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			v, err := c.GetSetting(args[0])
			if err != nil {
				return err
			}
			cmd.Println(v)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change a setting",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.SetSetting(args[0], args[1])
		},
	})
	return cmd
}
