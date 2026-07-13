// Package client is the Go client for the luncur REST API, used by the CLI.
package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

type UserInfo struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

func New(server, token string) *Client {
	return &Client{
		base:  strings.TrimRight(server, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// do sends a JSON request and decodes a JSON response. Non-2xx responses
// are turned into errors carrying the envelope's message and code.
func (c *Client) do(method, path string, in, out any) error {
	body := bytes.NewBuffer(nil)
	if in != nil {
		if err := json.NewEncoder(body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&env) == nil && env.Error.Code != "" {
			return fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Login(email, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do("POST", "/v1/login",
		map[string]string{"email": email, "password": password}, &out)
	return out.Token, err
}

func (c *Client) Me() (UserInfo, error) {
	var out UserInfo
	err := c.do("GET", "/v1/me", nil, &out)
	return out, err
}

func (c *Client) CreateUser(email, password, role string) (UserInfo, error) {
	var out UserInfo
	err := c.do("POST", "/v1/users",
		map[string]string{"email": email, "password": password, "role": role}, &out)
	return out, err
}

// ListUsers fetches every user (admin only).
func (c *Client) ListUsers() ([]UserInfo, error) {
	var out []UserInfo
	err := c.do("GET", "/v1/users", nil, &out)
	return out, err
}

// ChangePassword updates the logged-in user's password.
func (c *Client) ChangePassword(oldPW, newPW string) error {
	return c.do("PUT", "/v1/me/password",
		map[string]string{"old_password": oldPW, "new_password": newPW}, nil)
}

// ChangeEmail updates the logged-in user's login email.
func (c *Client) ChangeEmail(password, email string) error {
	return c.do("PUT", "/v1/me/email",
		map[string]string{"password": password, "email": email}, nil)
}

// SetUserPassword sets any user's password (admin only).
func (c *Client) SetUserPassword(id int64, password string) error {
	return c.do("PUT", fmt.Sprintf("/v1/users/%d/password", id),
		map[string]string{"password": password}, nil)
}

// doRaw sends a request with raw byte body and returns raw byte response.
// Non-2xx responses still decode the JSON error envelope.
func (c *Client) doRaw(method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &env) == nil && env.Error.Code != "" {
			return nil, fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}
	return respBody, nil
}

// doMultipart sends a request with a pre-built multipart body and decodes a
// JSON response, mirroring do's error-envelope handling.
func (c *Client) doMultipart(method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&env) == nil && env.Error.Code != "" {
			return fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type ProjectInfo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type AppInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Replicas    int    `json:"replicas"`
	URL         string `json:"url"`
	Internal    bool   `json:"internal,omitempty"`
	InternalURL string `json:"internal_url,omitempty"`
	Status      string `json:"status,omitempty"`
	Image       string `json:"image,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
	GPU         int64  `json:"gpu,omitempty"`
	ModelSource string `json:"model_source,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Seq         int64  `json:"seq,omitempty"`
	Autoscale   *AutoscaleInfo `json:"autoscale,omitempty"`
}

// AutoscaleInfo mirrors appJSON's "autoscale" block, present only when
// autoscale is on (min > 0).
type AutoscaleInfo struct {
	Min int `json:"min"`
	Max int `json:"max"`
	CPU int `json:"cpu"`
}

type DeployResult struct {
	DeploymentID string `json:"deployment_id"`
	Seq          int64  `json:"seq"`
	Status       string `json:"status"`
	URL          string `json:"url"`
}

// DeployInfo is one row of an app's deploy history, as returned by
// ListDeploys — Seq is the per-app human-facing deploy number; ID is the
// opaque internal id the rollback API still expects.
type DeployInfo struct {
	ID             string `json:"id"`
	Seq            int64  `json:"seq"`
	Status         string `json:"status"`
	Image          string `json:"image"`
	CreatedAt      string `json:"created_at"`
	RolledBackFrom string `json:"rolled_back_from,omitempty"`
}

func (c *Client) CreateProject(name string) (ProjectInfo, error) {
	var out ProjectInfo
	err := c.do("POST", "/v1/projects",
		map[string]string{"name": name}, &out)
	return out, err
}

func (c *Client) ListProjects() ([]ProjectInfo, error) {
	var out []ProjectInfo
	err := c.do("GET", "/v1/projects", nil, &out)
	return out, err
}

// AddMember adds email to project with the given role ("member" or
// "viewer"); an empty role defaults to "member" server-side.
func (c *Client) AddMember(project, email, role string) error {
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/members",
		map[string]string{"email": email, "role": role}, nil)
}

// RenameProject changes a project's name in place; its k8s namespace never
// changes, so cluster objects stay where they are.
func (c *Client) RenameProject(name, newName string) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(name),
		map[string]string{"name": newName}, nil)
}

// DeleteProject destroys a project and everything in it: apps, addons,
// domains, volumes, and the project's namespace.
func (c *Client) DeleteProject(name string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(name), nil, nil)
}

func (c *Client) RemoveMember(project, email string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/members/"+url.PathEscape(email), nil, nil)
}

func (c *Client) CreateApp(project, name string, port int, kind, schedule, buildPath string, internal bool, gpu int64) (AppInfo, error) {
	var out AppInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps",
		map[string]interface{}{"name": name, "port": port, "kind": kind, "schedule": schedule, "build_path": buildPath, "internal": internal, "gpu": gpu}, &out)
	return out, err
}

// CreateModelApp registers (and, for built-in runtimes, immediately
// deploys) a kind=model app serving an OpenAI-compatible endpoint.
func (c *Client) CreateModelApp(project, name, source, runtime string, gpu int64) (AppInfo, error) {
	var out AppInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps",
		map[string]any{"name": name, "kind": "model", "model_source": source, "runtime": runtime, "gpu": gpu}, &out)
	return out, err
}

