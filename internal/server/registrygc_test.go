package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

// newFakeRegistryServer serves the registry v2 API shapes runRegistryGC
// depends on: _catalog from the given repo->tags map, tags/list per repo,
// HEAD manifest returning a deterministic digest, and DELETE recording
// "<repo>@<digest>" into the returned slice.
func newFakeRegistryServer(t *testing.T, catalog map[string][]string) (host string, deleted *[]string) {
	t.Helper()
	deleted = &[]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, r *http.Request) {
		var repos []string
		for repo := range catalog {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		json.NewEncoder(w).Encode(map[string][]string{"repositories": repos})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/")
		if idx := strings.Index(path, "/tags/list"); idx >= 0 {
			repo := path[:idx]
			tags, ok := catalog[repo]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string][]string{"tags": tags})
			return
		}
		if idx := strings.Index(path, "/manifests/"); idx >= 0 {
			repo := path[:idx]
			ref := path[idx+len("/manifests/"):]
			switch r.Method {
			case http.MethodHead:
				w.Header().Set("Docker-Content-Digest", "sha256:"+strings.ReplaceAll(repo, "/", "-")+"-"+ref)
				w.WriteHeader(http.StatusOK)
			case http.MethodDelete:
				*deleted = append(*deleted, repo+"@"+ref)
				w.WriteHeader(http.StatusAccepted)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), deleted
}

// fakeGCExecer serves the two exec-phase commands runRegistryGC issues:
// `du -sk /var/lib/registry` (successive canned outputs, before/after) and
// `registry garbage-collect ...` (records the command, returns gcErr).
type fakeGCExecer struct {
	duOutputs []string
	duCalls   int
	gcErr     error
	commands  [][]string
}

func (f *fakeGCExecer) ExecPod(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error {
	f.commands = append(f.commands, cmd)
	joined := strings.Join(cmd, " ")
	switch {
	case strings.HasPrefix(joined, "du "):
		if f.duCalls >= len(f.duOutputs) {
			return fmt.Errorf("unexpected extra du call (already made %d)", f.duCalls)
		}
		fmt.Fprint(stdout, f.duOutputs[f.duCalls])
		f.duCalls++
		return nil
	case strings.Contains(joined, "garbage-collect"):
		return f.gcErr
	default:
		return fmt.Errorf("unexpected command %q", joined)
	}
}

// registryPod is the fake clientset fixture AppPods needs to find the
// registry pod in the system namespace.
func registryPod(namespace string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "registry-0",
		Namespace: namespace,
		Labels:    map[string]string{"app.kubernetes.io/name": "registry"},
	}}
}

// TestRunRegistryGC seeds one app with 3 deployments (2 failed + 1
// live/newest), registry_keep=1, and a fake registry catalog that also
// carries a stray repo absent from the DB entirely. It asserts the
// out-of-retention deployment images and the whole orphan repo are
// deleted, the live/newest image survives, and BytesReclaimed reflects
// the fake exec's before/after du readings.
func TestRunRegistryGC(t *testing.T) {
	registryHost, deleted := newFakeRegistryServer(t, map[string][]string{
		"proj/web":    {"1", "2", "3"},
		"orphan/repo": {"a", "b"},
	})

	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "failed", registryHost+"/proj/web:1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "failed", registryHost+"/proj/web:2", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "live", registryHost+"/proj/web:3", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("registry_keep", "1"); err != nil {
		t.Fatal(err)
	}

	cs := k8sfake.NewSimpleClientset(registryPod("luncur-system"))
	srv := newServer(Deps{Store: st, RegistryHost: registryHost, Kube: kube.NewForTest(nil, cs)})
	exec := &fakeGCExecer{duOutputs: []string{"5000\t/var/lib/registry\n", "3000\t/var/lib/registry\n"}}
	srv.execer = exec

	report, err := srv.runRegistryGC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", report.Warnings)
	}
	if report.DeletedManifests != 4 {
		t.Fatalf("DeletedManifests = %d, want 4", report.DeletedManifests)
	}
	if report.BytesReclaimed != 2048000 {
		t.Fatalf("BytesReclaimed = %d, want 2048000", report.BytesReclaimed)
	}

	want := map[string]bool{
		"proj/web@sha256:proj-web-1":       true,
		"proj/web@sha256:proj-web-2":       true,
		"orphan/repo@sha256:orphan-repo-a": true,
		"orphan/repo@sha256:orphan-repo-b": true,
	}
	if len(*deleted) != len(want) {
		t.Fatalf("deleted = %v, want %v", *deleted, want)
	}
	for _, d := range *deleted {
		if !want[d] {
			t.Fatalf("unexpected deletion %q (deleted = %v)", d, *deleted)
		}
	}
	for _, d := range *deleted {
		if d == "proj/web@sha256:proj-web-3" {
			t.Fatal("live/newest tag 3 must not be deleted")
		}
	}
}

