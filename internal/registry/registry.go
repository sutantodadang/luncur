// Package registry is a minimal client for luncur's embedded Docker
// registry's plain-HTTP v2 API, plus the pure retention function registry
// GC uses to decide which manifests survive a sweep.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Client talks to the embedded registry over plain HTTP (no TLS, no auth)
// — the same style internal/server's imageInRegistry probe uses, since
// luncur's registry is only ever reachable in-cluster.
type Client struct {
	Host       string // host:port
	HTTPClient *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// Repositories lists every repository in the registry's catalog.
func (c *Client) Repositories(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.Host+"/v2/_catalog", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: unexpected status %d", resp.StatusCode)
	}
	var out struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Repositories, nil
}

// Tags lists a repository's tags. A 404 (unknown repository) is not an
// error — it just means no tags.
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+c.Host+"/v2/"+repo+"/tags/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tags %s: unexpected status %d", repo, resp.StatusCode)
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Tags, nil
}

// Digest HEAD-checks a tag's manifest and returns its content digest, the
// identifier DeleteManifest deletes by.
func (c *Client) Digest(ctx context.Context, repo, tag string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		"http://"+c.Host+"/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept",
		"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("digest %s:%s: unexpected status %d", repo, tag, resp.StatusCode)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("digest %s:%s: no Docker-Content-Digest header", repo, tag)
	}
	return digest, nil
}

// DeleteManifest removes a manifest by digest. A 404 (already gone) is not
// an error — deletes must be idempotent.
func (c *Client) DeleteManifest(ctx context.Context, repo, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://"+c.Host+"/v2/"+repo+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete %s@%s: unexpected status %d", repo, digest, resp.StatusCode)
	}
	return nil
}

// DeployRef is one deployment row's registry image, carrying the retention
// signals KeepTags needs. Callers pass refs newest-first per app.
type DeployRef struct {
	Repo, Tag    string
	Live, Newest bool
}

// KeepTags computes the retention keep-set: per repository, the first
// `keep` refs (in the caller's newest-first order) are kept, plus every
// Live or Newest ref regardless of position. Returns repo -> tag -> true
// for kept tags only (absent means "not kept").
func KeepTags(refs []DeployRef, keep int) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	position := map[string]int{} // repo -> refs seen so far for that repo
	for _, ref := range refs {
		keepThis := ref.Live || ref.Newest || position[ref.Repo] < keep
		position[ref.Repo]++
		if !keepThis {
			continue
		}
		if out[ref.Repo] == nil {
			out[ref.Repo] = map[string]bool{}
		}
		out[ref.Repo][ref.Tag] = true
	}
	return out
}
