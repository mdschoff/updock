// Package e2e exercises updock against a real Docker daemon:
//
//  1. run a throwaway local registry
//  2. push a healthy image, start a container from it
//  3. push a NEWER healthy image  → expect updock to update the container
//  4. push a BROKEN image (crashes on start) → expect updock to try it,
//     detect the failure, and roll the container back to the working version
//
// Requires a running Docker daemon; skipped under -short.
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdschoff/updock/internal/config"
	"github.com/mdschoff/updock/internal/dockerx"
	"github.com/mdschoff/updock/internal/engine"
	"github.com/mdschoff/updock/internal/notify"
)

const (
	registryName = "updock-e2e-registry"
	registryAddr = "127.0.0.1:5590"
	appName      = "updock-e2e-app"
	appImage     = registryAddr + "/e2e-app:latest"
)

// recorder captures events for assertions.
type recorder struct{ events []notify.Event }

func (r *recorder) Notify(_ context.Context, e notify.Event) { r.events = append(r.events, e) }

func (r *recorder) last() string {
	if len(r.events) == 0 {
		return ""
	}
	return r.events[len(r.events)-1].Action
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// buildAndPush builds an image with the given CMD and pushes it as appImage.
func buildAndPush(t *testing.T, cmd string) {
	t.Helper()
	dir := t.TempDir()
	df := fmt.Sprintf("FROM busybox\nCMD %s\n", cmd)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	docker(t, "build", "-q", "-t", appImage, dir)
	docker(t, "push", appImage)
}

// publishNewVersion pushes a new version to the registry WITHOUT re-pointing
// the local tag, mirroring production: the registry moves ahead while the
// local daemon still holds the currently running version.
func publishNewVersion(t *testing.T, cmd string) {
	t.Helper()
	const keep = registryAddr + "/e2e-keep:current"
	docker(t, "tag", appImage, keep) // protect the running version's manifest
	buildAndPush(t, cmd)
	docker(t, "tag", keep, appImage) // restore local tag to the running version
	docker(t, "rmi", keep)
}

// appImageID returns the image identifier the app container currently runs.
// Comparing these values across phases works on both image stores.
func appImageID(t *testing.T) string {
	t.Helper()
	return docker(t, "inspect", "--format", "{{.Image}}", appName)
}

func TestUpdateAndRollbackAgainstRealDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short mode")
	}
	slog.SetLogLoggerLevel(slog.LevelDebug)
	cli, err := dockerx.New()
	if err != nil {
		t.Skipf("docker not available: %v", err)
	}
	ctx := context.Background()
	if err := cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon not running: %v", err)
	}

	// -- setup ---------------------------------------------------------------
	cleanup := func() {
		exec.Command("docker", "rm", "-f", appName, appName+".updock-backup", registryName).Run()
	}
	cleanup()
	t.Cleanup(cleanup)

	docker(t, "run", "-d", "--name", registryName, "-p", registryAddr+":5000", "registry:2")
	// Give the registry a moment to accept connections.
	time.Sleep(2 * time.Second)

	// v1: healthy app.
	buildAndPush(t, `["sh", "-c", "echo v1 running; while true; do sleep 1; done"]`)
	docker(t, "run", "-d", "--name", appName, "--label", "updock.enable=true", appImage)
	v1 := appImageID(t)

	cfg := config.Default()
	cfg.OptIn = true // only touch containers labeled updock.enable=true
	cfg.VerifyWindow = 6 * time.Second
	cfg.StopTimeout = 5 * time.Second
	rec := &recorder{}
	eng := engine.New(cli, cfg, rec)
	eng.Updater.PollInterval = 500 * time.Millisecond

	// -- phase 1: no update available → nothing happens ----------------------
	if err := eng.RunOnce(ctx); err != nil {
		t.Fatalf("run (up-to-date): %v", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("expected no events when up to date, got %+v", rec.events)
	}

	// -- phase 2: healthy update → container updated -------------------------
	publishNewVersion(t, `["sh", "-c", "echo v2 running; while true; do sleep 1; done"]`)
	if err := eng.RunOnce(ctx); err != nil {
		t.Fatalf("run (healthy update): %v", err)
	}
	if got := rec.last(); got != "updated" {
		t.Fatalf("expected action 'updated', got %q (events: %+v)", got, rec.events)
	}
	v2 := appImageID(t)
	if v2 == v1 {
		t.Fatal("image unchanged after supposed update")
	}
	if state := docker(t, "inspect", "--format", "{{.State.Running}}", appName); state != "true" {
		t.Fatalf("app not running after update, state=%s", state)
	}
	// Backup container must be gone after a committed update.
	if out, _ := exec.Command("docker", "inspect", appName+".updock-backup").CombinedOutput(); !strings.Contains(strings.ToLower(string(out)), "no such") {
		t.Fatalf("backup container still exists after successful update: %s", out)
	}

	// -- phase 3: broken update → automatic rollback to v2 -------------------
	publishNewVersion(t, `["sh", "-c", "echo v3 exploding; exit 1"]`)
	if err := eng.RunOnce(ctx); err != nil {
		t.Fatalf("run (broken update): %v", err)
	}
	if got := rec.last(); got != "rolled_back" {
		t.Fatalf("expected action 'rolled_back', got %q (events: %+v)", got, rec.events)
	}
	if state := docker(t, "inspect", "--format", "{{.State.Running}}", appName); state != "true" {
		t.Fatalf("app not running after rollback, state=%s", state)
	}
	if after := appImageID(t); after != v2 {
		t.Fatalf("expected rollback to v2 image %s, got %s", v2, after)
	}
	logs := docker(t, "logs", appName)
	if !strings.Contains(logs, "v2 running") {
		t.Fatalf("rolled-back container is not running v2; logs: %s", logs)
	}
}
