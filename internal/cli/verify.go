package cli

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type verifyReport struct {
	Files     []string
	Tables    int
	Integrity string
	SealerKey bool
}

// verifyArchive restores archivePath into a throwaway dir and checks the
// result: SQLite opens, PRAGMA integrity_check passes, sealer key present.
// This is the automated restore drill — a backup nobody has restored is
// not a backup.
func verifyArchive(archivePath string) (verifyReport, error) {
	var rep verifyReport
	tmp, err := os.MkdirTemp("", "luncur-verify-*")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(tmp)

	files, err := restoreArchive(archivePath, tmp, true, time.Now)
	if err != nil {
		return rep, fmt.Errorf("restore into scratch dir: %w", err)
	}
	rep.Files = files

	db, err := sql.Open("sqlite", filepath.Join(tmp, "luncur.db"))
	if err != nil {
		return rep, err
	}
	defer db.Close()
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&rep.Integrity); err != nil {
		return rep, err
	}
	if rep.Integrity != "ok" {
		return rep, fmt.Errorf("integrity_check: %s", rep.Integrity)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&rep.Tables); err != nil {
		return rep, err
	}
	_, statErr := os.Stat(filepath.Join(tmp, "luncur.key"))
	rep.SealerKey = statErr == nil
	return rep, nil
}
