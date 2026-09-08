package dockerx

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverHostRespectsExplicitEnv(t *testing.T) {
	home := t.TempDir()
	touch(t, filepath.Join(home, ".orbstack/run/docker.sock"))
	if got := discoverHostIn("tcp://somewhere:2375", "/nonexistent", home, ""); got != "" {
		t.Fatalf("DOCKER_HOST set must disable discovery, got %q", got)
	}
}

func TestDiscoverHostPrefersDefaultSocket(t *testing.T) {
	home := t.TempDir()
	defaultSock := filepath.Join(home, "docker.sock")
	touch(t, defaultSock)
	touch(t, filepath.Join(home, ".orbstack/run/docker.sock"))
	if got := discoverHostIn("", defaultSock, home, ""); got != "" {
		t.Fatalf("existing default socket must disable discovery, got %q", got)
	}
}

func TestDiscoverHostFindsPerUserSocket(t *testing.T) {
	home := t.TempDir()
	sock := filepath.Join(home, ".orbstack/run/docker.sock")
	touch(t, sock)
	if got := discoverHostIn("", filepath.Join(home, "missing.sock"), home, ""); got != "unix://"+sock {
		t.Fatalf("expected discovered OrbStack socket, got %q", got)
	}
}

func TestDiscoverHostFindsRootlessSocket(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	sock := filepath.Join(xdg, "docker.sock")
	touch(t, sock)
	if got := discoverHostIn("", filepath.Join(home, "missing.sock"), home, xdg); got != "unix://"+sock {
		t.Fatalf("expected rootless socket, got %q", got)
	}
}

func TestDiscoverHostNothingFound(t *testing.T) {
	home := t.TempDir()
	if got := discoverHostIn("", filepath.Join(home, "missing.sock"), home, ""); got != "" {
		t.Fatalf("expected empty result, got %q", got)
	}
}
