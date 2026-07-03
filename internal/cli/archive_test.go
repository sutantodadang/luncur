package cli

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackSource(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "x"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := packSource(dir)
	if err != nil {
		t.Fatalf("packSource: %v", err)
	}

	gr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gr)

	found := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		found[hdr.Name] = string(b)
	}

	content, ok := found["main.go"]
	if !ok {
		t.Fatalf("main.go missing from archive; entries: %v", found)
	}
	if content != "package main" {
		t.Fatalf("main.go content = %q, want %q", content, "package main")
	}

	for name := range found {
		if strings.Contains(name, "node_modules") {
			t.Fatalf("node_modules should be excluded, found entry %q", name)
		}
	}
}
