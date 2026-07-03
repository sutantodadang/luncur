package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

// bootstrapAdmin creates the initial admin from "email:password" iff the
// users table is empty. Idempotent so `luncur serve` restarts are safe.
func bootstrapAdmin(st *store.Store, spec string) error {
	email, password, ok := strings.Cut(spec, ":")
	if !ok || email == "" || password == "" {
		return fmt.Errorf("--bootstrap-admin must be email:password")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := st.CreateUser(email, password, "admin")
	return err
}

func serveCmd() *cobra.Command {
	var dbPath, listen, bootstrap, kubeconfig, secretKeyFile, externalIP string
	var dataDir, builderImage, registryHost string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the luncur API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			if bootstrap != "" {
				if err := bootstrapAdmin(st, bootstrap); err != nil {
					return err
				}
			}

			keyFile := secretKeyFile
			if keyFile == "" {
				keyFile = filepath.Join(filepath.Dir(dbPath), "luncur.key")
			}
			sealer, err := secret.LoadOrCreate(keyFile)
			if err != nil {
				return err
			}

			var kubeClient *kube.Client
			kc, err := kube.New(kubeconfig)
			if err != nil {
				log.Printf("warning: kubernetes unavailable: %v", err)
			} else {
				kubeClient = kc
			}

			if kubeClient != nil {
				if err := build.EnsureSystem(context.Background(), kubeClient,
					"luncur-system", "luncur-data", "luncur-registry", "registry:2"); err != nil {
					log.Printf("warning: ensure system infra: %v", err)
				}
			}

			log.Printf("luncur serve listening on %s (db %s)", listen, dbPath)
			srv := &http.Server{
				Addr: listen,
				Handler: server.New(server.Deps{
					Store:        st,
					Sealer:       sealer,
					Kube:         kubeClient,
					ExternalIP:   externalIP,
					DataDir:      dataDir,
					BuilderImage: builderImage,
					RegistryHost: registryHost,
				}),
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
			}

			errCh := make(chan error, 1)
			go func() {
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errCh <- err
				} else {
					errCh <- nil
				}
			}()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				log.Printf("luncur serve shutting down")
				shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				return srv.Shutdown(shutCtx)
			}
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "luncur.db", "path to SQLite database")
	cmd.Flags().StringVar(&listen, "listen", ":8080", "listen address")
	cmd.Flags().StringVar(&bootstrap, "bootstrap-admin", "",
		"email:password — create initial admin if no users exist")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "",
		"path to kubeconfig (empty tries in-cluster config)")
	cmd.Flags().StringVar(&secretKeyFile, "secret-key-file", "",
		"path to secret sealing key (default luncur.key beside --db)")
	cmd.Flags().StringVar(&externalIP, "external-ip", "", "external IP advertised to clients")
	cmd.Flags().StringVar(&dataDir, "data-dir", "./data", "directory for source-build uploads/state")
	cmd.Flags().StringVar(&builderImage, "builder-image", "luncur/builder:latest", "buildkit builder image")
	cmd.Flags().StringVar(&registryHost, "registry-host", "registry.luncur-system:5000", "in-cluster registry host:port")
	return cmd
}
