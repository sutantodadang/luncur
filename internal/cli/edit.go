package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/render"
)

func editCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "edit <app> <kind>",
		Short: "Edit an app's rendered manifest and save the diff as an override",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, kind := args[0], args[1]

			editor := os.Getenv("EDITOR")
			if editor == "" {
				return fmt.Errorf("$EDITOR not set")
			}

			c, err := apiClient()
			if err != nil {
				return err
			}

			baseRaw, err := c.Raw(project, app, true)
			if err != nil {
				return err
			}
			currentRaw, err := c.Raw(project, app, false)
			if err != nil {
				return err
			}

			baseDoc, err := render.ExtractDoc(baseRaw, kind)
			if err != nil {
				return err
			}
			currentDoc, err := render.ExtractDoc(currentRaw, kind)
			if err != nil {
				return err
			}

			tmp, err := os.CreateTemp("", "luncur-edit-*.yaml")
			if err != nil {
				return err
			}
			path := tmp.Name()
			defer os.Remove(path)
			if _, err := tmp.Write(currentDoc); err != nil {
				tmp.Close()
				return err
			}
			if err := tmp.Close(); err != nil {
				return err
			}

			ec := exec.Command(editor, path)
			ec.Stdin = os.Stdin
			ec.Stdout = os.Stdout
			ec.Stderr = os.Stderr
			if err := ec.Run(); err != nil {
				return fmt.Errorf("run editor: %w", err)
			}

			editedDoc, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			patch, err := render.ComputeOverride(kind, baseDoc, editedDoc)
			if err != nil {
				return err
			}
			if patch == "{}" {
				cmd.Println("no changes")
				return nil
			}

			if err := c.PutOverride(project, app, kind, patch); err != nil {
				return err
			}
			cmd.Println("override saved; takes effect on next deploy (or immediately if app is live)")
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
