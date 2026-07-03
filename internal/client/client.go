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
	var out struct{ Token string `json:"token"` }
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
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Replicas int    `json:"replicas"`
	URL      string `json:"url"`
	Status   string `json:"status,omitempty"`
	Image    string `json:"image,omitempty"`
}

type DeployResult struct {
	DeploymentID int64  `json:"deployment_id"`
	Status       string `json:"status"`
	URL          string `json:"url"`
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

func (c *Client) CreateApp(project, name string, port int) (AppInfo, error) {
	var out AppInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps",
		map[string]interface{}{"name": name, "port": port}, &out)
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

// CreateGitApp registers an app whose source is a git repo, built at deploy time.
func (c *Client) CreateGitApp(project, name string, port int, gitURL, branch string) (AppInfo, error) {
	var out AppInfo
	err := c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps",
		map[string]any{"name": name, "port": port, "git_url": gitURL, "git_branch": branch}, &out)
	return out, err
}

func (c *Client) Scale(project, app string, replicas int) error {
	return c.do("POST", "/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/scale",
		map[string]int{"replicas": replicas}, nil)
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
