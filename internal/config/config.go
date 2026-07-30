// Package config loads updock's YAML configuration with sane defaults, so
// running with no config file at all is fully supported.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode controls what updock does when it finds an update.
type Mode string

const (
	ModeAuto   Mode = "auto"   // update, verify, roll back on failure
	ModeNotify Mode = "notify" // only send a notification
	ModeHold   Mode = "hold"   // like notify, but phrased as awaiting approval
)

type Ntfy struct {
	URL   string `yaml:"url"`   // e.g. https://ntfy.sh
	Topic string `yaml:"topic"` // e.g. my-server-updates
}

type Webhook struct {
	URL string `yaml:"url"`
}

type Notify struct {
	Ntfy    *Ntfy    `yaml:"ntfy"`
	Webhook *Webhook `yaml:"webhook"`
}

type Config struct {
	Interval     time.Duration
	VerifyWindow time.Duration
	StopTimeout  time.Duration
	OptIn        bool // when true, only containers labeled updock.enable=true are managed
	DefaultMode  Mode
	Notify       Notify
}

// raw mirrors Config with string durations for YAML parsing.
type raw struct {
	Interval     string `yaml:"interval"`
	VerifyWindow string `yaml:"verify_window"`
	StopTimeout  string `yaml:"stop_timeout"`
	OptIn        *bool  `yaml:"opt_in"`
	DefaultMode  string `yaml:"default_mode"`
	Notify       Notify `yaml:"notify"`
}

// Default is the configuration used when no file is given.
func Default() Config {
	return Config{
		Interval:     6 * time.Hour,
		VerifyWindow: 90 * time.Second,
		StopTimeout:  30 * time.Second,
		OptIn:        false,
		DefaultMode:  ModeAuto,
	}
}

// Load reads path if non-empty, layering it over defaults. An empty path
// returns Default(). A missing file at an explicitly given path is an error.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	var r raw
	if err := yaml.Unmarshal(data, &r); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := setDur(&cfg.Interval, r.Interval, "interval"); err != nil {
		return cfg, err
	}
	if err := setDur(&cfg.VerifyWindow, r.VerifyWindow, "verify_window"); err != nil {
		return cfg, err
	}
	if err := setDur(&cfg.StopTimeout, r.StopTimeout, "stop_timeout"); err != nil {
		return cfg, err
	}
	if r.OptIn != nil {
		cfg.OptIn = *r.OptIn
	}
	if r.DefaultMode != "" {
		m := Mode(r.DefaultMode)
		if m != ModeAuto && m != ModeNotify && m != ModeHold {
			return cfg, fmt.Errorf("default_mode must be auto, notify or hold, got %q", r.DefaultMode)
		}
		cfg.DefaultMode = m
	}
	cfg.Notify = r.Notify
	return cfg, nil
}

func setDur(dst *time.Duration, val, field string) error {
	if val == "" {
		return nil
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s must be positive", field)
	}
	*dst = d
	return nil
}
