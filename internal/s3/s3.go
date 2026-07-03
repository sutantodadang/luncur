// Package s3 is a minimal S3-compatible client (SigV4, path-style) for
// backup uploads — stdlib only, works with AWS/R2/minio/B2.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sign implements AWS Signature Version 4 for a single-chunk request with
// an unsigned payload. Signed headers: host, x-amz-content-sha256,
// x-amz-date.
func sign(req *http.Request, accessKey, secretKey, region string, t time.Time) {
	const service = "s3"
	amzDate := t.Format("20060102T150405Z")
	shortDate := t.Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:UNSIGNED-PAYLOAD\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	scope := strings.Join([]string{shortDate, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256hex(canonicalRequest),
	}, "\n")

	signingKey := hmacSHA256(
		hmacSHA256(
			hmacSHA256(
				hmacSHA256([]byte("AWS4"+secretKey), []byte(shortDate)),
				[]byte(region)),
			[]byte(service)),
		[]byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature))
}
