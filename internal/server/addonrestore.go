package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// errRestoreUnsupported: addon type has no logical dump to restore.
var errRestoreUnsupported = errors.New("addon-data restore is only supported for postgres and redis")

// restoreAddon streams a logical dump back into an addon's pod via
// pods/exec — the inverse of dumpAddon. Credentials are read from the pod's
// own environment, never placed on the command line or logged. postgres
// runs pg_restore --clean; redis overwrites dump.rdb and forces a reload.
func (s *server) restoreAddon(ctx context.Context, ad store.Addon, dump []byte) error {
	p, err := s.st.GetProjectByID(ad.ProjectID)
	if err != nil {
		return err
	}
	pod := "addon-" + ad.Name + "-0"
	var cmd []string
	switch ad.Type {
	case "postgres":
		cmd = []string{"sh", "-c", `PGPASSWORD="$POSTGRES_PASSWORD" pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists --no-owner`}
	case "redis":
		// ponytail: assumes RDB persistence (no AOF) — luncur's redis addon
		// has none, so overwriting dump.rdb then forcing DEBUG RELOAD is a
		// faithful restore of the dumped dataset.
		cmd = []string{"sh", "-c", `cat > /data/dump.rdb && redis-cli -a "$REDIS_PASSWORD" --no-auth-warning DEBUG RELOAD NOSAVE`}
	default:
		return errRestoreUnsupported
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var errBuf bytes.Buffer
	if err := s.execer.ExecPod(dctx, p.Namespace, pod, ad.Type, cmd, bytes.NewReader(dump), io.Discard, &errBuf); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// handleRestoreAddon restores a postgres/redis addon's logical dump,
// uploaded as multipart field "dump" — the exact inverse of the backup
// archive's addons/* member. minio/mlflow addons have no logical dump and
// are rejected with 400.
func (s *server) handleRestoreAddon(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}
	a, ok := s.requireAddon(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	if s.execer == nil {
		writeError(w, http.StatusServiceUnavailable, "exec_unavailable", "kubernetes exec is not available")
		return
	}
	if a.Type != "postgres" && a.Type != "redis" {
		writeError(w, http.StatusBadRequest, "unsupported", errRestoreUnsupported.Error())
		return
	}

	// 2 GiB: generous ceiling for a logical dump; backup already holds a
	// whole addon dump in memory the same way.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart body")
		return
	}
	file, _, err := r.FormFile("dump")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing dump file")
		return
	}
	defer file.Close()
	dump, err := io.ReadAll(file)
	if err != nil {
		log.Printf("read dump upload: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if len(dump) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "empty dump")
		return
	}

	if err := s.restoreAddon(r.Context(), a, dump); err != nil {
		log.Printf("restore addon %s: %v", a.Name, err)
		writeError(w, http.StatusBadGateway, "restore_failed", "could not restore addon data")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": a.Name, "type": a.Type})
}
