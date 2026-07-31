package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mdschoff/updock/internal/config"
	"github.com/mdschoff/updock/internal/dockerx"
	"github.com/mdschoff/updock/internal/engine"
)

const (
	authRegistryName = "updock-e2e-authreg"
	authRegistryAddr = "127.0.0.1:5592"
	authAppName      = "updock-e2e-authapp"
	authAppImage     = authRegistryAddr + "/auth-app:latest"
	authUser         = "updock"
	authPass         = "e2e-secret-pw"
)

// TestPrivateRegistryUpdate proves the authenticated path end to end: an
// htpasswd-protected registry, credentials supplied only via a docker
// config.json, and an update that requires both an authenticated digest
// check and an authenticated daemon-side pull.
func TestPrivateRegistryUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short mode")
	}
	cli, err := dockerx.New()
	if err != nil {
		t.Skipf("docker not available: %v", err)
	}
	ctx := context.Background()
	if err := cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon not running: %v", err)
	}

	cleanup := func() {
		exec.Command("docker", "rm", "-f", authAppName, authAppName+".updock-backup", authRegistryName).Run()
	}
	cleanup()
	t.Cleanup(cleanup)

	// htpasswd file (bcrypt, the only algorithm registry:2 accepts).
	hash, err := bcrypt.GenerateFromPassword([]byte(authPass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "htpasswd"),
		[]byte(authUser+":"+string(hash)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	docker(t, "run", "-d", "--name", authRegistryName,
		"-p", authRegistryAddr+":5000",
		"-v", authDir+":/auth:ro",
		"-e", "REGISTRY_AUTH=htpasswd",
		"-e", "REGISTRY_AUTH_HTPASSWD_REALM=updock-e2e",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
		"registry:2")
	time.Sleep(2 * time.Second)

	// Credentials exist ONLY in this config.json; DOCKER_CONFIG points both
	// the docker CLI (push) and updock's auth loader at it.
	cfgDir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte(authUser + ":" + authPass))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"),
		[]byte(fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, authRegistryAddr, auth)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", cfgDir)

	// v1 up and running.
	buildAndPushRef(t, authAppImage, `["sh", "-c", "echo auth-v1; while true; do sleep 1; done"]`)
	docker(t, "run", "-d", "--name", authAppName, "--label", "updock.enable=true", authAppImage)
	v1 := docker(t, "inspect", "--format", "{{.Image}}", authAppName)

	// Registry moves ahead; local tag stays on the running version.
	const keep = authRegistryAddr + "/auth-keep:current"
	docker(t, "tag", authAppImage, keep)
	buildAndPushRef(t, authAppImage, `["sh", "-c", "echo auth-v2; while true; do sleep 1; done"]`)
	docker(t, "tag", keep, authAppImage)
	docker(t, "rmi", keep)

	cfg := config.Default()
	cfg.OptIn = true
	cfg.VerifyWindow = 6 * time.Second
	cfg.StopTimeout = 5 * time.Second
	rec := &recorder{}
	eng := engine.New(cli, cfg, rec)
	eng.Updater.PollInterval = 500 * time.Millisecond

	if err := eng.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := rec.last(); got != "updated" {
		t.Fatalf("expected action 'updated' via authenticated registry, got %q (events: %+v)", got, rec.events)
	}
	if after := docker(t, "inspect", "--format", "{{.Image}}", authAppName); after == v1 {
		t.Fatal("image unchanged after supposed update")
	}
}

// buildAndPushRef builds an image with the given CMD and pushes it as ref.
func buildAndPushRef(t *testing.T, ref, cmd string) {
	t.Helper()
	dir := t.TempDir()
	df := fmt.Sprintf("FROM busybox\nCMD %s\n", cmd)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	docker(t, "build", "-q", "-t", ref, dir)
	docker(t, "push", ref)
}
