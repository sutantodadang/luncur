package server

// One-click template installs (internal/templates): create the app with
// its volumes, create and attach its addons, resolve and store its env,
// then deploy once the addons are ready (a first deploy that races its
// database would crash-loop and fail the rollout gate).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/sutantodadang/luncur/internal/addon"
	"github.com/sutantodadang/luncur/internal/store"
	"github.com/sutantodadang/luncur/internal/templates"
)

// Test seams.
var (
	templateAddonWait = 5 * time.Minute
	templateAddonPoll = 3 * time.Second
)

// installStep is one step of a template install, reported so a partial
// failure tells the user exactly what exists and what's left.
type installStep struct {
	Step   string `json:"step"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type installResult struct {
	App      string        `json:"app"`
	Template string        `json:"template"`
	Steps    []installStep `json:"steps"`
	DeployID string        `json:"deployment_id,omitempty"`
	Seq      int64         `json:"seq,omitempty"`
	URL      string        `json:"url,omitempty"`
	Error    string        `json:"error,omitempty"`
}

var errTemplateAppExists = errors.New("an app with that name already exists")

// installTemplate runs the install; res always carries the steps done.
func (s *server) installTemplate(ctx context.Context, p store.Project, env store.Environment, u store.User, tpl templates.Template, appName string, extra map[string]string) (installResult, error) {
	if appName == "" {
		appName = tpl.Name
	}
	res := installResult{App: appName, Template: tpl.Name}
	step := func(name string, err error, detail string) error {
		st := installStep{Step: name, OK: err == nil, Detail: detail}
		if err != nil {
			st.Detail = err.Error()
			res.Error = name + ": " + err.Error()
		}
		res.Steps = append(res.Steps, st)
		return err
	}
	if s.sealer == nil {
		return res, step("check", errSealerUnavailable, "")
	}
	if _, err := s.st.GetAppInEnv(env.ID, appName); err == nil {
		return res, step("create app", errTemplateAppExists, "")
	}

	a, err := s.st.CreateAppInEnv(env.ID, appName, tpl.App.Port, "web", "")
	if step("create app", err, fmt.Sprintf("%s (web, port %d)", appName, tpl.App.Port)) != nil {
		return res, err
	}
	if tpl.App.HealthPath != "" {
		if err := s.st.SetHealthPath(a.ID, tpl.App.HealthPath); step("health check", err, tpl.App.HealthPath) != nil {
			return res, err
		}
	}
	if tpl.App.CPU != "" || tpl.App.Memory != "" {
		cpu, err := parseCPUMilli(tpl.App.CPU)
		if err == nil {
			var mem int64
			if mem, err = parseMemoryMB(tpl.App.Memory); err == nil {
				err = s.st.SetResources(a.ID, cpu, mem)
			}
		}
		if step("resources", err, fmt.Sprintf("cpu %s memory %s", orDash(tpl.App.CPU), orDash(tpl.App.Memory))) != nil {
			return res, err
		}
	}
	for _, v := range tpl.Volumes {
		_, err := s.st.AddVolume(a.ID, v.Name, v.Path, v.SizeGB)
		if step("volume "+v.Name, err, fmt.Sprintf("%s (%dGi)", v.Path, v.SizeGB)) != nil {
			return res, err
		}
	}

	conns := map[string]templates.Conn{}
	var addonNames []string
	for _, ta := range tpl.Addons {
		name := appName + "-" + ta.Key
		ad, err := s.createAddon(ctx, p, env, ta.Type, name, "", 2, appName)
		if step("addon "+name, err, ta.Type+", attached to "+appName) != nil {
			return res, err
		}
		creds, err := s.unsealCreds(ad)
		if err != nil {
			return res, step("addon "+name, err, "")
		}
		_, url := addonKeyURL(ad.Type, ad.Name, env.Namespace, creds)
		conns[ta.Key] = templates.Conn{
			Host: addon.ServiceName(ad.Name) + "." + env.Namespace, Port: addonPort(ad.Type),
			User: creds.User, Password: creds.Password, Database: creds.DB, URL: url,
		}
		addonNames = append(addonNames, ad.Name)
	}

	res.URL = s.appURLForEnv(a, env.Name, p.DefaultEnv)
	vars, err := tpl.Resolve(res.URL, hostForEnv(a.Name, env.Name, p.DefaultEnv, s.externalIP), conns, extra)
	if err == nil && len(vars) > 0 {
		err = s.setAppEnvBulk(ctx, p, env, a, vars)
	}
	if step("env", err, fmt.Sprintf("%d variable(s)", len(vars))) != nil {
		return res, err
	}

	d, err := s.st.CreateDeployment(a.ID, "deploying", tpl.App.Image, u.ID)
	if step("deploy", err, tpl.App.Image+" (after addons are ready)") != nil {
		return res, err
	}
	res.DeployID, res.Seq = d.ID, d.Seq
	go s.deployTemplateWhenReady(p, env, a, d, tpl.App.Image, addonNames)
	return res, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func addonPort(typ string) string {
	switch typ {
	case "postgres":
		return "5432"
	case "redis":
		return "6379"
	case "minio":
		return strconv.Itoa(int(addon.MinIOPort))
	}
	return ""
}

// deployTemplateWhenReady waits (bounded) for the template's addons, then
// deploys the image through the normal gated path.
func (s *server) deployTemplateWhenReady(p store.Project, env store.Environment, a store.App, d store.Deployment, image string, addons []string) {
	ctx, cancel := context.WithTimeout(context.Background(), templateAddonWait+10*time.Minute)
	defer cancel()
	deadline := time.Now().Add(templateAddonWait)
	for _, name := range addons {
		for {
			ready, err := s.kube.StatefulSetReady(ctx, env.Namespace, addon.ServiceName(name))
			if err == nil && ready {
				break
			}
			if time.Now().After(deadline) {
				s.buildLogf(d, "addon %s not ready after %s — deploying anyway", name, templateAddonWait)
				break
			}
			time.Sleep(templateAddonPoll)
		}
	}
	if err := s.applyImageDeploy(ctx, p, env, a, d, image); err != nil {
		log.Printf("template deploy %s/%s: %v", p.Name, a.Name, err)
	}
}

func templateJSON(t templates.Template) map[string]any {
	addons := make([]string, 0, len(t.Addons))
	for _, a := range t.Addons {
		addons = append(addons, a.Type)
	}
	return map[string]any{"name": t.Name, "title": t.Title, "description": t.Description, "category": t.Category,
		"image": t.App.Image, "addons": addons, "volumes": len(t.Volumes)}
}

func (s *server) handleListTemplates(w http.ResponseWriter, r *http.Request, u store.User) {
	all := templates.All()
	out := make([]map[string]any, 0, len(all))
	for _, t := range all {
		out = append(out, templateJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (s *server) handleInstallTemplate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}
	tpl, ok := templates.Find(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such template")
		return
	}
	var req struct {
		AppName string            `json:"app_name"`
		Env     map[string]string `json:"env"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}
	res, err := s.installTemplate(r.Context(), p, env, u, tpl, req.AppName, req.Env)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errTemplateAppExists) {
			code = http.StatusConflict
		}
		writeJSON(w, code, res)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// handleUIInstallTemplate is handleInstallTemplate's UI twin (project page
// gallery): on success it lands on the new app's Overview.
func (s *server) handleUIInstallTemplate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	if s.kube == nil {
		http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		return
	}
	env, ok := s.uiEnv(w, r, p)
	if !ok {
		return
	}
	tpl, found := templates.Find(r.PathValue("name"))
	if !found {
		http.Error(w, "no such template", http.StatusNotFound)
		return
	}
	res, err := s.installTemplate(r.Context(), p, env, u, tpl, r.PostFormValue("app_name"), nil)
	if err != nil {
		flash(w, "err", res.Error)
		http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
		return
	}
	flash(w, "ok", fmt.Sprintf("%s installed — deploying #%d once its addons are ready", res.App, res.Seq))
	path := "/ui/projects/" + p.Name + "/apps/" + res.App
	if env.Name != p.DefaultEnv {
		path = "/ui/projects/" + p.Name + "/envs/" + env.Name + "/apps/" + res.App
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}
