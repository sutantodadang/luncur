// Package s3 is a minimal S3-compatible client (SigV4, path-style) for
// backup uploads — stdlib only, works with AWS/R2/minio/B2.
package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/awssig"
)

type Client struct {
	Endpoint  string // scheme+host, e.g. https://s3.us-east-1.amazonaws.com
	Region    string // default us-east-1
	Bucket    string
	AccessKey string
	SecretKey string

	HTTPClient *http.Client     // default http.DefaultClient
	Now        func() time.Time // default time.Now (injectable in tests)
}

func (c *Client) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) url(key string) string {
	return strings.TrimRight(c.Endpoint, "/") + "/" + c.Bucket + "/" + strings.TrimLeft(key, "/")
}

func (c *Client) send(ctx context.Context, method, key string, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, method, c.url(key), body)
	if err != nil {
		return err
	}
	if size > 0 {
		req.ContentLength = size
	}
	sign(req, c.AccessKey, c.SecretKey, c.region(), c.now().UTC())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 %s %s: %d %s", method, key, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Put uploads one object. The payload is not hashed (UNSIGNED-PAYLOAD), so
// body can stream without buffering.
func (c *Client) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	return c.send(ctx, http.MethodPut, key, body, size)
}

func (c *Client) Delete(ctx context.Context, key string) error {
	return c.send(ctx, http.MethodDelete, key, nil, 0)
}

// Get downloads one object. The caller must close the returned body. Error
// responses (>=300) are drained and surfaced like send's.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(key), nil)
	if err != nil {
		return nil, err
	}
	sign(req, c.AccessKey, c.SecretKey, c.region(), c.now().UTC())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 GET %s: %d %s", key, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// sign delegates to the shared SigV4 signer with S3's UNSIGNED-PAYLOAD.
func sign(req *http.Request, accessKey, secretKey, region string, t time.Time) {
	awssig.Sign(req, accessKey, secretKey, region, "s3", "UNSIGNED-PAYLOAD", t)
}
