package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestVolumeAddListRemoveRoundTrip(t *testing.T) {
	srv, st := testServer(t) // no kube; app never live, so no sync required
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	// Add (name defaults to last path segment).
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"/var/lib/data","size_gb":5}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add volume: want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["name"] != "data" || out["path"] != "/var/lib/data" || out["size_gb"] != float64(5) {
		t.Fatalf("add response: %v", out)
	}
	warning, _ := out["warning"].(string)
	if !strings.Contains(warning, "Recreate") || !strings.Contains(warning, "backup") {
		t.Fatalf("warning: %q", warning)
	}

	// List.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/volumes", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list volumes: want 200, got %d", resp.StatusCode)
	}
	var list struct {
		Volumes []map[string]any `json:"volumes"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Volumes) != 1 || list.Volumes[0]["name"] != "data" {
		t.Fatalf("list: %v", list.Volumes)
	}

	// Invalid path -> 400.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"relative","size_gb":5}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad path: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Remove (no purge).
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/volumes/data", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove volume: want 200, got %d", resp.StatusCode)
	}
	var rm map[string]any
	json.NewDecoder(resp.Body).Decode(&rm)
	resp.Body.Close()
	if rm["removed"] != "data" || rm["purged"] != false {
		t.Fatalf("remove response: %v", rm)
	}

	// Remove again -> 404.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/volumes/data", admin, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second remove: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVolumeAddCronKindMismatch400(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/volumes", admin, `{"path":"/data","size_gb":1}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "kind_mismatch" {
		t.Fatalf("code = %q, want kind_mismatch", env.Error.Code)
	}
}

func TestVolumeAddEjected409(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "web")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, "").Body.Close()

	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/volumes", admin, `{"path":"/data","size_gb":1}`), "add volume on ejected app")
	assertAppEjected(t, doAuthed(t, "DELETE", srv.URL+"/v1/projects/proj/apps/web/volumes/data", admin, ""), "remove volume on ejected app")
}

func assertVolumeReplicaConflict(t *testing.T, resp *http.Response, label string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("%s: want 409, got %d", label, resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("%s: decode: %v", label, err)
	}
	if env.Error.Code != "volume_replica_conflict" {
		t.Fatalf("%s: code = %q, want volume_replica_conflict", label, env.Error.Code)
	}
}

func TestVolumeReplicaConflicts(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	// Scale to 2, then adding a volume conflicts.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":2}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale to 2: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	assertVolumeReplicaConflict(t,
		doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"/data","size_gb":1}`),
		"add volume with replicas>1")

	// Back to 1, add works.
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":1}`).Body.Close()
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"/data","size_gb":1}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add volume at 1 replica: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// With a volume, scaling above 1 conflicts; scaling to 1 stays fine.
	assertVolumeReplicaConflict(t,
		doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":2}`),
		"scale to 2 with volume")
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale to 1 with volume: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVolumePurgeDeletesPVC(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"/data","size_gb":1}`).Body.Close()

	// Remove without purge: no PVC delete issued.
	*actions = nil
	resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/volumes/data", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if joined := strings.Join(*actions, ","); strings.Contains(joined, "delete persistentvolumeclaims") {
		t.Fatalf("remove without purge deleted PVC: %s", joined)
	}

	// Re-add, remove with purge: PVC delete issued.
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"/data","size_gb":1}`).Body.Close()
	*actions = nil
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/volumes/data?purge=true", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("purge remove: want 200, got %d", resp.StatusCode)
	}
	var rm map[string]any
	json.NewDecoder(resp.Body).Decode(&rm)
	resp.Body.Close()
	if rm["purged"] != true {
		t.Fatalf("purged: %v", rm)
	}
	if joined := strings.Join(*actions, ","); !strings.Contains(joined, "delete persistentvolumeclaims") {
		t.Fatalf("purge did not delete PVC: %s", joined)
	}
}

func TestVolumePurgeWithoutKube503RowSurvives(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"/data","size_gb":1}`).Body.Close()

	resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/volumes/data?purge=true", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("purge without kube: want 503, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "kubernetes_unavailable" {
		t.Fatalf("code = %q, want kubernetes_unavailable", env.Error.Code)
	}

	vols, err := st.ListVolumes(appID(t, st, "web", "api"))
	if err != nil || len(vols) != 1 {
		t.Fatalf("row must survive failed purge: %+v %v", vols, err)
	}
}

func TestVolumeLiveAddAppliesPVCAndRecreate(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"nginx:1"}`).Body.Close()

	before := len(*actions)
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/volumes", admin, `{"path":"/data","size_gb":2}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add volume: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if len(*actions) <= before {
		t.Fatal("adding a volume to a live app should re-apply to cluster")
	}
	if joined := strings.Join(*actions, ","); !strings.Contains(joined, "patch persistentvolumeclaims") {
		t.Fatalf("no PVC apply recorded: %s", joined)
	}

	// Rendered manifest carries the PVC and Recreate strategy.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/raw", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw manifest: want 200, got %d", resp.StatusCode)
	}
	var buf strings.Builder
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	yamlStr := buf.String()
	if !strings.Contains(yamlStr, "PersistentVolumeClaim") || !strings.Contains(yamlStr, "api-data") {
		t.Fatalf("rendered manifest missing PVC:\n%s", yamlStr)
	}
	if !strings.Contains(yamlStr, "Recreate") {
		t.Fatalf("rendered manifest missing Recreate strategy:\n%s", yamlStr)
	}
	if !strings.Contains(yamlStr, "mountPath: /data") {
		t.Fatalf("rendered manifest missing volume mount:\n%s", yamlStr)
	}
}
