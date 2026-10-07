package store

import (
	"encoding/json"
)

var overridableKinds = map[string]bool{"Deployment": true, "Service": true, "Ingress": true, "CronJob": true}

// dangerousDeploymentKeys are pod-spec fields that let an override escape
// the app's own namespace/pod boundary (host filesystem/network/PID/IPC
// access, privileged containers, or an arbitrary ServiceAccount identity).
// Strategic-merge patches can nest these anywhere under
// spec.template.spec, so we scan the whole patch for the key names rather
// than trying to walk a fixed path.
var dangerousDeploymentKeys = map[string]string{
	"hostNetwork":        "hostNetwork",
	"hostPID":            "hostPID",
	"hostIPC":            "hostIPC",
	"hostPath":           "hostPath volumes",
	"privileged":         "privileged",
	"serviceAccountName": "serviceAccountName",
	"serviceAccount":     "serviceAccount", // deprecated alias of serviceAccountName
}

// rejectDangerousOverride denies patch fields that would let a member
// escalate to node compromise or hijack routing:
//   - metadata.name/metadata.namespace on any kind (renaming orphans objects)
//   - Ingress spec.rules (host hijack) and spec.defaultBackend (catch-all)
//   - Service types other than ClusterIP, nodePort, externalIPs
//   - Deployment pod-spec escape hatches (see dangerousDeploymentKeys)
func rejectDangerousOverride(kind string, patch map[string]any) error {
	if md, ok := patch["metadata"].(map[string]any); ok {
		if _, ok := md["name"]; ok {
			return validationErrorf("override may not set %q", "metadata.name")
		}
		if _, ok := md["namespace"]; ok {
			return validationErrorf("override may not set %q", "metadata.namespace")
		}
	}

	if kind == "Ingress" {
		if spec, ok := patch["spec"].(map[string]any); ok {
			// rules: host hijack. defaultBackend (and its pre-v1 name
			// backend): a cluster-wide catch-all for every unmatched host.
			for _, key := range []string{"rules", "defaultBackend", "backend"} {
				if _, ok := spec[key]; ok {
					return validationErrorf("override may not set %q", "spec."+key)
				}
			}
		}
	}

	// CronJob nests the same pod spec (spec.jobTemplate.spec.template.spec),
	// so it gets the same escape-hatch scan.
	if kind == "Deployment" || kind == "CronJob" {
		for _, key := range collectKeys(patch) {
			if field, bad := dangerousDeploymentKeys[key]; bad {
				return validationErrorf("override may not set %q", field)
			}
		}
	}

	if kind == "Service" {
		for _, key := range collectKeys(patch) {
			if key == "externalIPs" || key == "loadBalancerIP" || key == "nodePort" {
				return validationErrorf("override may not set %q", key)
			}
		}
		// NodePort/LoadBalancer expose the app on every node's host IP,
		// around the ingress and the isolation NetworkPolicy (K3s ServiceLB
		// binds the port on the host itself).
		if spec, ok := patch["spec"].(map[string]any); ok {
			if t, ok := spec["type"]; ok && t != "ClusterIP" {
				return validationErrorf("override may not set spec.type %v (only ClusterIP)", t)
			}
		}
	}

	return nil
}

// collectKeys recursively walks a decoded JSON value (map[string]any /
// []any / scalars) and returns every map key found anywhere within it.
func collectKeys(v any) []string {
	var keys []string
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			keys = append(keys, k)
			keys = append(keys, collectKeys(val)...)
		}
	case []any:
		for _, val := range t {
			keys = append(keys, collectKeys(val)...)
		}
	}
	return keys
}

func (s *Store) SetOverride(appID int64, kind, patchJSON string) error {
	if err := ValidateOverride(kind, patchJSON); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO overrides (app_id, kind, patch_json) VALUES (?, ?, ?)
		 ON CONFLICT (app_id, kind) DO UPDATE
		 SET patch_json = excluded.patch_json, updated_at = datetime('now')`,
		appID, kind, patchJSON,
	)
	return err
}

// ValidateOverride applies SetOverride's checks without storing anything:
// a supported kind, a JSON-object patch, and none of the fields that would
// escape the app's boundary (see rejectDangerousOverride).
func ValidateOverride(kind, patchJSON string) error {
	if !overridableKinds[kind] {
		return validationErrorf("unsupported kind %q (Deployment, Service, Ingress, or CronJob)", kind)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(patchJSON), &obj); err != nil || obj == nil {
		return validationErrorf("override patch must be a JSON object (got %q): %v", patchJSON, err)
	}
	return rejectDangerousOverride(kind, obj)
}

func (s *Store) Overrides(appID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT kind, patch_json FROM overrides WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, p string
		if err := rows.Scan(&k, &p); err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, rows.Err()
}

func (s *Store) DeleteOverride(appID int64, kind string) error {
	res, err := s.db.Exec(`DELETE FROM overrides WHERE app_id = ? AND kind = ?`, appID, kind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