func (c *Client) ListApps(project string) ([]AppInfo, error) {
	var out []AppInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps", nil, &out)
	return out, err
}

func (c *Client) GetApp(project, app string) (AppInfo, error) {
	var out AppInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app), nil, &out)
	return out, err
}

// MetricsInfo is an app's live CPU/memory/replica/deploy-count snapshot, as
// returned by GET .../metrics. Available is false when metrics-server isn't
// reachable; DeployCount is always populated.
type MetricsInfo struct {
	Available       bool  `json:"available"`
	CPUMillicores   int64 `json:"cpu_millicores"`
	MemoryMiB       int64 `json:"memory_mib"`
	Pods            int   `json:"pods"`
	ReadyReplicas   int64 `json:"ready_replicas"`
	DesiredReplicas int64 `json:"desired_replicas"`
	DeployCount     int64 `json:"deploy_count"`
}

func (c *Client) AppMetrics(project, app string) (MetricsInfo, error) {
	var out MetricsInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/metrics", nil, &out)
	return out, err
}

func (c *Client) DeleteApp(project, app string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app), nil, nil)
}

func (c *Client) Deploy(project, app, image string) (DeployResult, error) {
	var out DeployResult
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/deploy",
		map[string]string{"image": image}, &out)
	return out, err
}

// DeploySource uploads a source tarball as a multipart form and returns the
// resulting (async) deployment status.
func (c *Client) DeploySource(project, app string, tarball io.Reader) (DeployResult, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("source", "source.tar.gz")
	if err != nil {
		return DeployResult{}, err
	}
	if _, err := io.Copy(part, tarball); err != nil {
		return DeployResult{}, err
	}
	if err := w.Close(); err != nil {
		return DeployResult{}, err
	}

	var out DeployResult
	err = c.doMultipart("POST",
		"/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/deploy",
		w.FormDataContentType(), &buf, &out)
	return out, err
}

// GetDeploy fetches the current status of a deployment.
func (c *Client) GetDeploy(project, app string, id string) (DeployResult, error) {
	var out DeployResult
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+
		"/deploys/"+url.PathEscape(id), nil, &out)
	return out, err
}

// DeployLogs fetches the build log text for a deployment.
func (c *Client) DeployLogs(project, app string, id string) ([]byte, error) {
	return c.doRaw("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+
		"/deploys/"+url.PathEscape(id)+"/logs", nil)
}

// ListDeploys fetches an app's deploy history (newest first) — used to
// resolve a human-facing seq (deploy number) to the internal id the
// rollback API expects.
func (c *Client) ListDeploys(project, app string) ([]DeployInfo, error) {
	var out []DeployInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/deploys", nil, &out)
	return out, err
}

// CreateGitApp registers an app whose source is a git repo, built at deploy time.
func (c *Client) CreateGitApp(project, name string, port int, gitURL, branch, kind, schedule, buildPath string, internal bool, gpu int64) (AppInfo, error) {
	var out AppInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps",
		map[string]any{
			"name": name, "port": port, "git_url": gitURL, "git_branch": branch,
			"kind": kind, "schedule": schedule, "build_path": buildPath, "internal": internal,
			"gpu": gpu,
		}, &out)
	return out, err
}

// Scale posts a partial scale change: nil fields are left unchanged
// server-side. cpu/memory are quantity strings ("250m", "256Mi"); an empty
// string clears (back to unset).
func (c *Client) Scale(project, app string, replicas *int, cpu, memory *string, gpu *int64) error {
	body := map[string]any{}
	if replicas != nil {
		body["replicas"] = *replicas
	}
	if cpu != nil {
		body["cpu"] = *cpu
	}
	if memory != nil {
		body["memory"] = *memory
	}
	if gpu != nil {
		body["gpu"] = *gpu
	}
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/scale",
		body, nil)
}

// Autoscale sets (min, max, cpu all > 0) or clears (all 0) the app's
// autoscaling/v2 HPA parameters.
func (c *Client) Autoscale(project, app string, min, max, cpu int) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/autoscale",
		map[string]any{"min": min, "max": max, "cpu": cpu}, nil)
}

// SetHealth sets (or, with path == "", clears) the app's HTTP health check
// path used for readiness/liveness probes.
func (c *Client) SetHealth(project, app, path string) error {
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/health",
		map[string]string{"path": path}, nil)
}

func (c *Client) EnvSet(project, app, key, value string) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/env",
		map[string]string{"key": key, "value": value}, nil)
}

// EnvPush bulk-upserts env vars from raw .env text; returns count set.
func (c *Client) EnvPush(project, app, dotenv string) (int, error) {
	var out struct {
		Set int `json:"set"`
	}
	err := c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/env/bulk",
		map[string]string{"dotenv": dotenv}, &out)
	return out.Set, err
}

func (c *Client) EnvUnset(project, app, key string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/env/"+url.PathEscape(key), nil, nil)
}

// Redeploy re-rolls an app's current release: a git app rebuilds from its
// repo, any other app re-applies its latest image. Returns the new
// deployment's summary.
func (c *Client) Redeploy(project, app string) (DeployResult, error) {
	var out DeployResult
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/redeploy", nil, &out)
	return out, err
}

// SetGitToken stores a sealed private-repo clone token for a git-source app.
func (c *Client) SetGitToken(project, app, token string) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/git-token",
		map[string]string{"token": token}, nil)
}

// ClearGitToken removes an app's stored git token.
func (c *Client) ClearGitToken(project, app string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/git-token", nil, nil)
}

func (c *Client) EnvList(project, app string) (map[string]string, error) {
	out := make(map[string]string)
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/env", nil, &out)
	return out, err
}

