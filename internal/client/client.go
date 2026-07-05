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
	var body *bytes.Buffer = bytes.NewBuffer(nil)
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
	Seq         int64  `json:"seq,omitempty"`
}

type DeployResult struct {
	DeploymentID int64  `json:"deployment_id"`
	Seq          int64  `json:"seq"`
	Status       string `json:"status"`
	URL          string `json:"url"`
}

// DeployInfo is one row of an app's deploy history, as returned by
// ListDeploys — Seq is the per-app human-facing deploy number; ID is the
// internal id the rollback API still expects.
type DeployInfo struct {
	ID             int64  `json:"id"`
	Seq            int64  `json:"seq"`
	Status         string `json:"status"`
	Image          string `json:"image"`
	CreatedAt      string `json:"created_at"`
	RolledBackFrom int64  `json:"rolled_back_from,omitempty"`
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

func (c *Client) AddMember(project, email string) error {
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/members",
		map[string]string{"email": email}, nil)
}

func (c *Client) CreateApp(project, name string, port int, kind, schedule, buildPath string, internal bool) (AppInfo, error) {
	var out AppInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps",
		map[string]interface{}{"name": name, "port": port, "kind": kind, "schedule": schedule, "build_path": buildPath, "internal": internal}, &out)
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
func (c *Client) GetDeploy(project, app string, id int64) (DeployResult, error) {
	var out DeployResult
	err := c.do("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+
		"/deploys/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// DeployLogs fetches the build log text for a deployment.
func (c *Client) DeployLogs(project, app string, id int64) ([]byte, error) {
	return c.doRaw("GET", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+
		"/deploys/"+strconv.FormatInt(id, 10)+"/logs", nil)
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
func (c *Client) CreateGitApp(project, name string, port int, gitURL, branch, kind, schedule, buildPath string, internal bool) (AppInfo, error) {
	var out AppInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps",
		map[string]any{
			"name": name, "port": port, "git_url": gitURL, "git_branch": branch,
			"kind": kind, "schedule": schedule, "build_path": buildPath, "internal": internal,
		}, &out)
	return out, err
}

// Scale posts a partial scale change: nil fields are left unchanged
// server-side. cpu/memory are quantity strings ("250m", "256Mi"); an empty
// string clears (back to unset).
func (c *Client) Scale(project, app string, replicas *int, cpu, memory *string) error {
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
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/scale",
		body, nil)
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

func (c *Client) EnvUnset(project, app, key string) error {
	return c.do("DELETE", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/env/"+url.PathEscape(key), nil, nil)
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
func (c *Client) FollowDeployLogs(project, app string, id int64, w io.Writer) error {
	return c.stream(fmt.Sprintf("/v1/projects/%s/apps/%s/deploys/%d/logs?follow=1",
		url.PathEscape(project), url.PathEscape(app), id), w)
}

// RuntimeLogs streams (or, with follow=false, fetches once via SSE) the
// app's runtime pod logs.
func (c *Client) RuntimeLogs(project, app string, follow bool, w io.Writer) error {
	p := fmt.Sprintf("/v1/projects/%s/apps/%s/logs", url.PathEscape(project), url.PathEscape(app))
	if follow {
		p += "?follow=1"
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

// Rollback redeploys a previous deployment's image (deployID == 0 auto-picks
// the previous live deployment) and returns the new deployment's per-app
// seq (deploy number) — the human-facing number, not the internal id.
func (c *Client) Rollback(project, app string, deployID int64) (int64, error) {
	var out struct {
		DeploymentID int64 `json:"deployment_id"`
		Seq          int64 `json:"seq"`
	}
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/rollback",
		map[string]int64{"deploy_id": deployID}, &out)
	return out.Seq, err
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
