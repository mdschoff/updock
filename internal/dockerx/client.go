// Package dockerx wraps the Docker Engine API behind a small interface so the
// update logic can be tested without a daemon.
package dockerx

import (
	"context"
	"time"
)

// ContainerInfo is the subset of container metadata updock cares about.
type ContainerInfo struct {
	ID     string
	Name   string // without the leading slash
	Image  string // the reference the container was created from, e.g. "nginx:1.27"
	Labels map[string]string
}

// State is a point-in-time view of a container used during verification.
type State struct {
	Running      bool
	RestartCount int
	ExitCode     int
	Health       string // "", "starting", "healthy" or "unhealthy"
}

// ImageStatus classifies a container's image relative to the registry.
type ImageStatus string

const (
	// StatusCurrent: the container runs the image the registry serves.
	StatusCurrent ImageStatus = "current"
	// StatusStale: the registry serves a newer image.
	StatusStale ImageStatus = "stale"
	// StatusLocalOnly: the container runs a locally built image that never
	// touched a registry; updock must leave it alone.
	StatusLocalOnly ImageStatus = "local-only"
)

// Client is the set of Docker operations the engine needs.
type Client interface {
	// ListRunning returns all currently running containers.
	ListRunning(ctx context.Context) ([]ContainerInfo, error)
	// RemoteDigest returns the manifest digest the registry currently serves
	// for the given image reference, without pulling it.
	RemoteDigest(ctx context.Context, imageRef string) (string, error)
	// ImageStatus reports whether the container's image matches remoteDigest.
	// It must handle both the classic image store (image ID ≠ manifest
	// digest, RepoDigests populated) and the containerd image store (image
	// ID == manifest digest; superseded manifests become uninspectable).
	// remoteDigest may be "" to probe only for StatusLocalOnly.
	ImageStatus(ctx context.Context, containerID, imageRef, remoteDigest string) (ImageStatus, error)
	// Pull downloads an image.
	Pull(ctx context.Context, imageRef string) error
	// Stop stops a container gracefully, killing it after timeout.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	// Start starts a created or stopped container.
	Start(ctx context.Context, id string) error
	// Rename renames a container.
	Rename(ctx context.Context, id, name string) error
	// CaptureSpec snapshots the user-intended configuration of a container:
	// its full config minus everything inherited from its current image, so
	// that a replacement created from a newer image picks up the new image's
	// defaults (CMD, ENV, …) while preserving user overrides. Must be called
	// BEFORE pulling the new image: pulling can re-point the tag and (on the
	// containerd image store) make the old image's config unreadable.
	CaptureSpec(ctx context.Context, containerID string) (*CloneSpec, error)
	// CreateFromSpec creates (but does not start) a container named name from
	// a captured spec, using imageRef as its image.
	CreateFromSpec(ctx context.Context, spec *CloneSpec, name, imageRef string) (string, error)
	// Remove force-removes a container.
	Remove(ctx context.Context, id string) error
	// State inspects the current state of a container.
	State(ctx context.Context, id string) (State, error)
}
