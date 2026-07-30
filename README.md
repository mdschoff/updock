# updock

**Automatic Docker container updates that can't leave you broken.**

updock watches your running containers, updates them when their image gets a new release — and if the new version crashes or fails its healthcheck, **automatically rolls back to the version that worked** and tells you about it.

```
pull → swap → verify → keep it   (or: verify fails → roll back → notify)
```

## Why

[Watchtower](https://github.com/containrrr/watchtower) was archived in December 2025. But even before that, blind auto-updates were the reason many people never dared to enable it: an update that breaks at 3am stays broken until you notice. The remaining alternatives either only *notify* you (Diun) or don't verify and roll back either.

updock's position: **auto-updates are only trustworthy if the updater can undo them.** Every update is verified — the replacement container must pass its Docker healthcheck (or at minimum stay up) during a verification window before the old container is discarded. If it doesn't, the previous version is restored automatically, and you get a notification with the reason instead of an outage.

## Quick start

```yaml
# docker-compose.yml
services:
  updock:
    image: ghcr.io/mdschoff/updock:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      # optional config:
      - ./updock.yml:/etc/updock/updock.yml:ro
```

That's it. Every running container is now checked every 6 hours and safely updated. Containers you build locally are detected and left alone.

Or run it once from your shell to see what it would do:

```bash
updock --once --dry-run
```

## How verification works

After swapping in the new container, updock watches it for a verification window (default 90s):

| Observation | Verdict |
|---|---|
| Docker healthcheck reports `healthy` | ✅ update committed immediately |
| healthcheck reports `unhealthy` | ❌ rollback |
| container exits or restarts | ❌ rollback |
| no healthcheck, still running at window end | ✅ update committed |
| healthcheck still `starting` at window end | ❌ rollback (not verified = not kept) |

The old container is kept intact (stopped, renamed `<name>.updock-backup`) until verification passes, so rollback is a rename + start — no re-pull, no data loss. Volumes, networks, port mappings, env vars and labels are preserved; defaults that come from the image itself (CMD, ENV, …) correctly follow the **new** image.

## Configuration

Everything works with zero config. To customize, mount a YAML file at `/etc/updock/updock.yml`:

```yaml
interval: 6h          # how often to check registries
verify_window: 90s    # how long a new container must prove itself
stop_timeout: 30s     # graceful stop for the old container
opt_in: false         # true = only manage containers labeled updock.enable=true
default_mode: auto    # auto | notify | hold
notify:
  ntfy:
    url: https://ntfy.sh
    topic: my-updates
  webhook:
    url: https://example.com/hook
```

### Per-container labels

| Label | Effect |
|---|---|
| `updock.enable=false` | never touch this container |
| `updock.enable=true` | manage this container (required when `opt_in: true`) |
| `updock.mode=notify` | only tell me when an update exists |
| `updock.mode=hold` | tell me and wait for manual approval |
| `updock.mode=auto` | update + verify + rollback (default) |

Containers pinned by digest (`image@sha256:…`) and locally built images are always skipped.

## Migrating from Watchtower

updock honors `com.centurylinklabs.watchtower.enable` labels, so your existing exclusions keep working — swap the service in your compose file and you're done. What you gain: updates are verified and rolled back on failure instead of applied blind.

## Comparison

|  | updock | Watchtower (archived) | Diun | WUD |
|---|---|---|---|---|
| Auto-update | ✅ | ✅ | ❌ notify only | ⚠️ via triggers |
| Health verification after update | ✅ | ❌ | — | ❌ |
| Automatic rollback | ✅ | ❌ | — | ❌ |
| Single binary, no database | ✅ | ✅ | ✅ | ❌ |
| Maintained | ✅ | ❌ | ✅ | ✅ |

## Building & testing

```bash
go build ./cmd/updock
go test ./internal/...          # unit tests (no docker needed)
go test ./e2e/                  # full end-to-end test against your local docker
```

The e2e test spins up a throwaway local registry, pushes a good image, verifies updock updates to it, then pushes a broken image and verifies updock rolls back.

## Roadmap

- Podman support
- Changelog/release-notes diff in notifications
- Registry auth (private images)
- `updock approve` for held updates
- Discord/Slack/Gotify notifiers

## License

MIT
