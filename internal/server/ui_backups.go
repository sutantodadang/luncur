package server

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"

	"github.com/sutantodadang/luncur/internal/store"
)

// uiBackupRow is backups.html's per-row view model: store.Backup plus a
// human-readable size and the path's base filename — the full server path
// never renders in the browser.
type uiBackupRow struct {
	ID        int64
	File      string
	Size      string
	Uploaded  bool
	CreatedAt string
}

// humanBytes renders n bytes as a short "12.3 MiB"-style string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (s *server) handleUIBackups(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	rows, err := s.st.ListBackups()
	if err != nil {
		log.Printf("ui backups: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]uiBackupRow, 0, len(rows))
	for _, b := range rows {
		out = append(out, uiBackupRow{
			ID: b.ID, File: filepath.Base(b.Path), Size: humanBytes(b.SizeBytes),
			Uploaded: b.Uploaded, CreatedAt: b.CreatedAt,
		})
	}
	s.renderPage(w, "backups.html", map[string]any{
		"User": u, "Backups": out, "CSRF": s.csrf(w, r), "IsAdmin": true,
	})
}

// handleUIBackupCreate is handleCreateBackup's UI twin: same createBackup
// core (always uploads, matching the CLI default), redirect instead of a
// JSON body.
func (s *server) handleUIBackupCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if _, _, err := s.createBackup(r.Context(), true); err != nil {
		log.Printf("ui create backup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "backup created")
	http.Redirect(w, r, "/ui/backups", http.StatusSeeOther)
}

// handleUIBackupPrune is handlePruneBackups' UI twin: same pruneBackups
// core, redirect instead of a JSON body.
func (s *server) handleUIBackupPrune(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	if _, err := s.pruneBackups(r.Context()); err != nil {
		log.Printf("ui prune backups: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "old backups pruned")
	http.Redirect(w, r, "/ui/backups", http.StatusSeeOther)
}
