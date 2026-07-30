// Package notify fans update events out to configured sinks (log, ntfy,
// generic webhook).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mdschoff/updock/internal/config"
)

// Event is one user-relevant occurrence.
type Event struct {
	Time      time.Time `json:"time"`
	Container string    `json:"container"`
	Image     string    `json:"image"`
	Action    string    `json:"action"` // updated | rolled_back | update_available | held | error
	Detail    string    `json:"detail,omitempty"`
}

// Title renders a short human headline for the event.
func (e Event) Title() string {
	switch e.Action {
	case "updated":
		return fmt.Sprintf("✅ %s updated", e.Container)
	case "rolled_back":
		return fmt.Sprintf("↩️ %s rolled back", e.Container)
	case "update_available":
		return fmt.Sprintf("⬆️ update available for %s", e.Container)
	case "held":
		return fmt.Sprintf("⏸ %s: update held for approval", e.Container)
	default:
		return fmt.Sprintf("⚠️ %s: update error", e.Container)
	}
}

// Notifier delivers events. Implementations must not block for long.
type Notifier interface {
	Notify(ctx context.Context, e Event)
}

// Multi fans out to several notifiers.
type Multi []Notifier

func (m Multi) Notify(ctx context.Context, e Event) {
	for _, n := range m {
		n.Notify(ctx, e)
	}
}

// FromConfig builds the notifier stack. The log notifier is always included.
func FromConfig(cfg config.Config) Notifier {
	ns := Multi{Log{}}
	if n := cfg.Notify.Ntfy; n != nil && n.URL != "" && n.Topic != "" {
		ns = append(ns, &Ntfy{URL: strings.TrimRight(n.URL, "/"), Topic: n.Topic})
	}
	if w := cfg.Notify.Webhook; w != nil && w.URL != "" {
		ns = append(ns, &Webhook{URL: w.URL})
	}
	return ns
}

// Log writes events to the structured log.
type Log struct{}

func (Log) Notify(_ context.Context, e Event) {
	slog.Info("event", "action", e.Action, "container", e.Container, "image", e.Image, "detail", e.Detail)
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// Ntfy publishes to an ntfy.sh-compatible server.
type Ntfy struct {
	URL   string
	Topic string
}

func (n *Ntfy) Notify(ctx context.Context, e Event) {
	body := e.Detail
	if body == "" {
		body = e.Image
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.URL+"/"+n.Topic, strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Title", e.Title())
	if e.Action == "rolled_back" || e.Action == "error" {
		req.Header.Set("Priority", "high")
	}
	if resp, err := httpClient.Do(req); err != nil {
		slog.Warn("ntfy notification failed", "err", err)
	} else {
		resp.Body.Close()
	}
}

// Webhook POSTs the event as JSON to an arbitrary URL.
type Webhook struct {
	URL string
}

func (w *Webhook) Notify(ctx context.Context, e Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := httpClient.Do(req); err != nil {
		slog.Warn("webhook notification failed", "err", err)
	} else {
		resp.Body.Close()
	}
}
