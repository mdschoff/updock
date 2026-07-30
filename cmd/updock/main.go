// updock keeps Docker containers up to date — and rolls them back when an
// update breaks. See https://github.com/mdschoff/updock
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdschoff/updock/internal/config"
	"github.com/mdschoff/updock/internal/dockerx"
	"github.com/mdschoff/updock/internal/engine"
	"github.com/mdschoff/updock/internal/notify"
)

var version = "dev"

const defaultConfigPath = "/etc/updock/updock.yml"

func main() {
	cfgPath := flag.String("config", "", "path to YAML config (default: "+defaultConfigPath+" if present)")
	once := flag.Bool("once", false, "run a single check-and-update cycle, then exit")
	dryRun := flag.Bool("dry-run", false, "report what would happen without changing anything")
	interval := flag.Duration("interval", 0, "override poll interval, e.g. 30m")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("updock", version)
		return
	}

	path := *cfgPath
	if path == "" {
		if _, err := os.Stat(defaultConfigPath); err == nil {
			path = defaultConfigPath
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		fatal("loading config", err)
	}
	if *interval > 0 {
		cfg.Interval = *interval
	}

	cli, err := dockerx.New()
	if err != nil {
		fatal("connecting to docker", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cli.Ping(ctx); err != nil {
		fatal("docker daemon unreachable (is /var/run/docker.sock mounted?)", err)
	}

	eng := engine.New(cli, cfg, notify.FromConfig(cfg))
	eng.DryRun = *dryRun

	if *once {
		if err := eng.RunOnce(ctx); err != nil {
			fatal("run failed", err)
		}
		return
	}

	slog.Info("updock started", "version", version,
		"interval", cfg.Interval, "verify_window", cfg.VerifyWindow,
		"default_mode", cfg.DefaultMode, "opt_in", cfg.OptIn, "dry_run", *dryRun)
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if err := eng.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("cycle failed", "err", err)
		}
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-ticker.C:
		}
	}
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "updock: %s: %v\n", msg, err)
	os.Exit(1)
}
