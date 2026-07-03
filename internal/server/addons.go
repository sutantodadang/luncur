package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/sutantodadang/luncur/internal/addon"
	"github.com/sutantodadang/luncur/internal/store"
)

// newAddonCreds mints credentials for a new addon instance: a random
// 24-hex-char password, plus the fixed user/db postgres expects (redis has
// no user/db, only a password).
func newAddonCreds(typ string) (addon.Creds, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return addon.Creds{}, err
	}
	pw := hex.EncodeToString(raw)
	if typ == "postgres" {
		return addon.Creds{User: "app", Password: pw, DB: "app"}, nil
	}
	return addon.Creds{Password: pw}, nil
}

// sealCreds/unsealCreds JSON-round-trip addon.Creds through the sealer —
// same pattern as app env vars (appenv.go).
func (s *server) sealCreds(c addon.Creds) ([]byte, error) {
	if s.sealer == nil {
		return nil, errSealerUnavailable
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return s.sealer.Seal(raw)
}

func (s *server) unsealCreds(a store.Addon) (addon.Creds, error) {
	if s.sealer == nil {
		return addon.Creds{}, errSealerUnavailable
	}
	raw, err := s.sealer.Open(a.CredsEnc)
	if err != nil {
		return addon.Creds{}, err
	}
	var c addon.Creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return addon.Creds{}, err
	}
	return c, nil
}

// addonEnv computes the connection env an app's attached addons inject:
// DATABASE_URL / REDIS_URL, or DATABASE_URL_<NAME> (name uppercased,
// dashes→underscores) for a second addon of the same type. A key already
// present in userEnv is left out of the returned map (user wins) and
// reported in the collisions slice instead.
func (s *server) addonEnv(p store.Project, a store.App, userEnv map[string]string) (map[string]string, []string, error) {
	addons, err := s.st.AddonsForApp(a.ID)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]string{}
	var collisions []string
	seenType := map[string]bool{}
	for _, ad := range addons {
		creds, err := s.unsealCreds(ad)
		if err != nil {
			return nil, nil, fmt.Errorf("unseal addon %s creds: %w", ad.Name, err)
		}
		host := addon.ServiceName(ad.Name) + "." + p.Namespace

		var key, url string
		switch ad.Type {
		case "postgres":
			key = "DATABASE_URL"
			url = fmt.Sprintf("postgres://%s:%s@%s:5432/%s", creds.User, creds.Password, host, creds.DB)
		case "redis":
			key = "REDIS_URL"
			url = fmt.Sprintf("redis://:%s@%s:6379", creds.Password, host)
		}
		if seenType[ad.Type] {
			key = key + "_" + strings.ToUpper(strings.ReplaceAll(ad.Name, "-", "_"))
		}
		seenType[ad.Type] = true

		if _, taken := userEnv[key]; taken {
			collisions = append(collisions, key)
			continue
		}
		out[key] = url
	}
	return out, collisions, nil
}

