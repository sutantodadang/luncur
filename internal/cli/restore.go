package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/s3"
	"github.com/sutantodadang/luncur/internal/store"
)

// restoreArchive is the testable core of `luncur restore`: validate the
// archive's manifest, run the bootstrap guard against an existing
// non-empty DB, take a pre-restore copy under force, and extract
// luncur.db (+ luncur.key) into dataDir. Returns the archive's addons/*
// member names for the guided-restore printout. The archive is read fully
// before anything in dataDir is touched.
func restoreArchive(archivePath, dataDir string, force bool, now func() time.Time) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	tr := tar.NewReader(gz)

	var dbBytes, keyBytes, manifestBytes []byte
	var addonMembers []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		switch {
		case hdr.Name == "luncur.db":
			if dbBytes, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		case hdr.Name == "luncur.key":
			if keyBytes, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		case hdr.Name == "manifest.json":
			if manifestBytes, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		case strings.HasPrefix(hdr.Name, "addons/"):
			addonMembers = append(addonMembers, hdr.Name)
		}
	}

	if manifestBytes == nil {
		return nil, fmt.Errorf("archive has no manifest.json — not a luncur backup")
	}
	var manifest struct {
		CreatedAt string   `json:"created_at"`
		Members   []string `json:"members"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("corrupt manifest.json: %w", err)
	}
	if dbBytes == nil {
		return nil, fmt.Errorf("archive has no luncur.db member")
	}

	// Bootstrap guard: never silently overwrite a live install.
	dbPath := filepath.Join(dataDir, "luncur.db")
	keyPath := filepath.Join(dataDir, "luncur.key")
	if _, err := os.Stat(dbPath); err == nil {
		st, err := store.Open(dbPath)
		if err != nil {
			return nil, fmt.Errorf("open existing %s: %w", dbPath, err)
		}
		projects, err := st.ListProjects()
		_ = st.Close()
		if err != nil {
			return nil, err
		}
		if len(projects) > 0 && !force {
			return nil, fmt.Errorf(
				"%s already has %d project(s); refusing to overwrite — re-run with --force to replace it (a pre-restore copy will be kept)",
				dbPath, len(projects))
		}
		if force {
			preDir := filepath.Join(dataDir, "pre-restore-"+now().UTC().Format("20060102-150405"))
			if err := os.MkdirAll(preDir, 0o700); err != nil {
				return nil, err
			}
			if err := copyFile(dbPath, filepath.Join(preDir, "luncur.db")); err != nil {
				return nil, fmt.Errorf("pre-restore copy: %w", err)
			}
			if _, err := os.Stat(keyPath); err == nil {
				if err := copyFile(keyPath, filepath.Join(preDir, "luncur.key")); err != nil {
					return nil, fmt.Errorf("pre-restore copy: %w", err)
				}
			}
		}
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dbPath, dbBytes, 0o600); err != nil {
		return nil, err
	}
	if keyBytes != nil {
		if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
			return nil, err
		}
	}

	sort.Strings(addonMembers)
	return addonMembers, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// restoreCmd is `luncur restore`: a host command like `luncur up` — the
// server must be scaled down first (a running server holds luncur.db open).
func restoreCmd() *cobra.Command {
	var dataDir string
	var force bool
	var s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey string
	cmd := &cobra.Command{
		Use:   "restore <archive-path-or-s3-key>",
		Short: "Restore luncur.db and luncur.key from a backup archive (host command)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := args[0]

			if s3Endpoint != "" {
				cl := &s3.Client{
					Endpoint: s3Endpoint, Bucket: s3Bucket,
					AccessKey: s3AccessKey, SecretKey: s3SecretKey,
				}
				body, err := cl.Get(context.Background(), src)
				if err != nil {
					return fmt.Errorf("download %s: %w", src, err)
				}
				defer body.Close()
				tmp, err := os.CreateTemp("", "luncur-restore-*.tar.gz")
				if err != nil {
					return err
				}
				defer os.Remove(tmp.Name())
				if _, err := io.Copy(tmp, body); err != nil {
					tmp.Close()
					return fmt.Errorf("download %s: %w", src, err)
				}
				if err := tmp.Close(); err != nil {
					return err
				}
				cmd.Printf("downloaded s3://%s/%s\n", s3Bucket, src)
				src = tmp.Name()
			}

			addons, err := restoreArchive(src, dataDir, force, time.Now)
			if err != nil {
				return err
			}

			cmd.Printf("restored luncur.db and luncur.key into %s\n\n", dataDir)
			cmd.Println("next steps:")
			cmd.Println("  1. start (or restart) the server:")
			cmd.Println("     kubectl -n luncur-system scale deploy/luncur --replicas=0   # if it was running against this data dir")
			cmd.Println("     kubectl -n luncur-system scale deploy/luncur --replicas=1")
			if len(addons) > 0 {
				cmd.Println("  2. re-create each addon (luncur addon create ... with the same names), then restore its data:")
				for _, m := range addons {
					base := strings.TrimPrefix(m, "addons/")
					name := strings.TrimSuffix(strings.TrimSuffix(base, ".pgdump"), ".rdb")
					switch {
					case strings.HasSuffix(m, ".pgdump"):
						cmd.Printf("     # %s (postgres)\n", name)
						cmd.Printf("     kubectl -n <project-ns> exec -i addon-<name>-0 -- sh -c 'PGPASSWORD=\"$POSTGRES_PASSWORD\" pg_restore -U \"$POSTGRES_USER\" -d \"$POSTGRES_DB\" --clean' < %s\n", base)
					case strings.HasSuffix(m, ".rdb"):
						cmd.Printf("     # %s (redis)\n", name)
						cmd.Printf("     # scale the addon StatefulSet to 0, copy %s onto its PVC as dump.rdb, scale back to 1\n", base)
					}
				}
				cmd.Println("     (extract the addon dump files from the archive: tar -xzf <archive> addons/)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "./data", "luncur data directory to restore into")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a non-empty existing install (keeps a pre-restore copy)")
	cmd.Flags().StringVar(&s3Endpoint, "s3-endpoint", "", "treat <source> as an S3 key and download it from this endpoint")
	cmd.Flags().StringVar(&s3Bucket, "s3-bucket", "", "S3 bucket (with --s3-endpoint)")
	cmd.Flags().StringVar(&s3AccessKey, "s3-access-key", "", "S3 access key (with --s3-endpoint)")
	cmd.Flags().StringVar(&s3SecretKey, "s3-secret-key", "", "S3 secret key (with --s3-endpoint)")
	return cmd
}
