package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/client"
)

// dumpFromPath resolves <path> into a restore dump: either the raw file
// itself (a .pgdump/.rdb produced directly, e.g. by hand), or — when it's a
// luncur backup archive (.tar.gz) — that archive's addons/*-<name>.{pgdump,rdb}
// member, extracted into memory. Detection is by content, not extension:
// gzip.NewReader succeeding plus at least one addons/ member means archive.
func dumpFromPath(path, name string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		// Not gzip at all — stream the raw file as-is.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	defer f.Close()

	tr := tar.NewReader(gz)
	pgSuffix := "-" + name + ".pgdump"
	rdbSuffix := "-" + name + ".rdb"
	sawAddons := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if !strings.HasPrefix(hdr.Name, "addons/") {
			continue
		}
		sawAddons = true
		base := strings.TrimPrefix(hdr.Name, "addons/")
		if strings.HasSuffix(base, pgSuffix) || strings.HasSuffix(base, rdbSuffix) {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(bytes.NewReader(b)), nil
		}
	}
	if sawAddons {
		return nil, fmt.Errorf("no addon dump for %q found in archive %s", name, path)
	}
	// Gzip, but not a luncur backup archive (no addons/ members at all) —
	// fall back to streaming the raw file.
	raw, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func addonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addon",
		Short: "Manage project addons (postgres|redis|minio|mlflow)",
	}

	var createProject, createName, createVersion string
	var createSize int
	create := &cobra.Command{
		Use:   "create <type>",
		Short: "Provision a new addon (postgres|redis|minio|mlflow)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			a, err := c.CreateAddon(createProject, client.AddonCreate{
				Type: args[0], Name: createName, Version: createVersion, SizeGB: createSize,
			})
			if err != nil {
				return err
			}
			cmd.Printf("created %s (%s)\n", a.Name, a.Type)
			return nil
		},
	}
	create.Flags().StringVar(&createProject, "project", "", "project name")
	create.MarkFlagRequired("project")
	create.Flags().StringVar(&createName, "name", "", "addon name (default <type><n>)")
	create.Flags().StringVar(&createVersion, "version", "", "addon version (default postgres:16, redis:7, pinned minio/mlflow)")
	create.Flags().IntVar(&createSize, "size", 1, "volume size in GB")

	var addProject, addApp, addName, addVersion string
	var addSize int
	add := &cobra.Command{
		Use:   "add <type>",
		Short: "Provision a new addon and attach it to an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			a, err := c.CreateAddon(addProject, client.AddonCreate{
				Type: args[0], Name: addName, Version: addVersion, SizeGB: addSize, App: addApp,
			})
			if err != nil {
				return err
			}
			cmd.Printf("created %s (%s), attached to %s\n", a.Name, a.Type, addApp)
			return nil
		},
	}
	add.Flags().StringVar(&addProject, "project", "", "project name")
	add.MarkFlagRequired("project")
	add.Flags().StringVar(&addApp, "app", "", "app to attach the addon to")
	add.MarkFlagRequired("app")
	add.Flags().StringVar(&addName, "name", "", "addon name (default <type><n>)")
	add.Flags().StringVar(&addVersion, "version", "", "addon version (default postgres:16, redis:7, pinned minio/mlflow)")
	add.Flags().IntVar(&addSize, "size", 1, "volume size in GB")

	var attachProject string
	attach := &cobra.Command{
		Use:   "attach <name> <app>",
		Short: "Attach an existing addon to an app",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			warning, err := c.AttachAddon(attachProject, args[0], args[1])
			if err != nil {
				return err
			}
			if warning != "" {
				cmd.Println(warning)
			}
			return nil
		},
	}
	attach.Flags().StringVar(&attachProject, "project", "", "project name")
	attach.MarkFlagRequired("project")

	var detachProject string
	detach := &cobra.Command{
		Use:   "detach <name> <app>",
		Short: "Detach an addon from an app",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.DetachAddon(detachProject, args[0], args[1])
		},
	}
	detach.Flags().StringVar(&detachProject, "project", "", "project name")
	detach.MarkFlagRequired("project")

	var listProject string
	list := &cobra.Command{
		Use:   "list",
		Short: "List a project's addons",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			addons, err := c.ListAddons(listProject)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTYPE\tVERSION\tREADY\tATTACHED")
			for _, a := range addons {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", a.Name, a.Type, a.Version, a.Ready, strings.Join(a.AttachedTo, ","))
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&listProject, "project", "", "project name")
	list.MarkFlagRequired("project")

	var removeProject string
	var removeForce, removeKeepData bool
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete an addon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			return c.RemoveAddon(removeProject, args[0], removeForce, removeKeepData)
		},
	}
	remove.Flags().StringVar(&removeProject, "project", "", "project name")
	remove.MarkFlagRequired("project")
	remove.Flags().BoolVar(&removeForce, "force", false, "remove even if attached to apps")
	remove.Flags().BoolVar(&removeKeepData, "keep-data", false, "keep the underlying PVC data")

	var upgradeProject, upgradeVersion string
	upgrade := &cobra.Command{
		Use:   "upgrade <name>",
		Short: "Upgrade an addon in place to a new version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			a, err := c.UpgradeAddon(upgradeProject, args[0], upgradeVersion)
			if err != nil {
				return err
			}
			cmd.Printf("upgraded %s to %s\n", a.Name, a.Version)
			if a.Warning != "" {
				cmd.Printf("warning: %s\n", a.Warning)
			}
			return nil
		},
	}
	upgrade.Flags().StringVar(&upgradeProject, "project", "", "project name")
	upgrade.MarkFlagRequired("project")
	upgrade.Flags().StringVar(&upgradeVersion, "version", "", "target version (image tag)")
	upgrade.MarkFlagRequired("version")

	var urlProject string
	urlCmd := &cobra.Command{
		Use:   "url <name>",
		Short: "Show an addon's connection URL and the env key it's injected as",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			envKey, connURL, err := c.AddonURL(urlProject, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("%s=%s\n", envKey, connURL)
			return nil
		},
	}
	urlCmd.Flags().StringVar(&urlProject, "project", "", "project name")
	urlCmd.MarkFlagRequired("project")

	var restoreProject, restoreFile string
	restore := &cobra.Command{
		Use:   "restore <name>",
		Short: "Restore a postgres/redis addon's data from a dump or backup archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			dump, err := dumpFromPath(restoreFile, args[0])
			if err != nil {
				return err
			}
			defer dump.Close()
			if err := c.RestoreAddon(restoreProject, args[0], dump); err != nil {
				return err
			}
			cmd.Printf("restored %s\n", args[0])
			return nil
		},
	}
	restore.Flags().StringVar(&restoreProject, "project", "", "project name")
	restore.MarkFlagRequired("project")
	restore.Flags().StringVar(&restoreFile, "file", "", "path to a raw dump (.pgdump/.rdb) or a luncur backup archive (.tar.gz)")
	restore.MarkFlagRequired("file")

	cmd.AddCommand(create, add, attach, detach, list, remove, upgrade, urlCmd, restore)
	return cmd
}
