package update

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mdschoff/updock/internal/dockerx"
)

// fakeClient is a scriptable in-memory dockerx.Client that records every call.
type fakeClient struct {
	calls []string

	// newContainerStates is the sequence of states the replacement container
	// reports during verification (last one repeats).
	newContainerStates []dockerx.State

	pullErr  error
	startErr map[string]error // per-container-id start errors

	removed []string
	renames map[string]string // id -> current name
	started []string
}

func newFake(states ...dockerx.State) *fakeClient {
	return &fakeClient{
		newContainerStates: states,
		renames:            map[string]string{},
		startErr:           map[string]error{},
	}
}

func (f *fakeClient) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeClient) ListRunning(context.Context) ([]dockerx.ContainerInfo, error) {
	return nil, nil
}
func (f *fakeClient) RemoteDigest(context.Context, string) (string, error) { return "sha256:new", nil }
func (f *fakeClient) ImageStatus(context.Context, string, string, string) (dockerx.ImageStatus, error) {
	return dockerx.StatusStale, nil
}

func (f *fakeClient) Pull(_ context.Context, ref string) error {
	f.record("pull %s", ref)
	return f.pullErr
}
func (f *fakeClient) Stop(_ context.Context, id string, _ time.Duration) error {
	f.record("stop %s", id)
	return nil
}
func (f *fakeClient) Start(_ context.Context, id string) error {
	f.record("start %s", id)
	if err := f.startErr[id]; err != nil {
		return err
	}
	f.started = append(f.started, id)
	return nil
}
func (f *fakeClient) Rename(_ context.Context, id, name string) error {
	f.record("rename %s -> %s", id, name)
	f.renames[id] = name
	return nil
}
func (f *fakeClient) CaptureSpec(_ context.Context, id string) (*dockerx.CloneSpec, error) {
	f.record("capture %s", id)
	return &dockerx.CloneSpec{}, nil
}
func (f *fakeClient) CreateFromSpec(_ context.Context, _ *dockerx.CloneSpec, name, imageRef string) (string, error) {
	f.record("create %s from %s", name, imageRef)
	return "new-id", nil
}
func (f *fakeClient) Remove(_ context.Context, id string) error {
	f.record("remove %s", id)
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeClient) State(_ context.Context, id string) (dockerx.State, error) {
	if id != "new-id" {
		return dockerx.State{Running: true}, nil
	}
	st := f.newContainerStates[0]
	if len(f.newContainerStates) > 1 {
		f.newContainerStates = f.newContainerStates[1:]
	}
	return st, nil
}

func testUpdater(f *fakeClient, window time.Duration) *Updater {
	return &Updater{
		Client:       f,
		VerifyWindow: window,
		StopTimeout:  time.Second,
		PollInterval: time.Millisecond,
	}
}

var testContainer = dockerx.ContainerInfo{
	ID: "old-id", Name: "app", Image: "example.com/app:latest",
}

func TestUpdateSuccessNoHealthcheck(t *testing.T) {
	// Replacement has no healthcheck and keeps running: pass after the window.
	f := newFake(dockerx.State{Running: true})
	res := testUpdater(f, 10*time.Millisecond).Update(context.Background(), testContainer)

	if res.Action != ActionUpdated {
		t.Fatalf("action = %q (%v), want updated", res.Action, res.Err)
	}
	if !slices.Contains(f.removed, "old-id") {
		t.Errorf("backup container was not removed after successful update; calls: %v", f.calls)
	}
	if slices.Contains(f.removed, "new-id") {
		t.Errorf("replacement container was removed on the success path")
	}
}

func TestUpdateSuccessHealthy(t *testing.T) {
	// Healthcheck flips to healthy before the window ends: pass immediately.
	f := newFake(
		dockerx.State{Running: true, Health: "starting"},
		dockerx.State{Running: true, Health: "healthy"},
	)
	res := testUpdater(f, time.Minute).Update(context.Background(), testContainer)
	if res.Action != ActionUpdated {
		t.Fatalf("action = %q (%v), want updated", res.Action, res.Err)
	}
}

func TestRollbackOnCrash(t *testing.T) {
	// Replacement exits immediately: roll back to the original container.
	f := newFake(dockerx.State{Running: false, ExitCode: 1})
	res := testUpdater(f, time.Minute).Update(context.Background(), testContainer)

	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back", res.Action, res.Err)
	}
	if !slices.Contains(f.removed, "new-id") {
		t.Errorf("failed replacement was not removed; calls: %v", f.calls)
	}
	if f.renames["old-id"] != "app" {
		t.Errorf("original container not renamed back, renames: %v", f.renames)
	}
	if !slices.Contains(f.started, "old-id") {
		t.Errorf("original container not restarted; calls: %v", f.calls)
	}
	if slices.Contains(f.removed, "old-id") {
		t.Errorf("original container must never be removed on rollback")
	}
}

func TestRollbackOnUnhealthy(t *testing.T) {
	f := newFake(
		dockerx.State{Running: true, Health: "starting"},
		dockerx.State{Running: true, Health: "unhealthy"},
	)
	res := testUpdater(f, time.Minute).Update(context.Background(), testContainer)
	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back", res.Action, res.Err)
	}
}

func TestRollbackOnHealthcheckStuckStarting(t *testing.T) {
	// Healthcheck never leaves "starting" before the window ends: fail.
	f := newFake(dockerx.State{Running: true, Health: "starting"})
	res := testUpdater(f, 10*time.Millisecond).Update(context.Background(), testContainer)
	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back", res.Action, res.Err)
	}
}

func TestPullFailureTouchesNothing(t *testing.T) {
	f := newFake(dockerx.State{Running: true})
	f.pullErr = fmt.Errorf("registry unreachable")
	res := testUpdater(f, time.Minute).Update(context.Background(), testContainer)

	if res.Action != ActionError {
		t.Fatalf("action = %q, want error", res.Action)
	}
	// Only the read-only capture and the failed pull may have happened; the
	// running container must not have been touched.
	allowed := map[string]bool{"capture old-id": true, "pull example.com/app:latest": true}
	for _, call := range f.calls {
		if !allowed[call] {
			t.Errorf("unexpected call after pull failure: %q", call)
		}
	}
}

func TestStartFailureRestoresOriginal(t *testing.T) {
	f := newFake(dockerx.State{Running: true})
	f.startErr["new-id"] = fmt.Errorf("port already allocated")
	res := testUpdater(f, time.Minute).Update(context.Background(), testContainer)

	if res.Action != ActionError {
		t.Fatalf("action = %q, want error", res.Action)
	}
	if f.renames["old-id"] != "app" {
		t.Errorf("original not renamed back after start failure; renames: %v", f.renames)
	}
	if !slices.Contains(f.started, "old-id") {
		t.Errorf("original not restarted after start failure; calls: %v", f.calls)
	}
}
