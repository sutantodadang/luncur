package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

func projectJSON(p store.Project) map[string]any {
	return map[string]any{"id": p.ID, "name": p.Name, "namespace": p.Namespace}
}

// requireProject loads a project by name and checks that u may access it:
// admins may access any project, members must be in project_members.
// Writes the error response and returns ok=false on failure.
func (s *server) requireProject(w http.ResponseWriter, u store.User, name string) (store.Project, bool) {
	p, err := s.st.GetProject(name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such project")
		return store.Project{}, false
	}
	if err != nil {
		log.Printf("get project: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Project{}, false
	}
	if u.Role != "admin" {
		ok, err := s.st.IsMember(p.ID, u.ID)
		if err != nil || !ok {
			writeError(w, http.StatusForbidden, "forbidden", "not a member of this project")
			return store.Project{}, false
		}
	}
	return p, true
}

func (s *server) handleCreateProject(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	p, err := s.st.CreateProject(req.Name)
	if err != nil {
		// Either projects.name or projects.k8s_namespace may trip first;
		// namespace derives 1:1 from name, so both mean "duplicate project".
		if strings.Contains(err.Error(), "UNIQUE constraint failed: projects.") {
			writeError(w, http.StatusConflict, "project_exists", "project already exists")
			return
		}
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("create project: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, projectJSON(p))
}

func (s *server) handleListProjects(w http.ResponseWriter, r *http.Request, u store.User) {
	var (
		list []store.Project
		err  error
	)
	if u.Role == "admin" {
		list, err = s.st.ListProjects()
	} else {
		list, err = s.st.ListProjectsFor(u.ID)
	}
	if err != nil {
		log.Printf("list projects: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, projectJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleAddMember(w http.ResponseWriter, r *http.Request, u store.User) {
	name := r.PathValue("project")
	p, err := s.st.GetProject(name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such project")
		return
	}
	if err != nil {
		log.Printf("get project: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	member, err := s.st.GetUserByEmail(req.Email)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	if err != nil {
		log.Printf("get user by email: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.st.AddMember(p.ID, member.ID); err != nil {
		log.Printf("add member: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
