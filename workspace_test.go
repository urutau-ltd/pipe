package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeArtifactPattern(t *testing.T) {
	if _, err := normalizeArtifactPattern("../dist"); err == nil {
		t.Fatal("expected rejection for path traversal")
	}
	if _, err := normalizeArtifactPattern("/abs/path"); err == nil {
		t.Fatal("expected rejection for absolute path")
	}

	got, err := normalizeArtifactPattern("dist/*.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != filepath.Clean("dist/*.tar.gz") {
		t.Fatalf("unexpected normalized path: %q", got)
	}
}

func TestExportArtifacts(t *testing.T) {
	workspace := t.TempDir()
	target := t.TempDir()

	if err := os.MkdirAll(filepath.Join(workspace, "dist"), 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	src := filepath.Join(workspace, "dist", "app.tar.gz")
	if err := os.WriteFile(src, []byte("artifact"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	if err := exportArtifacts(workspace, target, []string{"dist/*.tar.gz"}); err != nil {
		t.Fatalf("exportArtifacts: %v", err)
	}

	dst := filepath.Join(target, "dist", "app.tar.gz")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read exported artifact: %v", err)
	}
	if string(data) != "artifact" {
		t.Fatalf("unexpected artifact content: %q", string(data))
	}
}