func (c *Client) Raw(project, app string, base bool) ([]byte, error) {
	path := "/v1/projects/" + url.PathEscape(project) + "/apps/" + url.PathEscape(app) + "/raw"
	if base {
		path += "?base=1"
	}
	return c.doRaw("GET", path, nil)
}

func (c *Client) PutOverride(project, app, kind, patchJSON string) error {
	_, err := c.doRaw("PUT",
		"/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/overrides/"+url.PathEscape(kind),
		[]byte(patchJSON))
	return err
}

func (c *Client) DeleteOverride(project, app, kind string) error {
	_, err := c.doRaw("DELETE",
		"/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/overrides/"+url.PathEscape(kind),
		nil)
	return err
}

// stream consumes an SSE endpoint, writing each data payload as one line.
// A terminating "event: end" ends the stream cleanly; its data payload is
// the final status and is not written.
func (c *Client) stream(path string, w io.Writer) error {
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
			return fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ending := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "event: end":
			ending = true
		case strings.HasPrefix(line, "data: "):
			if ending {
				return nil
			}
			fmt.Fprintln(w, line[len("data: "):])
		}
	}
	return sc.Err()
}

// FollowDeployLogs streams a build log's SSE endpoint until the deployment
// finishes, writing each log line to w as it arrives.
func (c *Client) FollowDeployLogs(project, app string, id string, w io.Writer) error {
	return c.stream(fmt.Sprintf("/v1/projects/%s/apps/%s/deploys/%s/logs?follow=1",
		url.PathEscape(project), url.PathEscape(app), url.PathEscape(id)), w)
}

// RuntimeLogs streams (or, with follow=false, fetches once via SSE) the
// app's runtime pod logs. tail > 0 limits to the last N lines; since (a Go
// duration string like "15m") limits to a trailing time window.
func (c *Client) RuntimeLogs(project, app string, follow bool, tail int64, since string, w io.Writer) error {
	p := fmt.Sprintf("/v1/projects/%s/apps/%s/logs", url.PathEscape(project), url.PathEscape(app))
	q := url.Values{}
	if follow {
		q.Set("follow", "1")
	}
	if tail > 0 {
		q.Set("tail", strconv.FormatInt(tail, 10))
	}
	if since != "" {
		q.Set("since", since)
	}
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return c.stream(p, w)
}

type SSHKeyInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	CreatedAt   string `json:"created_at"`
}

func (c *Client) AddSSHKey(name, publicKey string) (string, error) {
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	err := c.do("POST", "/v1/ssh-keys",
		map[string]string{"name": name, "public_key": publicKey}, &out)
	return out.Fingerprint, err
}

func (c *Client) ListSSHKeys() ([]SSHKeyInfo, error) {
	var out []SSHKeyInfo
	err := c.do("GET", "/v1/ssh-keys", nil, &out)
	return out, err
}

func (c *Client) DeleteSSHKey(id int64) error {
	return c.do("DELETE", fmt.Sprintf("/v1/ssh-keys/%d", id), nil, nil)
}

type DomainInfo struct {
	Hostname      string `json:"hostname"`
	CertStatus    string `json:"cert_status"`
	CertError     string `json:"cert_error"`
	CertExpiresAt string `json:"cert_expires_at"`
	DNSWarning    string `json:"dns_warning"`
}

func (c *Client) AddDomain(project, app, hostname string) (DomainInfo, error) {
	var out DomainInfo
	err := c.do("POST", fmt.Sprintf("/v1/projects/%s/apps/%s/domains", project, app),
		map[string]string{"hostname": hostname}, &out)
	return out, err
}

func (c *Client) ListDomains(project, app string) ([]DomainInfo, error) {
	var out []DomainInfo
	err := c.do("GET", fmt.Sprintf("/v1/projects/%s/apps/%s/domains", project, app), nil, &out)
	return out, err
}

func (c *Client) DeleteDomain(project, app, hostname string) error {
	return c.do("DELETE", fmt.Sprintf("/v1/projects/%s/apps/%s/domains/%s", project, app, hostname), nil, nil)
}

func (c *Client) RetryDomain(project, app, hostname string) error {
	return c.do("POST", fmt.Sprintf("/v1/projects/%s/apps/%s/domains/%s/retry", project, app, hostname), nil, nil)
}

// VolumeInfo is one per-app persistent volume as returned by the API.
// Warning is only set on create (Recreate-strategy / not-in-backup notice).
type VolumeInfo struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	SizeGB  int    `json:"size_gb"`
	Warning string `json:"warning,omitempty"`
}

// AddVolume attaches a persistent volume to an app. name may be empty (the
// server defaults it to the last path segment).
func (c *Client) AddVolume(project, app, name, path string, sizeGB int) (VolumeInfo, error) {
	var out VolumeInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/volumes",
		map[string]any{"name": name, "path": path, "size_gb": sizeGB}, &out)
	return out, err
}

func (c *Client) ListVolumes(project, app string) ([]VolumeInfo, error) {
	var out struct {
		Volumes []VolumeInfo `json:"volumes"`
	}
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/volumes", nil, &out)
	return out.Volumes, err
}

// RemoveVolume detaches a volume from an app. With purge, the cluster PVC
// (and its data) is deleted too; without it the PVC is left in place.
func (c *Client) RemoveVolume(project, app, name string, purge bool) error {
	path := "/v1/projects/" + url.PathEscape(project) + "/apps/" + url.PathEscape(app) + "/volumes/" + url.PathEscape(name)
	if purge {
		path += "?purge=true"
	}
	return c.do("DELETE", path, nil, nil)
}

