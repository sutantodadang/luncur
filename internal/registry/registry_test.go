package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeepTags(t *testing.T) {
	t.Run("first keep kept plus live/newest regardless of position", func(t *testing.T) {
		var refs []DeployRef
		for i := 0; i < 12; i++ {
			refs = append(refs, DeployRef{Repo: "proj/web", Tag: tagName(i)})
		}
		refs[10].Live = true // 11th ref (index 10), outside the first 10 by position

		got := KeepTags(refs, 10)
		for i := 0; i < 10; i++ {
			if !got["proj/web"][tagName(i)] {
				t.Fatalf("tag %d should be kept (within first 10)", i)
			}
		}
		if !got["proj/web"][tagName(10)] {
			t.Fatal("tag 10 (Live) should be kept despite position 11")
		}
		if got["proj/web"][tagName(11)] {
			t.Fatal("tag 11 should be dropped")
		}
	})

	t.Run("repos independent", func(t *testing.T) {
		refs := []DeployRef{
			{Repo: "a", Tag: "1"},
			{Repo: "a", Tag: "2"},
			{Repo: "b", Tag: "1"},
		}
		got := KeepTags(refs, 1)
		if !got["a"]["1"] || got["a"]["2"] {
			t.Fatalf("repo a = %v, want only tag 1 kept", got["a"])
		}
		if !got["b"]["1"] {
			t.Fatalf("repo b = %v, want tag 1 kept", got["b"])
		}
	})

	t.Run("newest always kept", func(t *testing.T) {
		refs := []DeployRef{
			{Repo: "c", Tag: "1"},
			{Repo: "c", Tag: "2"},
			{Repo: "c", Tag: "3", Newest: true},
		}
		got := KeepTags(refs, 0)
		if !got["c"]["3"] {
			t.Fatal("Newest tag should be kept even with keep=0")
		}
		if got["c"]["1"] || got["c"]["2"] {
			t.Fatalf("non-newest, non-live tags should be dropped with keep=0: %v", got["c"])
		}
	})
}

func tagName(i int) string {
	return string(rune('a' + i))
}

// TestClientAgainstFake exercises Client's four calls against an httptest
// fake that mirrors the registry v2 API shapes: _catalog, tags/list, HEAD
// manifest returning Docker-Content-Digest, DELETE recording the digest.
func TestClientAgainstFake(t *testing.T) {
	var deleted []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"repositories":["proj/web","proj/api"]}`))
	})
	mux.HandleFunc("/v2/proj/web/tags/list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tags":["1","2"]}`))
	})
	mux.HandleFunc("/v2/proj/missing/tags/list", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/v2/proj/web/manifests/", func(w http.ResponseWriter, r *http.Request) {
		ref := strings.TrimPrefix(r.URL.Path, "/v2/proj/web/manifests/")
		switch r.Method {
		case http.MethodHead:
			if ref != "1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if accept := r.Header.Get("Accept"); !strings.Contains(accept, "manifest") {
				t.Errorf("Digest: missing manifest Accept header, got %q", accept)
			}
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if ref == "sha256:doesnotexist" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			deleted = append(deleted, ref)
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{Host: strings.TrimPrefix(srv.URL, "http://")}
	ctx := context.Background()

	repos, err := c.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("Repositories = %v, want 2 entries", repos)
	}

	tags, err := c.Tags(ctx, "proj/web")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "1" || tags[1] != "2" {
		t.Fatalf("Tags = %v, want [1 2]", tags)
	}

	// 404 (unknown repository) -> empty, nil error.
	empty, err := c.Tags(ctx, "proj/missing")
	if err != nil {
		t.Fatalf("Tags 404: unexpected error %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("Tags 404 = %v, want empty", empty)
	}

	digest, err := c.Digest(ctx, "proj/web", "1")
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:abc" {
		t.Fatalf("Digest = %q, want sha256:abc", digest)
	}

	if err := c.DeleteManifest(ctx, "proj/web", digest); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != digest {
		t.Fatalf("deleted = %v, want [%s]", deleted, digest)
	}

	// 404 on delete (already gone) is not an error.
	if err := c.DeleteManifest(ctx, "proj/web", "sha256:doesnotexist"); err != nil {
		t.Fatalf("DeleteManifest 404 should not error: %v", err)
	}
}
