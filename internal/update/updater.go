// Package update implements the safe update flow:
// pull → stop → recreate → verify → (commit | rollback).
package update

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mdschoff/updock/internal/dockerx"
)

// Action describes the outcome of an update attempt.
type Action string

const (
	ActionUpdated    Action = "updated"
	ActionRolledBack Action = "rolled_back"
	ActionError      Action = "error"
)

// Result reports what happened to one container.
type Result struct {
	Container dockerx.ContainerInfo
	Action    Action
	Detail    string
	Err       error
}

// Updater performs verified updates of single containers.
type Updater struct {
	Client       dockerx.Client
	VerifyWindow time.Duration // how long a container must look healthy before we commit
	StopTimeout  time.Duration // graceful stop timeout for the old container
	PollInterval time.Duration // how often to inspect during verification
}

// BackupSuffix marks the stopped original kept as the rollback target while
// a replacement is under verification.
const BackupSuffix = ".updock-backup"

// opsContext returns a context for state-changing docker calls that must be
// allowed to finish even when ctx is cancelled by a shutdown signal: once the
// running container has been taken apart, aborting halfway strands it in a
// half-updated state (the bug: a SIGTERM during verification used to cancel
// the rollback itself). Bounded so a wedged daemon can't hold us forever.
func opsContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
}

// Update replaces container c with a fresh container on the latest image of
// the same reference, verifies it, and rolls back to the original container
// if verification fails. The original container object (and therefore its
// image) is preserved until verification passes.
func (u *Updater) Update(ctx context.Context, c dockerx.ContainerInfo) Result {
	poll := u.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	backupName := c.Name + BackupSuffix

	// 1. Capture the container's user-intended config while the old image is
	// still readable (pulling may make it unreadable on the containerd store).
	spec, err := u.Client.CaptureSpec(ctx, c.ID)
	if err != nil {
		return errResult(c, "capturing container config", err)
	}

	// 2. Pull before touching anything: minimizes downtime, and a pull
	// failure aborts while the running container is still untouched.
	if err := u.Client.Pull(ctx, c.Image); err != nil {
		return errResult(c, "pulling new image", err)
	}

	// From here on the running container gets taken apart — every remaining
	// docker call runs on the shutdown-immune ops context.
	ops, done := opsContext(ctx)
	defer done()

	// 3. Stop the old container and move it out of the way, keeping it intact
	// as the rollback target.
	if err := u.Client.Stop(ops, c.ID, u.StopTimeout); err != nil {
		return errResult(c, "stopping old container", err)
	}
	if err := u.Client.Rename(ops, c.ID, backupName); err != nil {
		// Old container is stopped but unharmed; restart it and report.
		u.restoreOld(ops, c, false)
		return errResult(c, "renaming old container", err)
	}

	// 4. Create and start the replacement under the original name.
	newID, err := u.Client.CreateFromSpec(ops, spec, c.Name, c.Image)
	if err != nil {
		u.restoreOld(ops, c, true)
		return errResult(c, "creating replacement container", err)
	}
	if err := u.Client.Start(ops, newID); err != nil {
		_ = u.Client.Remove(ops, newID)
		u.restoreOld(ops, c, true)
		return errResult(c, "starting replacement container", err)
	}

	// 5. Verify. The wait itself stays cancellable (a shutdown should not sit
	// out the window), but a cancelled verification is treated as a failure
	// and the rollback below still runs to completion on the ops context.
	ok, reason := u.verify(ctx, ops, newID, poll)
	if ok {
		// 6a. Commit: the backup container is no longer needed. The old image
		// is left on disk on purpose — cheap insurance, prune policy is the
		// user's business.
		if err := u.Client.Remove(ops, c.ID); err != nil {
			slog.Warn("updated, but failed to remove backup container",
				"container", c.Name, "backup", backupName, "err", err)
		}
		return Result{Container: c, Action: ActionUpdated,
			Detail: "replacement passed verification"}
	}

	// 6b. Rollback: remove the failed replacement, restore the original.
	slog.Warn("verification failed, rolling back", "container", c.Name, "reason", reason)
	if err := u.Client.Stop(ops, newID, 5*time.Second); err != nil {
		slog.Debug("stopping failed replacement", "err", err)
	}
	if err := u.Client.Remove(ops, newID); err != nil {
		return errResult(c, "rollback: removing failed replacement", err)
	}
	u.restoreOld(ops, c, true)
	return Result{Container: c, Action: ActionRolledBack,
		Detail: fmt.Sprintf("new version failed verification (%s); previous version restored", reason)}
}