// AddonCreate is CreateAddon's request body: Name/Version/SizeGB default on
// the server when empty/zero. App optionally attaches the new addon to an
// app in the same call (the CLI's `addon add` sugar).
type AddonCreate struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version"`
	SizeGB  int    `json:"size_gb"`
	App     string `json:"app"`
}

type AddonInfo struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Version    string   `json:"version"`
	Status     string   `json:"status,omitempty"`
	Ready      bool     `json:"ready"`
	AttachedTo []string `json:"attached_to"`
	Warning    string   `json:"warning,omitempty"`
}

func (c *Client) CreateAddon(project string, req AddonCreate) (AddonInfo, error) {
	var out AddonInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/addons", req, &out)
	return out, err
}

func (c *Client) ListAddons(project string) ([]AddonInfo, error) {
	var out []AddonInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/addons", nil, &out)
	return out, err
}

// AttachAddon attaches an existing addon to an app, returning the collision
// warning ("" when none — the injected env key was already user-set). The
// server answers 204 (no body) when there's no warning and 200 with a JSON
// body when there is, so this uses doRaw rather than do to avoid decoding
// an empty 204 body as JSON.
func (c *Client) AttachAddon(project, name, app string) (string, error) {
	body, err := json.Marshal(map[string]string{"app": app})
	if err != nil {
		return "", err
	}
	respBody, err := c.doRaw("POST",
		"/v1/projects/"+url.PathEscape(project)+"/addons/"+url.PathEscape(name)+"/attach", body)
	if err != nil {
		return "", err
	}
	if len(respBody) == 0 {
		return "", nil
	}
	var out struct {
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	return out.Warning, nil
}

func (c *Client) DetachAddon(project, name, app string) error {
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/addons/"+url.PathEscape(name)+"/detach",
		map[string]string{"app": app}, nil)
}

// UpgradeAddon re-renders an addon at a new version and applies it (a
// rolling restart). The response carries the manual-migration warning.
func (c *Client) UpgradeAddon(project, name, version string) (AddonInfo, error) {
	var out AddonInfo
	err := c.do("POST",
		"/v1/projects/"+url.PathEscape(project)+"/addons/"+url.PathEscape(name)+"/upgrade",
		map[string]string{"version": version}, &out)
	return out, err
}

func (c *Client) RemoveAddon(project, name string, force, keepData bool) error {
	path := "/v1/projects/" + url.PathEscape(project) + "/addons/" + url.PathEscape(name)
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	if keepData {
		q.Set("keep_data", "1")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do("DELETE", path, nil, nil)
}

// AddonURL fetches an addon's connection URL and the env key it is
// injected as.
func (c *Client) AddonURL(project, name string) (envKey, connURL string, err error) {
	var out struct {
		EnvKey string `json:"env_key"`
		URL    string `json:"url"`
	}
	err = c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/addons/"+url.PathEscape(name)+"/url", nil, &out)
	return out.EnvKey, out.URL, err
}

func (c *Client) GetSetting(key string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := c.do("GET", "/v1/settings/"+key, nil, &out)
	return out.Value, err
}

func (c *Client) SetSetting(key, value string) error {
	return c.do("PUT", "/v1/settings/"+key, map[string]string{"value": value}, nil)
}

// Rollback redeploys a previous deployment's image (deployID == "" auto-picks
// the previous live deployment) and returns the new deployment's per-app
// seq (deploy number) — the human-facing number, not the internal id.
func (c *Client) Rollback(project, app string, deployID string) (int64, error) {
	var out struct {
		DeploymentID string `json:"deployment_id"`
		Seq          int64  `json:"seq"`
	}
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/rollback",
		map[string]string{"deploy_id": deployID}, &out)
	return out.Seq, err
}

// S3Config is a project's external S3 configuration. SecretKey is only
// ever sent, never returned.
type S3Config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
}

// SetProjectS3 stores a project's external S3 credentials (sealed at rest).
func (c *Client) SetProjectS3(project string, cfg S3Config) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/s3", cfg, nil)
}

// GetProjectS3 fetches the stored config (no secret key).
func (c *Client) GetProjectS3(project string) (S3Config, error) {
	var out S3Config
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/s3", nil, &out)
	return out, err
}

// DeleteProjectS3 clears a project's external S3 configuration.
func (c *Client) DeleteProjectS3(project string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/s3", nil, nil)
}

// SetAppS3Env toggles LUNCUR_S3_* env injection for one app.
func (c *Client) SetAppS3Env(project, app string, enabled bool) error {
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/s3",
		map[string]bool{"enabled": enabled}, nil)
}

