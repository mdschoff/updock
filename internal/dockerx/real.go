package dockerx

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"net/netip"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Real is the production Client backed by the Docker Engine API.
type Real struct {
	cli *client.Client
}

// New connects to the Docker daemon using the standard environment
// (DOCKER_HOST etc.), negotiating the API version.
func New() (*Real, error) {
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connecting to docker: %w", err)
	}
	return &Real{cli: cli}, nil
}

// Ping verifies the daemon is reachable.
func (r *Real) Ping(ctx context.Context) error {
	_, err := r.cli.Ping(ctx, client.PingOptions{})
	return err
}

func (r *Real) ListRunning(ctx context.Context) ([]ContainerInfo, error) {
	list, err := r.cli.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(list.Items))
	for _, c := range list.Items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		imageRef := c.Image
		// When the tag a container was started from has since been re-pointed
		// at a newer image, the list API reports a bare image ID instead of
		// the tag. Fall back to the reference the container was created with.
		if strings.HasPrefix(imageRef, "sha256:") {
			if insp, err := r.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{}); err == nil && insp.Container.Config != nil {
				imageRef = insp.Container.Config.Image
			}
		}
		out = append(out, ContainerInfo{
			ID:     c.ID,
			Name:   name,
			Image:  imageRef,
			Labels: c.Labels,
		})
	}
	return out, nil
}

func (r *Real) RemoteDigest(ctx context.Context, imageRef string) (string, error) {
	insp, err := r.cli.DistributionInspect(ctx, imageRef, client.DistributionInspectOptions{})
	if err != nil {
		return "", err
	}
	return string(insp.Descriptor.Digest), nil
}

func (r *Real) ImageStatus(ctx context.Context, containerID, imageRef, remoteDigest string) (ImageStatus, error) {
	cont, err := r.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	// containerd image store: the container's image ID is the manifest digest
	// itself, so a direct match settles it.
	if cont.Container.Image == remoteDigest {
		return StatusCurrent, nil
	}
	img, err := r.cli.ImageInspect(ctx, cont.Container.Image)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			// containerd image store: when a tag is re-pointed, the
			// superseded manifest becomes uninspectable. The image clearly
			// came from a registry and clearly isn't what it serves now.
			return StatusStale, nil
		}
		return "", err
	}
	if len(img.RepoDigests) == 0 {
		return StatusLocalOnly, nil
	}
	repo := repoOf(imageRef)
	for _, rd := range img.RepoDigests {
		// RepoDigests entries look like "registry/repo@sha256:...".
		if at := strings.LastIndex(rd, "@"); at > 0 {
			if repoOf(rd[:at]) == repo && rd[at+1:] == remoteDigest {
				return StatusCurrent, nil
			}
		}
	}
	return StatusStale, nil
}

// repoOf normalizes an image reference to its repository name without tag or
// digest, so "nginx:1.27" and "docker.io/library/nginx@sha256:x" compare equal.
func repoOf(imageRef string) string {
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return imageRef
	}
	return reference.FamiliarName(named)
}

func (r *Real) Pull(ctx context.Context, imageRef string) error {
	resp, err := r.cli.ImagePull(ctx, imageRef, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer resp.Close()
	// The pull only completes once the response stream is consumed.
	return resp.Wait(ctx)
}

func (r *Real) Stop(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	_, err := r.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &secs})
	return err
}

