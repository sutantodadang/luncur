package cli

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

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

			log.Printf("luncur serve listening on %s (db %s)", listen, dbPath)
			srv := &http.Server{
				Addr: listen,
				Handler: server.New(server.Deps{
					Store:      st,
					Sealer:     sealer,
					Kube:       kubeClient,
					ExternalIP: externalIP,
				}),
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
			return srv.ListenAndServe()
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
	return cmd
}
