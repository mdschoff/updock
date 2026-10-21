package update

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mdschoff/updock/internal/dockerx"
)

// fakeClient is a scriptable in-memory dockerx.Client that records every
// call. Every method honors context cancellation, so tests can prove which
// operations run on the shutdown-immune ops context.
type fakeClient struct {
	calls []string

	// newContainerStates is the sequence of states the replacement container
	// reports during verification (last one repeats).
	newContainerStates []dockerx.State

	pullErr  error
	startErr map[string]error // per-container-id start errors

	// replacementMissing makes FindByName report no replacement container.
	replacementMissing bool
	// stateErrs are consumed one per State call before states; a non-nil
	// entry makes that call fail (simulates a flaky daemon).
	stateErrs []error
	// onState, when set, runs on each State call for the replacement —
	// used to cancel a context mid-verification.
	onState func()

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
func (f *fakeClient) ListBackups(ctx context.Context, _ string) ([]dockerx.ContainerInfo, error) {
	return nil, ctx.Err()
}
func (f *fakeClient) FindByName(ctx context.Context, name string) (dockerx.ContainerInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return dockerx.ContainerInfo{}, false, err
	}
	f.record("find %s", name)
	if f.replacementMissing {
		return dockerx.ContainerInfo{}, false, nil
	}
	return dockerx.ContainerInfo{ID: "new-id", Name: name, Image: "example.com/app:latest"}, true, nil
}
func (f *fakeClient) RemoteDigest(context.Context, string) (string, error) { return "sha256:new", nil }
func (f *fakeClient) ImageStatus(context.Context, string, string, string) (dockerx.ImageStatus, error) {
	return dockerx.StatusStale, nil
}