// RunInfo is one triggered run of a kind=job app.
type RunInfo struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Job        string `json:"job"`
	Nodes      int    `json:"nodes,omitempty"`
	Framework  string `json:"framework,omitempty"`
	ExitCode   *int64 `json:"exit_code,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// CreateRun triggers one run of a kind=job app, optionally overriding its
// stored nodes/framework training defaults for this run only (nodes==0 and
// framework=="" both mean "use the app's default").
func (c *Client) CreateRun(project, app string, nodes int, framework string) (RunInfo, error) {
	var out RunInfo
	body := map[string]any{}
	if nodes != 0 {
		body["nodes"] = nodes
	}
	if framework != "" {
		body["framework"] = framework
	}
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/runs", body, &out)
	return out, err
}

// SetTraining sets a kind=job app's default multi-node run shape: nodes
// (>=1) and an optional framework env preset ("torchrun", "torch", or "" for
// the raw LUNCUR_* contract only). This is the fallback startRun uses when a
// run request doesn't override nodes/framework itself.
func (c *Client) SetTraining(project, app string, nodes int, framework string) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/training",
		map[string]any{"nodes": nodes, "framework": framework}, nil)
}

// ListRuns fetches a job app's run history (newest first).
func (c *Client) ListRuns(project, app string) ([]RunInfo, error) {
	var out []RunInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/runs", nil, &out)
	return out, err
}

// GetRun fetches one run's status.
func (c *Client) GetRun(project, app string, id int64) (RunInfo, error) {
	var out RunInfo
	err := c.do("GET", fmt.Sprintf("/v1/projects/%s/apps/%s/runs/%d", url.PathEscape(project), url.PathEscape(app), id), nil, &out)
	return out, err
}

// FollowRunLogs streams a run's pod logs (SSE) until the stream ends. tail > 0
// limits to the last N lines; since (a Go duration string like "15m") limits
// to a trailing time window.
func (c *Client) FollowRunLogs(project, app string, id int64, follow bool, tail int64, since string, w io.Writer) error {
	p := fmt.Sprintf("/v1/projects/%s/apps/%s/runs/%d/logs", url.PathEscape(project), url.PathEscape(app), id)
	q := url.Values{}
	if follow {
		q.Set("follow", "1")
	}
	if tail > 0 {
		q.Set("tail", strconv.FormatInt(tail, 10))
	}
	if since != "" {
		q.Set("since", since)
	}
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return c.stream(p, w)
}

// SweepTrialInfo is one trial within a hyperparameter sweep, as returned by
// the API. Params is the param set actually assigned to this trial.
type SweepTrialInfo struct {
	ID          string            `json:"id"`
	State       string            `json:"state"`
	Params      map[string]string `json:"params,omitempty"`
	RunID       int64             `json:"run_id,omitempty"`
	MetricValue *float64          `json:"metric_value,omitempty"`
	MetricStep  *int64            `json:"metric_step,omitempty"`
}

// SweepInfo is a hyperparameter sweep as returned by the API. Trials is
// populated by StartSweep/GetSweep/StopSweep; ListSweeps omits it (counts +
// BestValue are enough for a summary row).
type SweepInfo struct {
	ID          string           `json:"id"`
	Status      string           `json:"status"`
	Metric      string           `json:"metric"`
	Direction   string           `json:"direction"`
	MaxTrials   int              `json:"max_trials"`
	Parallel    int              `json:"parallel"`
	EarlyStop   bool             `json:"early_stop"`
	Nodes       int              `json:"nodes,omitempty"`
	Framework   string           `json:"framework,omitempty"`
	Warning     string           `json:"warning,omitempty"`
	CreatedAt   string           `json:"created_at"`
	Counts      map[string]int   `json:"counts"`
	BestTrialID string           `json:"best_trial_id,omitempty"`
	BestValue   *float64         `json:"best_value,omitempty"`
	Truncated   bool             `json:"truncated,omitempty"`
	Trials      []SweepTrialInfo `json:"trials,omitempty"`
}

// StartSweep creates a hyperparameter sweep for a kind=job app: paramsYAML
// is the raw params.yaml contents (grid or random search space), metric is
// the name a trial's run reports via MLflow or the luncur-metric log-line
// contract.
func (c *Client) StartSweep(project, app, paramsYAML, metric, direction string, maxTrials, parallel int, earlyStop bool, nodes int, framework string) (SweepInfo, error) {
	var out SweepInfo
	body := map[string]any{
		"params_yaml": paramsYAML,
		"metric":      metric,
		"direction":   direction,
		"max_trials":  maxTrials,
		"parallel":    parallel,
		"early_stop":  earlyStop,
		"nodes":       nodes,
		"framework":   framework,
	}
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/sweeps", body, &out)
	return out, err
}

// ListSweeps fetches a job app's sweeps (newest first).
func (c *Client) ListSweeps(project, app string) ([]SweepInfo, error) {
	var out []SweepInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/sweeps", nil, &out)
	return out, err
}

// GetSweep fetches one sweep's status plus its full trial list.
func (c *Client) GetSweep(project, app, id string) (SweepInfo, error) {
	var out SweepInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/sweeps/"+url.PathEscape(id), nil, &out)
	return out, err
}

// StopSweep stops a running sweep; idempotent — a second call is a no-op.
func (c *Client) StopSweep(project, app, id string) (SweepInfo, error) {
	var out SweepInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/sweeps/"+url.PathEscape(id)+"/stop", nil, &out)
	return out, err
}

// PipelineLastRun is the newest-run summary embedded in a ListPipelines row.
type PipelineLastRun struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
}

// PipelineInfo is a stored pipeline as returned by the API. YAML is present
// on create/update/get responses; ListPipelines rows omit it in favor of
// LastRun.
type PipelineInfo struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Engine    string           `json:"engine,omitempty"`
	Cron      string           `json:"cron,omitempty"`
	YAML      string           `json:"yaml,omitempty"`
	CreatedAt string           `json:"created_at"`
	LastRun   *PipelineLastRun `json:"last_run,omitempty"`
}

// PipelineStepInfo is one step row within a pipeline run, as returned by the
// API. StartedAt/FinishedAt are SQLite's datetime('now') format
// ("2006-01-02 15:04:05", UTC) when set.
type PipelineStepInfo struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Attempt    int    `json:"attempt"`
	Detail     string `json:"detail,omitempty"`
	JobRunID   int64  `json:"job_run_id,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// PipelineRunInfo is one pipeline run, as returned by the API. Steps is
// populated by StartPipelineRun/GetPipelineRun/StopPipelineRun;
// ListPipelineRuns omits it (a run history listing doesn't need per-step
// detail).
type PipelineRunInfo struct {
	ID         string             `json:"id"`
	PipelineID string             `json:"pipeline_id"`
	Status     string             `json:"status"`
	Trigger    string             `json:"trigger"`
	Warning    string             `json:"warning,omitempty"`
	StartedAt  string             `json:"started_at"`
	FinishedAt string             `json:"finished_at,omitempty"`
	Steps      []PipelineStepInfo `json:"steps,omitempty"`
}

