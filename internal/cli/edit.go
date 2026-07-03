package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

// dataStructFor returns the typed zero value strategicpatch needs to
// understand list-merge keys. Duplicated from internal/render on purpose —
// the brief calls for a local copy rather than exporting the render one.
func dataStructFor(kind string) (any, error) {
	switch kind {
	case "Deployment":
		return appsv1.Deployment{}, nil
	case "Service":
		return corev1.Service{}, nil
	case "Ingress":
		return netv1.Ingress{}, nil
	default:
		return nil, fmt.Errorf("kind %q cannot be overridden", kind)
	}
}

// extractDoc splits a multi-document YAML stream on "---" boundaries and
// returns the document whose top-level `kind:` matches, erroring if none do.
func extractDoc(yamlMulti []byte, kind string) ([]byte, error) {
	docs := bytes.Split(yamlMulti, []byte("\n---"))
	for _, doc := range docs {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			continue
		}
		if meta.Kind == kind {
			return doc, nil
		}
	}
	return nil, fmt.Errorf("no document with kind %q found", kind)
}

// computeOverride diffs baseYAML against editedYAML and returns a strategic
// merge patch JSON string ("{}" if there is no difference). Pure function.
func computeOverride(kind string, baseYAML, editedYAML []byte) (string, error) {
	ds, err := dataStructFor(kind)
	if err != nil {
		return "", err
	}
	baseJSON, err := yaml.YAMLToJSON(baseYAML)
	if err != nil {
		return "", fmt.Errorf("base yaml: %w", err)
	}
	editedJSON, err := yaml.YAMLToJSON(editedYAML)
	if err != nil {
		return "", fmt.Errorf("edited yaml: %w", err)
	}
	patch, err := strategicpatch.CreateTwoWayMergePatch(baseJSON, editedJSON, ds)
	if err != nil {
		return "", fmt.Errorf("compute patch: %w", err)
	}
	return string(patch), nil
}

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

			baseDoc, err := extractDoc(baseRaw, kind)
			if err != nil {
				return err
			}
			currentDoc, err := extractDoc(currentRaw, kind)
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

			patch, err := computeOverride(kind, baseDoc, editedDoc)
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
