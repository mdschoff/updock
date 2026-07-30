// Package engine runs one full check-and-update cycle across all managed
// containers.
package engine

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/mdschoff/updock/internal/config"
	"github.com/mdschoff/updock/internal/dockerx"
	"github.com/mdschoff/updock/internal/notify"
	"github.com/mdschoff/updock/internal/update"
)

// Container labels understood by updock. For painless migration, the
// Watchtower enable label is honored as an alias.
const (
	LabelEnable           = "updock.enable" // "true" / "false"
	LabelMode             = "updock.mode"   // auto | notify | hold
	LabelSelf             = "updock.self"   // set on updock's own image; never self-update
	watchtowerEnableAlias = "com.centurylinklabs.watchtower.enable"
)

type Engine struct {
	Client   dockerx.Client
	Cfg      config.Config
	Notifier notify.Notifier
	Updater  *update.Updater
	DryRun   bool

	// warned tracks containers whose registry check already failed once, so
	// a permanently un-checkable container (private image without creds,
	// dangling local build) logs one warning, not one per cycle.
	warned map[string]bool
}

func New(cli dockerx.Client, cfg config.Config, n notify.Notifier) *Engine {
	return &Engine{
		Client:   cli,
		Cfg:      cfg,
		Notifier: n,
		Updater: &update.Updater{
			Client:       cli,
			VerifyWindow: cfg.VerifyWindow,
			StopTimeout:  cfg.StopTimeout,
		},
	}
}

// RunOnce checks every managed container and acts per its mode.
func (e *Engine) RunOnce(ctx context.Context) error {
	containers, err := e.Client.ListRunning(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.handle(ctx, c)
	}
	return nil
}

func (e *Engine) handle(ctx context.Context, c dockerx.ContainerInfo) {
	mode, manage := e.modeFor(c)
	if !manage {
		return
	}
	if strings.Contains(c.Image, "@") {
		slog.Debug("skipping digest-pinned container", "container", c.Name)
		return
	}

	remote, err := e.Client.RemoteDigest(ctx, c.Image)
	if err != nil {
		// Locally built images (no repo digest) routinely have no registry
		// counterpart — skip them quietly rather than warning every cycle.
		if st, serr := e.Client.ImageStatus(ctx, c.ID, c.Image, ""); serr == nil && st == dockerx.StatusLocalOnly {
			slog.Debug("locally built image, skipping", "container", c.Name)
			return
		}
		if e.warned == nil {
			e.warned = map[string]bool{}
		}
		if e.warned[c.ID] {
			slog.Debug("registry still unreachable, skipping", "container", c.Name)
		} else {
			e.warned[c.ID] = true
			slog.Warn("cannot check registry, skipping (further failures logged at debug)",
				"container", c.Name, "image", c.Image, "err", err)
		}
		return
	}
	delete(e.warned, c.ID)
	status, err := e.Client.ImageStatus(ctx, c.ID, c.Image, remote)
	if err != nil {
		slog.Warn("cannot determine image status, skipping", "container", c.Name, "err", err)
		return
	}
	switch status {
	case dockerx.StatusCurrent:
		return // up to date
	case dockerx.StatusLocalOnly:
		slog.Debug("locally built image, skipping", "container", c.Name)
		return
	}

	slog.Info("update available", "container", c.Name, "image", c.Image, "mode", mode)
	switch mode {
	case config.ModeNotify:
		e.emit(c, "update_available", "a newer image is available; updock is in notify-only mode")
	case config.ModeHold:
		e.emit(c, "held", "a newer image is available and is being held for manual approval")
	case config.ModeAuto:
		if e.DryRun {
			e.emit(c, "update_available", "dry-run: would update, verify and roll back on failure")
			return
		}
		res := e.Updater.Update(ctx, c)
		detail := res.Detail
		if res.Err != nil {
			detail = res.Err.Error()
		}
		e.emit(c, string(res.Action), detail)
	}
}

// modeFor decides whether a container is managed and in which mode.
func (e *Engine) modeFor(c dockerx.ContainerInfo) (config.Mode, bool) {
	if c.Labels[LabelSelf] == "true" {
		return "", false // never touch our own container
	}
	enable, hasEnable := c.Labels[LabelEnable]
	if !hasEnable {
		enable, hasEnable = c.Labels[watchtowerEnableAlias]
	}
	if hasEnable && enable == "false" {
		return "", false
	}
	if e.Cfg.OptIn && (!hasEnable || enable != "true") {
		return "", false
	}
	if m, ok := c.Labels[LabelMode]; ok {
		switch config.Mode(m) {
		case config.ModeAuto, config.ModeNotify, config.ModeHold:
			return config.Mode(m), true
		default:
			slog.Warn("invalid updock.mode label, using default",
				"container", c.Name, "label", m)
		}
	}
	return e.Cfg.DefaultMode, true
}

func (e *Engine) emit(c dockerx.ContainerInfo, action, detail string) {
	e.Notifier.Notify(context.Background(), notify.Event{
		Time:      time.Now(),
		Container: c.Name,
		Image:     c.Image,
		Action:    action,
		Detail:    detail,
	})
}
