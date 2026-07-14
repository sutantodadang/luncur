package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// requireProjectWrite is requireProject plus write authorization: global
// admins and role=member pass; role=viewer gets 403 read_only.
func (s *server) requireProjectWrite(w http.ResponseWriter, u store.User, name string) (store.Project, bool) {
	p, ok := s.requireProject(w, u, name)
	if !ok {
		return p, false
	}
	if u.Role == "admin" {
		return p, true
	}
	role, err := s.st.MemberRole(p.ID, u.ID)
	if err == nil && role == "viewer" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"read_only","message":"viewers cannot modify this project"}`))
		return p, false
	}
	return p, true
}

// requireEnv resolves a project (membership-checked exactly like
// requireProject) and then one of its environments: envName=="" resolves to
// the project's DefaultEnv, so every legacy (env-less) caller keeps
// resolving to the same environment it always implicitly used. A missing
// project or environment writes the response and returns ok=false.
func (s *server) requireEnv(w http.ResponseWriter, r *http.Request, u store.User, project, envName string) (store.Project, store.Environment, bool) {
	p, ok := s.requireProject(w, u, project)
	if !ok {
		return store.Project{}, store.Environment{}, false
	}
	return s.resolveEnv(w, p, envName)
}

// requireEnvWrite is requireEnv plus write authorization: global admins and
// role=member pass; role=viewer gets 403 read_only, mirroring
// requireProjectWrite.
func (s *server) requireEnvWrite(w http.ResponseWriter, r *http.Request, u store.User, project, envName string) (store.Project, store.Environment, bool) {
	p, ok := s.requireProjectWrite(w, u, project)
	if !ok {
		return store.Project{}, store.Environment{}, false
	}
	return s.resolveEnv(w, p, envName)
}

// resolveEnv is requireEnv/requireEnvWrite's shared tail: look up the named
// environment (project.DefaultEnv when envName is empty), 404 on missing.
func (s *server) resolveEnv(w http.ResponseWriter, p store.Project, envName string) (store.Project, store.Environment, bool) {
	if envName == "" {
		envName = p.DefaultEnv
	}
	env, err := s.st.GetEnvironment(p.ID, envName)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such environment")
		return store.Project{}, store.Environment{}, false
	}
	if err != nil {
		log.Printf("get environment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Project{}, store.Environment{}, false
	}
	return p, env, true
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
	// Every project needs a resolvable default environment for requireEnv to
	// fall back to (env-less legacy routes/CLI calls resolve envName==""
	// to p.DefaultEnv). Full env CRUD + CLI (Task 8) comes later; this seeds
	// the same production/develop/staging trio backfillEnvironments gives
	// legacy projects, so a freshly created project behaves identically.
	if err := s.st.SeedProjectEnvironments(p.ID); err != nil {
		log.Printf("seed project environments: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, projectJSON(p))
}

// handleRenameProject changes a project's name in place; the k8s namespace
// (derived at creation) is untouched, so existing cluster objects stay put.
func (s *server) handleRenameProject(w http.ResponseWriter, r *http.Request, u store.User) {
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
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if err := s.st.RenameProject(p.ID, req.Name); err != nil {
		// Same duplicate-name reasoning as handleCreateProject: name and
		// k8s_namespace are both UNIQUE and derive 1:1, so either tripping
		// means "duplicate project".
		if strings.Contains(err.Error(), "UNIQUE constraint failed: projects.") {
			writeError(w, http.StatusConflict, "project_exists", "project already exists")
			return
		}
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("rename project: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	renamed, err := s.st.GetProjectByID(p.ID)
	if err != nil {
		log.Printf("rename project: reload: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, projectJSON(renamed))
}

// deleteProject is handleDeleteProject's and handleUIProjectDelete's shared
// core: tear down every addon then every app (kube objects + row each),
// drop the project's namespace, then the project row itself. A mid-way
// error is safe to retry — nothing here is destroyed twice.
func (s *server) deleteProject(ctx context.Context, p store.Project, apps []store.App, addons []store.Addon) error {
	// The whole project is being torn down at once (every environment along
	// with it, once multi-environment teardown lands — see Task 15's
	// teardownPreview); today that's just the default (production)
	// environment, whose namespace is byte-identical to p.Namespace.
	env, err := s.st.GetEnvironment(p.ID, p.DefaultEnv)
	if err != nil {
		return fmt.Errorf("get default environment: %w", err)
	}
	for _, ad := range addons {
		// force=true: the project is being destroyed outright, so an
		// addon still attached to one of its own apps isn't a reason to
		// stop. keepData=false: volumes go with everything else.
		if err := s.removeAddon(ctx, p, env, ad, true, false); err != nil {
			return fmt.Errorf("remove addon %s: %w", ad.Name, err)
		}
	}
	for _, a := range apps {
		if err := s.destroyApp(ctx, p, env, a); err != nil {
			return fmt.Errorf("destroy app %s: %w", a.Name, err)
		}
	}
	if s.kube != nil {
		// Namespace may already be gone (e.g. a prior partial run); log
		// and continue rather than fail the whole delete on that alone.
		if err := s.kube.DeleteNamespace(ctx, p.Namespace); err != nil {
			log.Printf("delete project: delete namespace %s: %v", p.Namespace, err)
		}
	}
	return s.st.DeleteProject(p.ID)
}

// projectNeedsKube reports whether tearing down p requires a configured
// kube client: any addon, or any non-ejected app.
func projectNeedsKube(apps []store.App, addons []store.Addon) bool {
	if len(addons) > 0 {
		return true
	}
	for _, a := range apps {
		if !a.Ejected {
			return true
		}
	}
	return false
}

func (s *server) handleDeleteProject(w http.ResponseWriter, r *http.Request, u store.User) {
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

	apps, err := s.st.ListApps(p.ID)
	if err != nil {
		log.Printf("delete project: list apps: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	addons, err := s.st.ListAddons(p.ID)
	if err != nil {
		log.Printf("delete project: list addons: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if projectNeedsKube(apps, addons) && !s.requireKube(w) {
		return
	}

	if err := s.deleteProject(r.Context(), p, apps, addons); err != nil {
		log.Printf("delete project: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveMember drops a member from a project. Removing a non-member,
// or a member of an unknown user, both 404 — same not-found style as the
// rest of the project API.
func (s *server) handleRemoveMember(w http.ResponseWriter, r *http.Request, u store.User) {
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

	member, err := s.st.GetUserByEmail(r.PathValue("email"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	if err != nil {
		log.Printf("get user by email: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.st.RemoveMember(p.ID, member.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "not a member")
			return
		}
		log.Printf("remove member: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// visibleProjects returns the projects u may see: admins see every project,
// members see only those they belong to. Shared by the API and UI project
// listings so the visibility rule can't drift between the two.
func (s *server) visibleProjects(u store.User) ([]store.Project, error) {
	if u.Role == "admin" {
		return s.st.ListProjects()
	}
	return s.st.ListProjectsFor(u.ID)
}

func (s *server) handleListProjects(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.visibleProjects(u)
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
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	role := req.Role
	if role == "" {
		role = "member"
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

	if err := s.st.AddMember(p.ID, member.ID, role); err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "bad_request", ve.Error())
			return
		}
		log.Printf("add member: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
