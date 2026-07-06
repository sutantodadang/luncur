package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// latestReleaseVersionURL is GitHub's "latest release" API endpoint for
// this repo — used when `luncur update` is run with no version/image.
const latestReleaseVersionURL = "https://api.github.com/repos/sutantodadang/luncur/releases/latest"

// latestReleaseTag asks the GitHub API for the newest release tag.
func latestReleaseTag(c *http.Client, apiURL string) (string, error) {
	resp, err := c.Get(apiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("github returned %s", resp.Status)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.TagName == "" {
		return "", fmt.Errorf("no tag_name in latest release response")
	}
	return out.TagName, nil
}

func updateCmd() *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:   "update [version]",
		Short: "Roll the luncur server itself to a new image, in-cluster (no fresh reinstall)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}

			var version string
			if len(args) == 1 {
				version = args[0]
			}
			if version == "" && image == "" {
				httpClient := &http.Client{Timeout: 10 * time.Second}
				version, err = latestReleaseTag(httpClient, latestReleaseVersionURL)
				if err != nil {
					return fmt.Errorf("resolve latest release: %w", err)
				}
			}

			img, err := c.SystemUpdate(version, image)
			if err != nil {
				return err
			}
			cmd.Printf("updating server to %s — pods will roll; run 'luncur doctor' to verify\n", img)
			return nil
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "explicit image ref, overrides version resolution")
	return cmd
}