func (r *Real) Start(ctx context.Context, id string) error {
	_, err := r.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (r *Real) Rename(ctx context.Context, id, name string) error {
	_, err := r.cli.ContainerRename(ctx, id, client.ContainerRenameOptions{NewName: name})
	return err
}

// CloneSpec is a captured, sanitized container configuration ready to be
// re-created on a different image.
type CloneSpec struct {
	cfg  *container.Config
	host *container.HostConfig
	net  *network.NetworkingConfig
}

func (r *Real) CaptureSpec(ctx context.Context, containerID string) (*CloneSpec, error) {
	insp, err := r.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	src := insp.Container
	cfg := src.Config

	if img, err := r.cli.ImageInspect(ctx, src.Image); err == nil && img.Config != nil {
		stripImageDefaults(cfg, img.Config)
	} else {
		slog.Warn("current image config unreadable; changed image defaults (CMD, ENV, …) may not be picked up",
			"container", strings.TrimPrefix(src.Name, "/"), "err", err)
	}

	var netCfg *network.NetworkingConfig
	if src.NetworkSettings != nil && len(src.NetworkSettings.Networks) > 0 {
		endpoints := make(map[string]*network.EndpointSettings, len(src.NetworkSettings.Networks))
		for netName, ep := range src.NetworkSettings.Networks {
			// Copy user intent (aliases, static IPs via IPAMConfig) but drop
			// runtime-assigned state so the daemon allocates fresh values.
			cp := *ep
			cp.EndpointID = ""
			cp.NetworkID = ""
			cp.IPAddress = netip.Addr{}
			cp.Gateway = netip.Addr{}
			cp.GlobalIPv6Address = netip.Addr{}
			cp.IPv6Gateway = netip.Addr{}
			cp.MacAddress = nil
			endpoints[netName] = &cp
		}
		netCfg = &network.NetworkingConfig{EndpointsConfig: endpoints}
	}

	return &CloneSpec{cfg: cfg, host: src.HostConfig, net: netCfg}, nil
}

// stripImageDefaults removes from cfg every value the container merely
// inherited from its image, so the daemon re-resolves those from the NEW
// image at create time. Values that differ from the image's are user
// overrides and are kept.
func stripImageDefaults(cfg *container.Config, img *dockerspec.DockerOCIImageConfig) {
	if slices.Equal([]string(cfg.Cmd), img.Cmd) {
		cfg.Cmd = nil
	}
	if slices.Equal([]string(cfg.Entrypoint), img.Entrypoint) {
		cfg.Entrypoint = nil
	}
	if cfg.WorkingDir == img.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == img.User {
		cfg.User = ""
	}
	if cfg.StopSignal == img.StopSignal {
		cfg.StopSignal = ""
	}
	if cfg.Healthcheck != nil && img.Healthcheck != nil &&
		slices.Equal(cfg.Healthcheck.Test, img.Healthcheck.Test) {
		cfg.Healthcheck = nil
	}
	imgEnv := make(map[string]bool, len(img.Env))
	for _, e := range img.Env {
		imgEnv[e] = true
	}
	kept := cfg.Env[:0]
	for _, e := range cfg.Env {
		if !imgEnv[e] {
			kept = append(kept, e)
		}
	}
	cfg.Env = kept
	for k, v := range img.Labels {
		if cfg.Labels[k] == v {
			delete(cfg.Labels, k)
		}
	}
	for p := range img.ExposedPorts {
		if port, err := network.ParsePort(p); err == nil {
			delete(cfg.ExposedPorts, port)
		}
	}
	for v := range img.Volumes {
		delete(cfg.Volumes, v)
	}
}

func (r *Real) CreateFromSpec(ctx context.Context, spec *CloneSpec, name, imageRef string) (string, error) {
	spec.cfg.Image = imageRef
	resp, err := r.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           spec.cfg,
		HostConfig:       spec.host,
		NetworkingConfig: spec.net,
		Name:             name,
	})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (r *Real) Remove(ctx context.Context, id string) error {
	_, err := r.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	return err
}

func (r *Real) State(ctx context.Context, id string) (State, error) {
	insp, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return State{}, err
	}
	st := State{}
	if insp.Container.State != nil {
		st.Running = insp.Container.State.Running
		st.RestartCount = insp.Container.RestartCount
		st.ExitCode = insp.Container.State.ExitCode
		if insp.Container.State.Health != nil {
			st.Health = strings.ToLower(string(insp.Container.State.Health.Status))
		}
	}
	return st, nil
}