func (f *fakeClient) Pull(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("pull %s", ref)
	return f.pullErr
}
func (f *fakeClient) Stop(ctx context.Context, id string, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("stop %s", id)
	return nil
}
func (f *fakeClient) Start(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("start %s", id)
	if err := f.startErr[id]; err != nil {
		return err
	}
	f.started = append(f.started, id)
	return nil
}
func (f *fakeClient) Rename(ctx context.Context, id, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("rename %s -> %s", id, name)
	f.renames[id] = name
	return nil
}
func (f *fakeClient) CaptureSpec(ctx context.Context, id string) (*dockerx.CloneSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.record("capture %s", id)
	return &dockerx.CloneSpec{}, nil
}
func (f *fakeClient) CreateFromSpec(ctx context.Context, _ *dockerx.CloneSpec, name, imageRef string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.record("create %s from %s", name, imageRef)
	return "new-id", nil
}
func (f *fakeClient) Remove(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.record("remove %s", id)
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeClient) State(ctx context.Context, id string) (dockerx.State, error) {
	if err := ctx.Err(); err != nil {
		return dockerx.State{}, err
	}
	if id != "new-id" {
		return dockerx.State{Running: true}, nil
	}
	if len(f.stateErrs) > 0 {
		err := f.stateErrs[0]
		f.stateErrs = f.stateErrs[1:]
		if err != nil {
			return dockerx.State{}, err
		}
	}
	if f.onState != nil {
		f.onState()
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

var testBackup = dockerx.ContainerInfo{
	ID: "old-id", Name: "app" + BackupSuffix, Image: "example.com/app:latest",
}

// The bug this guards against: a SIGTERM during verification cancelled the
// rollback itself, stranding the backup container. The rollback must complete
// on the shutdown-immune ops context even though ctx is already cancelled.
func TestShutdownDuringVerifyStillRollsBack(t *testing.T) {
	f := newFake(dockerx.State{Running: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onState = cancel // shutdown arrives while the replacement is being watched

	res := testUpdater(f, time.Minute).Update(ctx, testContainer)

	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back", res.Action, res.Err)
	}
	if !slices.Contains(f.removed, "new-id") {
		t.Errorf("failed replacement was not removed; calls: %v", f.calls)
	}
	if f.renames["old-id"] != "app" || !slices.Contains(f.started, "old-id") {
		t.Errorf("original not restored after shutdown; renames: %v, started: %v", f.renames, f.started)
	}
}

func TestReconcileHealthyReplacementCommits(t *testing.T) {
	f := newFake(dockerx.State{Running: true, Health: "healthy"})
	res := testUpdater(f, time.Minute).Reconcile(context.Background(), testBackup)

	if res.Action != ActionUpdated {
		t.Fatalf("action = %q (%v), want updated", res.Action, res.Err)
	}
	if !slices.Contains(f.removed, "old-id") {
		t.Errorf("backup not removed after successful reconcile; calls: %v", f.calls)
	}
	if slices.Contains(f.removed, "new-id") {
		t.Errorf("verified replacement was removed")
	}
}

func TestReconcileStoppedReplacementRollsBack(t *testing.T) {
	f := newFake(dockerx.State{Running: false, ExitCode: 1})
	res := testUpdater(f, time.Minute).Reconcile(context.Background(), testBackup)

	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back", res.Action, res.Err)
	}
	if !slices.Contains(f.removed, "new-id") {
		t.Errorf("stopped replacement not removed; calls: %v", f.calls)
	}
	if f.renames["old-id"] != "app" || !slices.Contains(f.started, "old-id") {
		t.Errorf("backup not restored; renames: %v, started: %v", f.renames, f.started)
	}
	if slices.Contains(f.removed, "old-id") {
		t.Errorf("backup must never be removed on rollback")
	}
}

func TestReconcileUnhealthyReplacementRollsBack(t *testing.T) {
	f := newFake(
		dockerx.State{Running: true}, // the running-check before verification
		dockerx.State{Running: true, Health: "unhealthy"},
	)
	res := testUpdater(f, time.Minute).Reconcile(context.Background(), testBackup)
	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back", res.Action, res.Err)
	}
	if f.renames["old-id"] != "app" || !slices.Contains(f.started, "old-id") {
		t.Errorf("backup not restored; renames: %v, started: %v", f.renames, f.started)
	}
}

func TestReconcileMissingReplacementRestoresBackup(t *testing.T) {
	f := newFake(dockerx.State{Running: true})
	f.replacementMissing = true
	res := testUpdater(f, time.Minute).Reconcile(context.Background(), testBackup)

	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back", res.Action, res.Err)
	}
	if len(f.removed) != 0 {
		t.Errorf("nothing should be removed when replacement is missing; removed: %v", f.removed)
	}
	if f.renames["old-id"] != "app" || !slices.Contains(f.started, "old-id") {
		t.Errorf("backup not restored; renames: %v, started: %v", f.renames, f.started)
	}
}

func TestVerifyRetriesTransientInspectErrors(t *testing.T) {
	f := newFake(dockerx.State{Running: true, Health: "healthy"})
	f.stateErrs = []error{fmt.Errorf("EOF"), fmt.Errorf("EOF")} // two hiccups, then healthy
	res := testUpdater(f, time.Minute).Update(context.Background(), testContainer)
	if res.Action != ActionUpdated {
		t.Fatalf("action = %q (%v), want updated despite transient errors", res.Action, res.Err)
	}
}

func TestVerifyFailsAfterPersistentInspectErrors(t *testing.T) {
	f := newFake(dockerx.State{Running: true})
	f.stateErrs = []error{fmt.Errorf("EOF"), fmt.Errorf("EOF"), fmt.Errorf("EOF")}
	res := testUpdater(f, time.Minute).Update(context.Background(), testContainer)
	if res.Action != ActionRolledBack {
		t.Fatalf("action = %q (%v), want rolled_back after persistent errors", res.Action, res.Err)
	}
	if f.renames["old-id"] != "app" {
		t.Errorf("original not restored; renames: %v", f.renames)
	}
}
