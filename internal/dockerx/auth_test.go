package dockerx

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/registry"
)

func writeConfig(t *testing.T, cfg string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

func decodeAuth(t *testing.T, encoded string) registry.AuthConfig {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding auth header: %v", err)
	}
	var ac registry.AuthConfig
	if err := json.Unmarshal(raw, &ac); err != nil {
		t.Fatalf("unmarshaling auth header: %v", err)
	}
	return ac
}

func TestPlainAuthsEntry(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	writeConfig(t, `{"auths":{"registry.example.com":{"auth":"`+auth+`"}}}`)

	got := encodedAuthFor("registry.example.com/app:latest")
	if got == "" {
		t.Fatal("expected credentials, got none")
	}
	ac := decodeAuth(t, got)
	if ac.Username != "alice" || ac.Password != "s3cret" {
		t.Fatalf("wrong credentials: %+v", ac)
	}
}

func TestDockerHubLegacyIndexKey(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("bob:hunter2"))
	writeConfig(t, `{"auths":{"https://index.docker.io/v1/":{"auth":"`+auth+`"}}}`)

	// A bare Docker Hub reference must resolve to the legacy index entry.
	got := encodedAuthFor("redis:7-alpine")
	if got == "" {
		t.Fatal("expected Docker Hub credentials, got none")
	}
	if ac := decodeAuth(t, got); ac.Username != "bob" {
		t.Fatalf("wrong credentials: %+v", ac)
	}
}

func TestUnknownRegistryIsAnonymous(t *testing.T) {
	writeConfig(t, `{"auths":{"registry.example.com":{"auth":"x"}}}`)
	if got := encodedAuthFor("ghcr.io/someone/app:latest"); got != "" {
		t.Fatalf("expected anonymous access, got %q", got)
	}
}

func TestMissingConfigIsAnonymous(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir()) // empty dir, no config.json
	if got := encodedAuthFor("registry.example.com/app:latest"); got != "" {
		t.Fatalf("expected anonymous access, got %q", got)
	}
}

func TestSchemePrefixedKeyAndUsernamePasswordFields(t *testing.T) {
	writeConfig(t, `{"auths":{"https://registry.example.com/":{"username":"carol","password":"pw"}}}`)
	got := encodedAuthFor("registry.example.com/app:v1")
	if got == "" {
		t.Fatal("expected credentials, got none")
	}
	if ac := decodeAuth(t, got); ac.Username != "carol" || ac.Password != "pw" {
		t.Fatalf("wrong credentials: %+v", ac)
	}
}