// requireAddon loads an addon within a project by name. Writes the error
// response and returns ok=false on failure.
func (s *server) requireAddon(w http.ResponseWriter, p store.Project, name string) (store.Addon, bool) {
	a, err := s.st.GetAddon(p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such addon")
		return store.Addon{}, false
	}
	if err != nil {
		log.Printf("get addon: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Addon{}, false
	}
	return a, true
}

// createAddon is the shared core of handleCreateAddon: mint credentials,
// seal them, store the addon row, render and apply its manifests, and
// optionally attach it to an app (the CLI's `addon add` sugar). name and
// version default when empty: name to "<type><n>" (n = count of existing
// addons of that type, plus one), version to postgres 16 / redis 7.
func (s *server) createAddon(ctx context.Context, p store.Project, typ, name, version string, sizeGB int, appName string) (store.Addon, error) {
	if name == "" {
		existing, err := s.st.ListAddons(p.ID)
		if err != nil {
			return store.Addon{}, err
		}
		n := 1
		for _, a := range existing {
			if a.Type == typ {
				n++
			}
		}
		name = fmt.Sprintf("%s%d", typ, n)
	}
	if version == "" {
		switch typ {
		case "postgres":
			version = "16"
		case "redis":
			version = "7"
		}
	}

	creds, err := newAddonCreds(typ)
	if err != nil {
		return store.Addon{}, err
	}
	sealed, err := s.sealCreds(creds)
	if err != nil {
		return store.Addon{}, err
	}

	a, err := s.st.CreateAddon(p.ID, typ, name, version, sizeGB, sealed)
	if err != nil {
		return store.Addon{}, err
	}

	objs, err := addon.Render(addon.Params{
		Namespace: p.Namespace, Type: a.Type, Name: a.Name, Version: a.Version,
		SizeGB: a.SizeGB, Creds: creds,
	})
	if err != nil {
		return store.Addon{}, err
	}
	if err := s.kube.EnsureNamespace(ctx, p.Namespace); err != nil {
		return store.Addon{}, err
	}
	if err := s.kube.Apply(ctx, p.Namespace, objs); err != nil {
		return store.Addon{}, err
	}

	if appName != "" {
		app, err := s.st.GetApp(p.ID, appName)
		if err != nil {
			return store.Addon{}, err
		}
		if err := s.st.AttachAddon(a.ID, app.ID); err != nil {
			return store.Addon{}, err
		}
		s.syncIfLive(ctx, p, app)
	}

	return a, nil
}

func (s *server) handleCreateAddon(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	var req struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Version string `json:"version"`
		SizeGB  int    `json:"size_gb"`
		App     string `json:"app"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	a, err := s.createAddon(r.Context(), p, req.Type, req.Name, req.Version, req.SizeGB, req.App)
	if err != nil {
		switch {
		case errors.Is(err, errSealerUnavailable):
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no such app")
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}

	attachedTo := []string{}
	if req.App != "" {
		attachedTo = []string{req.App}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": a.Name, "type": a.Type, "version": a.Version,
		"status": "provisioning", "attached_to": attachedTo,
	})
}

// addonRow is a project addon's list-view model — its StatefulSet
// readiness and the apps it's attached to — shared by the JSON API
// (handleListAddons) and both UI addon sections (project + app pages).
type addonRow struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Version    string   `json:"version"`
	SizeGB     int      `json:"size_gb"`
	Ready      bool     `json:"ready"`
	AttachedTo []string `json:"attached_to"`
}

// addonRows builds every addon row for a project.
func (s *server) addonRows(ctx context.Context, p store.Project) ([]addonRow, error) {
	list, err := s.st.ListAddons(p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]addonRow, 0, len(list))
	for _, a := range list {
		ready := false
		if s.kube != nil {
			ready, err = s.kube.StatefulSetReady(ctx, p.Namespace, addon.ServiceName(a.Name))
			if err != nil {
				log.Printf("statefulset ready %s: %v", a.Name, err)
				ready = false
			}
		}
		apps, err := s.st.AppsForAddon(a.ID)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(apps))
		for _, app := range apps {
			names = append(names, app.Name)
		}
		out = append(out, addonRow{
			Name: a.Name, Type: a.Type, Version: a.Version, SizeGB: a.SizeGB,
			Ready: ready, AttachedTo: names,
		})
	}
	return out, nil
}

func (s *server) handleListAddons(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	rows, err := s.addonRows(r.Context(), p)
	if err != nil {
		log.Printf("list addons: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// attachAddon is the shared core of handleAttachAddon: attach the addon to
// an app, then re-sync if the app is live. Returns a warning ("" when
// none) when the addon's injected env key collides with a user-set var —
// user wins, so attaching doesn't change the app's actual env.
func (s *server) attachAddon(ctx context.Context, p store.Project, ad store.Addon, appName string) (string, error) {
	app, err := s.st.GetApp(p.ID, appName)
	if err != nil {
		return "", err
	}
	if app.Ejected {
		return "", errAppEjected
	}
	if err := s.st.AttachAddon(ad.ID, app.ID); err != nil {
		return "", err
	}

	userEnv, err := s.plainEnv(app)
	if err != nil {
		return "", err
	}
	_, collisions, err := s.addonEnv(p, app, userEnv)
	if err != nil {
		return "", err
	}

	s.syncIfLive(ctx, p, app)

	if len(collisions) > 0 {
		return fmt.Sprintf("env var(s) already set on the app, addon value not applied: %s", strings.Join(collisions, ", ")), nil
	}
	return "", nil
}

func (s *server) handleAttachAddon(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	ad, ok := s.requireAddon(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	var req struct {
		App string `json:"app"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	warning, err := s.attachAddon(r.Context(), p, ad, req.App)
	if err != nil {
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no such app")
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}

	if warning != "" {
		writeJSON(w, http.StatusOK, map[string]any{"warning": warning})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDetachAddon(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	ad, ok := s.requireAddon(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	var req struct {
		App string `json:"app"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	app, ok := s.requireApp(w, p, req.App)
	if !ok {
		return
	}
	if s.refuseEjected(w, app) {
		return
	}

	if err := s.st.DetachAddon(ad.ID, app.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "addon is not attached to this app")
			return
		}
		log.Printf("detach addon: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.syncIfLive(r.Context(), p, app)
	w.WriteHeader(http.StatusNoContent)
}

// errAddonAttached guards removeAddon: an addon still attached to one or
// more apps is not deleted unless force is set.
var errAddonAttached = errors.New("addon is attached to one or more apps")

// removeAddon is the shared core of handleDeleteAddon and its UI twin:
// guard against silently orphaning a live app's connection (unless force),
// delete the cluster objects, then the store row. Caller must have already
// confirmed s.kube is non-nil.
func (s *server) removeAddon(ctx context.Context, p store.Project, ad store.Addon, force, keepData bool) error {
	apps, err := s.st.AppsForAddon(ad.ID)
	if err != nil {
		return err
	}
	if len(apps) > 0 && !force {
		return errAddonAttached
	}
	if err := s.kube.DeleteAddonObjects(ctx, p.Namespace, ad.Name, keepData); err != nil {
		return err
	}
	return s.st.DeleteAddon(ad.ID)
}

func (s *server) handleDeleteAddon(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	ad, ok := s.requireAddon(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	force := r.URL.Query().Get("force") == "1"
	keepData := r.URL.Query().Get("keep_data") == "1"

	if err := s.removeAddon(r.Context(), p, ad, force, keepData); err != nil {
		if errors.Is(err, errAddonAttached) {
			writeError(w, http.StatusConflict, "addon_attached", "addon is attached to one or more apps; pass ?force=1 to remove anyway")
			return
		}
		log.Printf("delete addon: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
