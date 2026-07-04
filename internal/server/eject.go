package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

var errAppEjected = errors.New("app is ejected from luncur management")

// refuseEjected 409s mutations on ejected apps. Reads never call this.
func (s *server) refuseEjected(w http.ResponseWriter, a store.App) bool {
	if !a.Ejected {
		return false
	}
	writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
	return true
}

func (s *server) handleEjectApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}

	image, err := s.appImage(a)
	if err != nil {
		log.Printf("eject %s: image: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	rendered, err := s.renderApp(p, a, image, true)
	if err != nil {
		log.Printf("eject %s: render: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	y, err := render.YAML(rendered)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.st.SetAppEjected(a.ID); err != nil {
		log.Printf("eject %s: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	saved := ""
	if s.dataDir != "" {
		dir := filepath.Join(s.dataDir, "ejected")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			saved = filepath.Join(dir, fmt.Sprintf("%s-%s.yaml", p.Name, a.Name))
			if err := os.WriteFile(saved, y, 0o600); err != nil {
				log.Printf("eject %s: save yaml: %v", a.Name, err)
				saved = ""
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"yaml": string(y), "saved_to": saved})
}

// handleAdoptApp reverses eject: clears the flag and re-applies luncur's
// rendered state onto the still-running objects, reclaiming
// fieldManager=luncur (and overwriting any drift — documented behavior).
func (s *server) handleAdoptApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if !a.Ejected {
		writeError(w, http.StatusConflict, "not_ejected", "app is not ejected")
		return
	}

	if err := s.st.SetAppAdopted(a.ID); err != nil {
		log.Printf("adopt %s: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.Ejected = false

	out := map[string]any{"adopted": true}
	if s.kube != nil {
		if err := s.syncApp(r.Context(), p, a); err != nil {
			log.Printf("adopt %s: sync: %v", a.Name, err)
			out["warning"] = "adopted, but re-apply failed: " + err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}