// CreatePipeline compiles+validates and stores a pipeline.yaml. yamlStr is
// the raw file contents; engine is "" (follow the install's pipeline_engine
// setting), "native", or "argo"; cron is "" (manual trigger only) or a
// 5-field cron expression (validated server-side).
//
// Step env in pipeline.yaml is stored in plaintext — put secrets in app env
// instead.
func (c *Client) CreatePipeline(project, name, yamlStr, engine, cron string) (PipelineInfo, error) {
	var out PipelineInfo
	body := map[string]any{"name": name, "yaml": yamlStr, "engine": engine, "cron": cron}
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/pipelines", body, &out)
	return out, err
}

// UpdatePipeline replaces a pipeline's yaml/engine/cron; a nil pointer
// leaves that field's current value unchanged (an empty non-nil cron clears
// the schedule).
func (c *Client) UpdatePipeline(project, name string, yamlStr, engine, cron *string) (PipelineInfo, error) {
	var out PipelineInfo
	body := map[string]any{}
	if yamlStr != nil {
		body["yaml"] = *yamlStr
	}
	if engine != nil {
		body["engine"] = *engine
	}
	if cron != nil {
		body["cron"] = *cron
	}
	err := c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name), body, &out)
	return out, err
}

// ListPipelines fetches a project's pipelines, name ascending.
func (c *Client) ListPipelines(project string) ([]PipelineInfo, error) {
	var out []PipelineInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/pipelines", nil, &out)
	return out, err
}

// GetPipeline fetches one pipeline's detail, including its yaml.
func (c *Client) GetPipeline(project, name string) (PipelineInfo, error) {
	var out PipelineInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name), nil, &out)
	return out, err
}

// DeletePipeline deletes a pipeline; the API 409s (pipeline_busy) if a run is
// still in progress.
func (c *Client) DeletePipeline(project, name string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name), nil, nil)
}

// StartPipelineRun manually triggers a pipeline run; its root steps are
// already launching by the time this returns (the server fires one
// orchestrator tick inline before responding).
func (c *Client) StartPipelineRun(project, name string) (PipelineRunInfo, error) {
	var out PipelineRunInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name)+"/runs", nil, &out)
	return out, err
}

// ListPipelineRuns fetches a pipeline's run history (newest first, capped at
// 50 by the server).
func (c *Client) ListPipelineRuns(project, name string) ([]PipelineRunInfo, error) {
	var out []PipelineRunInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name)+"/runs", nil, &out)
	return out, err
}

// GetPipelineRun fetches one run plus its steps in topo order.
func (c *Client) GetPipelineRun(project, name, id string) (PipelineRunInfo, error) {
	var out PipelineRunInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name)+"/runs/"+url.PathEscape(id), nil, &out)
	return out, err
}

// StopPipelineRun stops a running pipeline run; idempotent — a second call
// is a no-op that just reports current state.
func (c *Client) StopPipelineRun(project, name, id string) (PipelineRunInfo, error) {
	var out PipelineRunInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name)+"/runs/"+url.PathEscape(id)+"/stop", nil, &out)
	return out, err
}

// PipelineWebhookSecret generates (or, if one already exists, rotates) a
// pipeline's trigger webhook secret. The returned secret is shown ONLY in
// this response — it is never recoverable from the store afterward.
func (c *Client) PipelineWebhookSecret(project, name string) (url_, secret string, err error) {
	var out struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	err = c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name)+"/webhook-secret", nil, &out)
	return out.URL, out.Secret, err
}

// DisablePipelineWebhook clears a pipeline's trigger webhook secret; any
// previously-issued secret stops verifying.
func (c *Client) DisablePipelineWebhook(project, name string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/pipelines/"+url.PathEscape(name)+"/webhook-secret", nil, nil)
}

type TokenInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
	ExpiresAt  string `json:"expires_at"`
}

func (c *Client) ListTokens() ([]TokenInfo, error) {
	var out []TokenInfo
	err := c.do("GET", "/v1/tokens", nil, &out)
	return out, err
}

func (c *Client) RevokeToken(id int64) error {
	return c.do("DELETE", fmt.Sprintf("/v1/tokens/%d", id), nil, nil)
}

