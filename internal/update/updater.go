// Package update implements the safe update flow:
// pull → stop → recreate → verify → (commit | rollback).
package update

import (
	"context"
	"fmt"
	"log/slog"
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

const backupSuffix = ".updock-backup"

// Update replaces container c with a fresh container on the latest image of
// the same reference, verifies it, and rolls back to the original container
// if verification fails. The original container object (and therefore its
// image) is preserved until verification passes.
func (u *Updater) Update(ctx context.Context, c dockerx.ContainerInfo) Result {
	poll := u.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	backupName := c.Name + backupSuffix

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

	// 3. Stop the old container and move it out of the way, keeping it intact
	// as the rollback target.
	if err := u.Client.Stop(ctx, c.ID, u.StopTimeout); err != nil {
		return errResult(c, "stopping old container", err)
	}
	if err := u.Client.Rename(ctx, c.ID, backupName); err != nil {
		// Old container is stopped but unharmed; restart it and report.
		u.restoreOld(ctx, c, false)
		return errResult(c, "renaming old container", err)
	}

	// 4. Create and start the replacement under the original name.
	newID, err := u.Client.CreateFromSpec(ctx, spec, c.Name, c.Image)
	if err != nil {
		u.restoreOld(ctx, c, true)
		return errResult(c, "creating replacement container", err)
	}
	if err := u.Client.Start(ctx, newID); err != nil {
		_ = u.Client.Remove(ctx, newID)
		u.restoreOld(ctx, c, true)
		return errResult(c, "starting replacement container", err)
	}

	// 5. Verify.
	ok, reason := u.verify(ctx, newID, poll)
	if ok {
		// 6a. Commit: the backup container is no longer needed. The old image
		// is left on disk on purpose — cheap insurance, prune policy is the
		// user's business.
		if err := u.Client.Remove(ctx, c.ID); err != nil {
			slog.Warn("updated, but failed to remove backup container",
				"container", c.Name, "backup", backupName, "err", err)
		}
		return Result{Container: c, Action: ActionUpdated,
			Detail: "replacement passed verification"}
	}

	// 6b. Rollback: remove the failed replacement, restore the original.
	slog.Warn("verification failed, rolling back", "container", c.Name, "reason", reason)
	if err := u.Client.Stop(ctx, newID, 5*time.Second); err != nil {
		slog.Debug("stopping failed replacement", "err", err)
	}
	if err := u.Client.Remove(ctx, newID); err != nil {
		return errResult(c, "rollback: removing failed replacement", err)
	}
	u.restoreOld(ctx, c, true)
	return Result{Container: c, Action: ActionRolledBack,
		Detail: fmt.Sprintf("new version failed verification (%s); previous version restored", reason)}
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
func (u *Updater) verify(ctx context.Context, id string, poll time.Duration) (bool, string) {
	deadline := time.Now().Add(u.VerifyWindow)
	for {
		st, err := u.Client.State(ctx, id)
		if err != nil {
			return false, fmt.Sprintf("inspect failed: %v", err)
		}
		if !st.Running {
			return false, fmt.Sprintf("container exited (code %d)", st.ExitCode)
		}
		if st.RestartCount > 0 {
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
		case <-ctx.Done():
			return false, "verification cancelled"
		case <-time.After(poll):
		}
	}
}

func errResult(c dockerx.ContainerInfo, stage string, err error) Result {
	return Result{Container: c, Action: ActionError,
		Detail: stage, Err: fmt.Errorf("%s: %w", stage, err)}
}
