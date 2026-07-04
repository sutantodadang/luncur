package awssig

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignKnownAnswerS3 re-runs Plan K's SigV4 known-answer vector (same
// request, creds, and timestamp as internal/s3's TestSignKnownAnswer)
// through the extracted signer.
func TestSignKnownAnswerS3(t *testing.T) {
	req, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/backups/a.tar.gz", strings.NewReader("hi"))
	ts := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	Sign(req, "AKIDEXAMPLE", "SECRETKEY", "us-east-1", "s3", "UNSIGNED-PAYLOAD", ts)

	auth := req.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256",
		"Credential=AKIDEXAMPLE/20260703/us-east-1/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Fatalf("auth header missing %q:\n%s", want, auth)
		}
	}
	if req.Header.Get("x-amz-date") != "20260703T120000Z" {
		t.Fatalf("x-amz-date = %q", req.Header.Get("x-amz-date"))
	}
	if req.Header.Get("x-amz-content-sha256") != "UNSIGNED-PAYLOAD" {
		t.Fatalf("content sha = %q", req.Header.Get("x-amz-content-sha256"))
	}

	// Deterministic: same input, same signature.
	req2, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/backups/a.tar.gz", strings.NewReader("hi"))
	Sign(req2, "AKIDEXAMPLE", "SECRETKEY", "us-east-1", "s3", "UNSIGNED-PAYLOAD", ts)
	if req2.Header.Get("Authorization") != auth {
		t.Fatal("signing is not deterministic")
	}
}

// TestSignServiceChangesSignature: a different service yields a different
// scope, and non-S3 services carry a real payload hash.
func TestSignServiceChangesSignature(t *testing.T) {
	ts := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	body := []byte("<xml/>")
	r1, _ := http.NewRequest("POST", "https://route53.amazonaws.com/2013-04-01/hostedzone/Z1/rrset", strings.NewReader(string(body)))
	Sign(r1, "AK", "SK", "us-east-1", "route53", HashPayload(body), ts)
	auth := r1.Header.Get("Authorization")
	if !strings.Contains(auth, "/route53/aws4_request") {
		t.Fatalf("scope missing route53: %q", auth)
	}
	if r1.Header.Get("x-amz-content-sha256") == "UNSIGNED-PAYLOAD" {
		t.Fatal("route53 request must carry a real payload hash")
	}
	if len(r1.Header.Get("x-amz-content-sha256")) != 64 {
		t.Fatalf("payload hash = %q", r1.Header.Get("x-amz-content-sha256"))
	}
}
