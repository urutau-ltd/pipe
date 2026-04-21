package main

import (
	"net"
	"path/filepath"
	"testing"
)

func TestNormalizeSocketHint(t *testing.T) {
	tmp := t.TempDir()
	socket := filepath.Join(tmp, "podman.sock")

	got, err := normalizeSocketHint("unix://" + socket)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != socket {
		t.Fatalf("unexpected socket path: got=%q want=%q", got, socket)
	}

	if _, err := normalizeSocketHint("relative.sock"); err == nil {
		t.Fatal("expected error for relative socket")
	}
}

func TestUnixSocketPathFromHost(t *testing.T) {
	if _, ok := unixSocketPathFromHost("tcp://127.0.0.1:2375"); ok {
		t.Fatal("did not expect tcp host to parse as unix socket")
	}

	got, ok := unixSocketPathFromHost("unix:///var/run/docker.sock")
	if !ok {
		t.Fatal("expected unix host to parse")
	}
	if got != "/var/run/docker.sock" {
		t.Fatalf("unexpected unix path: %q", got)
	}
}

func TestIsUnixSocket(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "engine.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer l.Close()

	if !isUnixSocket(path) {
		t.Fatal("expected unix socket to be detected")
	}
}