// InviteInfo is one registration invite as returned by the API.
type InviteInfo struct {
	Token     string `json:"token"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
	Path      string `json:"path"`
	Used      bool   `json:"used"`
	Emailed   bool   `json:"emailed"`
	Warning   string `json:"warning"`
}

func (c *Client) CreateInvite(role, email string) (InviteInfo, error) {
	var out InviteInfo
	body := map[string]string{"role": role}
	if email != "" {
		body["email"] = email
	}
	err := c.do("POST", "/v1/invites", body, &out)
	return out, err
}

func (c *Client) ListInvites() ([]InviteInfo, error) {
	var out []InviteInfo
	err := c.do("GET", "/v1/invites", nil, &out)
	return out, err
}

func (c *Client) RevokeInvite(token string) error {
	return c.do("DELETE", "/v1/invites/"+token, nil, nil)
}

// BackupInfo is one backup archive as returned by the API.
type BackupInfo struct {
	ID        int64    `json:"id"`
	Path      string   `json:"path"`
	SizeBytes int64    `json:"size_bytes"`
	Uploaded  bool     `json:"uploaded"`
	CreatedAt string   `json:"created_at"`
	Warnings  []string `json:"warnings"`
}

func (c *Client) CreateBackup(noUpload bool) (BackupInfo, error) {
	var out BackupInfo
	err := c.do("POST", "/v1/backups", map[string]bool{"no_upload": noUpload}, &out)
	return out, err
}

func (c *Client) ListBackups() ([]BackupInfo, error) {
	var out []BackupInfo
	err := c.do("GET", "/v1/backups", nil, &out)
	return out, err
}

func (c *Client) PruneBackups() (int, error) {
	var out struct {
		Removed int `json:"removed"`
	}
	err := c.do("POST", "/v1/backups/prune", nil, &out)
	return out.Removed, err
}

// EjectApp detaches project/app from luncur's management, one-way: the
// server renders and archives the app's final manifest and refuses every
// mutation on it from then on. yaml is the rendered manifest; savedTo is
// the server-side archive path (empty when the server has no data dir).
func (c *Client) EjectApp(project, app string) (yaml, savedTo string, err error) {
	var out struct {
		YAML    string `json:"yaml"`
		SavedTo string `json:"saved_to"`
	}
	err = c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/eject", nil, &out)
	return out.YAML, out.SavedTo, err
}

// AdoptApp reverses eject: luncur reclaims management of project/app and
// re-applies its rendered state. warning is non-empty when the flag was
// cleared but the re-apply failed.
func (c *Client) AdoptApp(project, app string) (string, error) {
	var out struct {
		Warning string `json:"warning"`
	}
	err := c.do("POST",
		"/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/adopt", nil, &out)
	return out.Warning, err
}

// WebhookInfo is a git-source app's deploy webhook state. Secret is only
// ever populated by WebhookEnable's response — it is never recoverable
// afterward, only the sealed bytes persist server-side.
type WebhookInfo struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path"`
	Secret  string `json:"secret,omitempty"`
}

// WebhookEnable turns on (or, if already enabled, rotates) an app's deploy
// webhook. The returned secret is shown ONLY in this response.
func (c *Client) WebhookEnable(project, app string) (WebhookInfo, error) {
	var out WebhookInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/webhook", nil, &out)
	return out, err
}

// WebhookShow reports whether an app's webhook is enabled and its path
// (never its secret).
func (c *Client) WebhookShow(project, app string) (WebhookInfo, error) {
	var out WebhookInfo
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/webhook", nil, &out)
	return out, err
}

// WebhookDisable turns off an app's deploy webhook.
func (c *Client) WebhookDisable(project, app string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/webhook", nil, nil)
}

// RegistryGCReport is one registry GC sweep's result, as returned by POST
// /v1/registry/gc.
type RegistryGCReport struct {
	DeletedManifests int      `json:"deleted_manifests"`
	BytesReclaimed   int64    `json:"bytes_reclaimed"`
	Warnings         []string `json:"warnings"`
}

func (c *Client) RegistryGC() (RegistryGCReport, error) {
	var out RegistryGCReport
	err := c.do("POST", "/v1/registry/gc", nil, &out)
	return out, err
}

// AuditEntry is one recorded mutating request, as returned by GET /v1/audit.
type AuditEntry struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	UserEmail string `json:"user_email"`
	Action    string `json:"action"`
	Target    string `json:"target"`
}

// DoctorCheck is one named diagnostic result from GET /v1/doctor.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Doctor runs the server's one-shot diagnostics (admin only).
func (c *Client) Doctor() (serverVersion string, checks []DoctorCheck, err error) {
	var out struct {
		ServerVersion string        `json:"server_version"`
		Checks        []DoctorCheck `json:"checks"`
	}
	err = c.do("GET", "/v1/doctor", nil, &out)
	return out.ServerVersion, out.Checks, err
}

// Node is one cluster node as returned by GET /v1/nodes.
type Node struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Ready       bool   `json:"ready"`
	IP          string `json:"ip"`
	Version     string `json:"version"`
	CPUCapMilli int64  `json:"cpu_capacity_millicores"`
	CPUMilli    int64  `json:"cpu_used_millicores"`
	MemCapMiB   int64  `json:"memory_capacity_mib"`
	MemMiB      int64  `json:"memory_used_mib"`
	GPU         bool   `json:"gpu"`
	GPUCapacity int64  `json:"gpu_capacity"`
	MetricsOK   bool   `json:"metrics_available"`
}

// ListNodes fetches every cluster node (admin only).
func (c *Client) ListNodes() ([]Node, error) {
	var out struct {
		Nodes []Node `json:"nodes"`
	}
	err := c.do("GET", "/v1/nodes", nil, &out)
	return out.Nodes, err
}

// Pod is one running pod as returned by GET .../apps/{app}/pods.
type Pod struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Reason    string `json:"reason"`
	Ready     bool   `json:"ready"`
	Restarts  int32  `json:"restarts"`
	Node      string `json:"node"`
	StartedAt string `json:"started_at"`
	CPUMilli  int64  `json:"cpu_millicores"`
	MemoryMiB int64  `json:"memory_mib"`
	MetricsOK bool   `json:"metrics_available"`
}

// AppPods fetches an app's live pods with per-pod usage.
func (c *Client) AppPods(project, app string) ([]Pod, error) {
	var out struct {
		Pods []Pod `json:"pods"`
	}
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/pods", nil, &out)
	return out.Pods, err
}

// MetricSample is one point of an app's live usage history.
type MetricSample struct {
	At            string `json:"at"`
	CPUMillicores int64  `json:"cpu_millicores"`
	MemoryMiB     int64  `json:"memory_mib"`
}

// MetricsHistory fetches an app's sampled usage (last ~30 minutes).
func (c *Client) MetricsHistory(project, app string) ([]MetricSample, error) {
	var out struct {
		Samples []MetricSample `json:"samples"`
	}
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/metrics/history", nil, &out)
	return out.Samples, err
}