// TestRunRegistryGCKeepsAppCacheAndSweepsGhostCache seeds one app with a
// live deployment and a build-cache manifest for that app, plus a
// build-cache repo for an app that no longer exists in the DB. It asserts
// the existing app's cache manifest survives the sweep while the ghost
// app's cache repo is deleted, mirroring the fix in T3: without adding
// cache repos to the keep-set, every cache manifest would be wiped on
// every sweep since deployRefs only ever sees app image repos.
func TestRunRegistryGCKeepsAppCacheAndSweepsGhostCache(t *testing.T) {
	registryHost, deleted := newFakeRegistryServer(t, map[string][]string{
		"proj/web":               {"1"},
		"luncur-cache/proj-web":  {"buildcache"},
		"luncur-cache/ghost-app": {"buildcache"},
	})

	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "live", registryHost+"/proj/web:1", 0); err != nil {
		t.Fatal(err)
	}

	cs := k8sfake.NewSimpleClientset(registryPod("luncur-system"))
	srv := newServer(Deps{Store: st, RegistryHost: registryHost, Kube: kube.NewForTest(nil, cs)})
	exec := &fakeGCExecer{duOutputs: []string{"1000\t/var/lib/registry\n", "1000\t/var/lib/registry\n"}}
	srv.execer = exec

	report, err := srv.runRegistryGC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", report.Warnings)
	}

	for _, d := range *deleted {
		if d == "luncur-cache/proj-web@sha256:luncur-cache-proj-web-buildcache" {
			t.Fatal("existing app's cache manifest must not be deleted")
		}
	}
	wantDeleted := "luncur-cache/ghost-app@sha256:luncur-cache-ghost-app-buildcache"
	found := false
	for _, d := range *deleted {
		if d == wantDeleted {
			found = true
		}
	}
	if !found {
		t.Fatalf("ghost app's cache manifest not deleted; deleted=%v", *deleted)
	}
}

// TestRegistryGCAPI exercises the admin-only HTTP endpoint: a member is
// forbidden; an admin gets 200 with the report fields even with no kube
// configured (the manifest-delete phase still runs; the exec phase warns).
func TestRegistryGCAPI(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{})
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: registryHost})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "pleb@b.co", "member")

	forbidden := doAuthed(t, "POST", srv.URL+"/v1/registry/gc", member, "")
	forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("member: want 403, got %d", forbidden.StatusCode)
	}

	resp := doAuthed(t, "POST", srv.URL+"/v1/registry/gc", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		DeletedManifests int      `json:"deleted_manifests"`
		BytesReclaimed   int64    `json:"bytes_reclaimed"`
		Warnings         []string `json:"warnings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.DeletedManifests != 0 {
		t.Fatalf("deleted_manifests = %d, want 0", out.DeletedManifests)
	}
	if out.BytesReclaimed != -1 {
		t.Fatalf("bytes_reclaimed = %d, want -1 (no kube configured)", out.BytesReclaimed)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("want a warning about the exec phase when kube is unavailable")
	}
}
