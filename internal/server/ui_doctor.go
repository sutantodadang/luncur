package server

import (
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

// uiDoctorRow is doctor.html's per-row view model: a doctorCheck plus the
// chip class its status maps to (status names don't match chip suffixes
// one-to-one — "fail" renders as chip-bad — so the mapping lives here
// instead of in the template).
type uiDoctorRow struct {
	Name, Status, Detail, Chip string
}

func (s *server) handleUIDoctor(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	checks := s.runDoctor(r.Context())
	rows := make([]uiDoctorRow, 0, len(checks))
	for _, c := range checks {
		chip := "chip-muted"
		switch c.Status {
		case "ok":
			chip = "chip-ok"
		case "warn":
			chip = "chip-warn"
		case "fail":
			chip = "chip-bad"
		}
		rows = append(rows, uiDoctorRow{Name: c.Name, Status: c.Status, Detail: c.Detail, Chip: chip})
	}
	s.renderPage(w, "doctor.html", map[string]any{
		"User": u, "Version": s.version, "Checks": rows,
		"CSRF": s.csrf(w, r), "IsAdmin": true,
	})
}