// AuditList fetches the audit log (admin only), newest first. limit <= 0
// leaves it unset (the server defaults/caps it); user/contains filter by
// exact email and by substring match respectively, both optional.
func (c *Client) AuditList(limit int, user, contains string) ([]AuditEntry, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if user != "" {
		q.Set("user", user)
	}
	if contains != "" {
		q.Set("contains", contains)
	}
	path := "/v1/audit"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Entries []AuditEntry `json:"entries"`
	}
	err := c.do("GET", path, nil, &out)
	return out.Entries, err
}

// SystemUpdate asks the server to roll itself to a new image; returns
// the image it accepted.
func (c *Client) SystemUpdate(version, image string) (string, error) {
	var out struct {
		Image string `json:"image"`
	}
	err := c.do("POST", "/v1/system/update", map[string]string{"version": version, "image": image}, &out)
	return out.Image, err
}

// ArgoInstall installs the pinned Argo Workflows controller (no argo-server
// UI) via the admin-only system endpoint; returns the installed version.
func (c *Client) ArgoInstall() (string, error) {
	var out struct {
		Installed bool   `json:"installed"`
		Version   string `json:"version"`
	}
	err := c.do("POST", "/v1/system/argo-install", nil, &out)
	return out.Version, err
}

// GPUOffer is one rentable machine from the GPU marketplace search.
type GPUOffer struct {
	ID          int64   `json:"id"`
	GPUName     string  `json:"gpu_name"`
	NumGPUs     int     `json:"num_gpus"`
	DPHTotal    float64 `json:"dph_total"`
	DiskSpace   float64 `json:"disk_space"`
	Geolocation string  `json:"geolocation"`
}

// GPUInstance is one rented GPU VM tracked by the server. ExternalID is the
// provider's contract/instance ref, a string since Nebius refs
// ("computeinstance-…") aren't numeric like vast.ai's.
type GPUInstance struct {
	ID             int64   `json:"id"`
	Provider       string  `json:"provider"`
	ExternalID     string  `json:"external_id"`
	Label          string  `json:"label"`
	GPUName        string  `json:"gpu_name"`
	NumGPUs        int     `json:"num_gpus"`
	Status         string  `json:"status"`
	ProviderStatus string  `json:"provider_status"`
	DPHTotal       float64 `json:"dph_total"`
	CreatedAt      string  `json:"created_at"`
}

// SetGPUKey stores the vast.ai API key (sealed server-side). provider
// defaults to "vastai" server-side when passed empty.
func (c *Client) SetGPUKey(provider, apiKey string) error {
	return c.do("PUT", "/v1/gpu/key", map[string]string{"provider": provider, "api_key": apiKey}, nil)
}

// SetNebiusCreds stores the Nebius service-account credentials (sealed
// server-side). privateKey is the raw PEM bytes; the caller never logs them.
func (c *Client) SetNebiusCreds(saID, pubkeyID string, privateKey []byte, parentID, subnetID string) error {
	return c.do("PUT", "/v1/gpu/key", map[string]string{
		"provider":    "nebius",
		"sa_id":       saID,
		"pubkey_id":   pubkeyID,
		"private_key": string(privateKey),
		"parent_id":   parentID,
		"subnet_id":   subnetID,
	}, nil)
}

// GPUOffers searches rentable VM offers, cheapest first.
func (c *Client) GPUOffers(gpuName string, numGPUs, limit int) ([]GPUOffer, error) {
	q := url.Values{}
	if gpuName != "" {
		q.Set("gpu_name", gpuName)
	}
	if numGPUs > 0 {
		q.Set("num_gpus", fmt.Sprint(numGPUs))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out struct {
		Offers []GPUOffer `json:"offers"`
	}
	err := c.do("GET", "/v1/gpu/offers?"+q.Encode(), nil, &out)
	return out.Offers, err
}

// GPURentReq is RentGPU's request: Provider selects vast.ai (OfferID) or
// Nebius (Platform + Preset); GPUName/NumGPUs are recorded for the panel on
// either provider.
type GPURentReq struct {
	Provider string
	OfferID  int64
	Platform string
	Preset   string
	DiskGB   int
	GPUName  string
	NumGPUs  int
}

// RentGPU accepts an offer (vast.ai) or platform/preset (Nebius) as a
// cluster-joining VM.
func (c *Client) RentGPU(req GPURentReq) (GPUInstance, error) {
	var out GPUInstance
	err := c.do("POST", "/v1/gpu/instances", map[string]any{
		"provider": req.Provider, "offer_id": req.OfferID, "platform": req.Platform,
		"preset": req.Preset, "disk_gb": req.DiskGB, "gpu_name": req.GPUName, "num_gpus": req.NumGPUs,
	}, &out)
	return out, err
}

// ListGPUInstances lists tracked GPU instances.
func (c *Client) ListGPUInstances() ([]GPUInstance, error) {
	var out []GPUInstance
	err := c.do("GET", "/v1/gpu/instances", nil, &out)
	return out, err
}

// DestroyGPUInstance deletes a rented VM (billing stops, data gone).
func (c *Client) DestroyGPUInstance(id int64) error {
	return c.do("DELETE", fmt.Sprintf("/v1/gpu/instances/%d", id), nil, nil)
}

// SetProjectGPUQuota sets a project's GPU budget; 0 = unlimited.
func (c *Client) SetProjectGPUQuota(project string, quota int64) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/gpu-quota", map[string]int64{"quota": quota}, nil)
}

// SetProjectQuota sets a project's namespace CPU/memory budget; 0 for either
// means unlimited for that resource.
func (c *Client) SetProjectQuota(project string, cpuMilli, memMB int64) error {
	return c.do("PUT", "/v1/projects/"+url.PathEscape(project)+"/quota", map[string]int64{
		"cpu_milli": cpuMilli, "memory_mb": memMB,
	}, nil)
}