// Reconcile resolves update state left behind by an interrupted run (SIGKILL,
// power loss, or a crash before the ops-context hardening): a stopped
// *.updock-backup container whose update never concluded. The replacement, if
// it is running, gets a fresh verification window — so an interrupted update
// ends the same way it would have: committed or rolled back, never stranded.
func (u *Updater) Reconcile(ctx context.Context, backup dockerx.ContainerInfo) Result {
	poll := u.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	name := strings.TrimSuffix(backup.Name, BackupSuffix)
	orig := dockerx.ContainerInfo{ID: backup.ID, Name: name, Image: backup.Image}

	ops, done := opsContext(ctx)
	defer done()

	repl, found, err := u.Client.FindByName(ops, name)
	if err != nil {
		return errResult(orig, "reconcile: looking up replacement", err)
	}
	if !found {
		// The replacement vanished entirely — restore the backup outright.
		u.restoreOld(ops, orig, true)
		return Result{Container: orig, Action: ActionRolledBack,
			Detail: "interrupted update: replacement missing; previous version restored"}
	}

	st, err := u.Client.State(ops, repl.ID)
	if err != nil {
		return errResult(orig, "reconcile: inspecting replacement", err)
	}
	if st.Running {
		if ok, reason := u.verify(ctx, ops, repl.ID, poll); ok {
			if err := u.Client.Remove(ops, backup.ID); err != nil {
				slog.Warn("reconciled, but failed to remove backup container",
					"container", name, "err", err)
			}
			return Result{Container: orig, Action: ActionUpdated,
				Detail: "interrupted update: replacement verified; backup removed"}
		} else {
			slog.Warn("reconcile: replacement failed verification, rolling back",
				"container", name, "reason", reason)
			if err := u.Client.Stop(ops, repl.ID, 5*time.Second); err != nil {
				slog.Debug("stopping failed replacement", "err", err)
			}
		}
	}
	// Replacement exists but is stopped or failed verification: remove it and
	// restore the original.
	if err := u.Client.Remove(ops, repl.ID); err != nil {
		return errResult(orig, "reconcile: removing replacement", err)
	}
	u.restoreOld(ops, orig, true)
	return Result{Container: orig, Action: ActionRolledBack,
		Detail: "interrupted update: previous version restored"}
}

// restoreOld renames the backup container back to its original name (if
// renamed is true) and starts it again.
func (u *Updater) restoreOld(ctx context.Context, c dockerx.ContainerInfo, renamed bool) {
	if renamed {
		if err := u.Client.Rename(ctx, c.ID, c.Name); err != nil {
			slog.Error("rollback: renaming original container back", "container", c.Name, "err", err)
			return
		}
	}
	if err := u.Client.Start(ctx, c.ID); err != nil {
		slog.Error("rollback: restarting original container", "container", c.Name, "err", err)
	}
}

// verify watches a container for VerifyWindow. Verdicts:
//   - the container stops running or restarts        → fail immediately
//   - a Docker healthcheck reports unhealthy         → fail immediately
//   - a Docker healthcheck reports healthy           → pass immediately
//   - window elapses with no healthcheck, still up   → pass
//   - window elapses with healthcheck still starting → fail (window too short
//     or the app never came up — either way, not verified)
//   - ctx cancelled (shutdown) → fail, so the caller rolls back to known-good
//
// waitCtx is the cancellable context driving the wait; ops is the
// shutdown-immune context used for inspection calls. Restarts are judged
// against the container's count at entry, so a container that restarted in
// some earlier epoch (e.g. re-verified during reconciliation after a reboot)
// is not failed for its history.
func (u *Updater) verify(waitCtx, ops context.Context, id string, poll time.Duration) (bool, string) {
	deadline := time.Now().Add(u.VerifyWindow)
	baseline := -1
	inspectFailures := 0
	for {
		st, err := u.Client.State(ops, id)
		if err != nil {
			// A flaky daemon connection must not condemn a healthy
			// replacement — retry a few times before failing verification.
			inspectFailures++
			if inspectFailures >= 3 {
				return false, fmt.Sprintf("inspect failed %d times: %v", inspectFailures, err)
			}
			select {
			case <-waitCtx.Done():
				return false, "verification cancelled"
			case <-time.After(poll):
			}
			continue
		}
		inspectFailures = 0
		if baseline < 0 {
			baseline = st.RestartCount
		}
		if !st.Running {
			return false, fmt.Sprintf("container exited (code %d)", st.ExitCode)
		}
		if st.RestartCount > baseline {
			return false, "container restarted during verification"
		}
		switch st.Health {
		case "unhealthy":
			return false, "healthcheck reported unhealthy"
		case "healthy":
			return true, ""
		}
		if time.Now().After(deadline) {
			if st.Health == "starting" {
				return false, "healthcheck still 'starting' when verify window ended"
			}
			return true, ""
		}
		select {
		case <-waitCtx.Done():
			return false, "verification cancelled"
		case <-time.After(poll):
		}
	}
}

func errResult(c dockerx.ContainerInfo, stage string, err error) Result {
	return Result{Container: c, Action: ActionError,
		Detail: stage, Err: fmt.Errorf("%s: %w", stage, err)}
}
