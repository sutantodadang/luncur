package cli

import (
	"strings"
	"testing"
)

const twoDocYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 1
---
apiVersion: v1
kind: Service
metadata:
  name: api
`

func TestExtractDoc(t *testing.T) {
	doc, err := extractDoc([]byte(twoDocYAML), "Service")
	if err != nil || !strings.Contains(string(doc), "kind: Service") {
		t.Fatalf("extract: %v\n%s", err, doc)
	}
	if _, err := extractDoc([]byte(twoDocYAML), "Ingress"); err == nil {
		t.Fatal("want error for missing kind")
	}
}

func TestComputeOverride(t *testing.T) {
	base := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 1
`
	edited := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  labels:
    team: x
spec:
  replicas: 1
`
	patch, err := computeOverride("Deployment", []byte(base), []byte(edited))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, `"team":"x"`) {
		t.Fatalf("patch: %s", patch)
	}
	// No edit → empty patch.
	same, err := computeOverride("Deployment", []byte(base), []byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if same != "{}" {
		t.Fatalf("want {} for no changes, got %s", same)
	}
}
