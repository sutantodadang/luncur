package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestReleaseTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
	}))
	defer srv.Close()

	tag, err := latestReleaseTag(srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v1.2.3" {
		t.Fatalf("tag = %q, want v1.2.3", tag)
	}
}

func TestLatestReleaseTagEmptyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":""}`)
	}))
	defer srv.Close()

	_, err := latestReleaseTag(srv.Client(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "no tag_name") {
		t.Fatalf("err = %v, want no tag_name error", err)
	}
}
