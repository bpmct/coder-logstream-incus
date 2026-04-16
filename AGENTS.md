# Agent / Developer Notes

This file documents non-obvious implementation decisions and known gotchas for future agents or contributors working on this codebase.

---

## Repository layout

| File | Purpose |
|------|---------|
| `main.go` | CLI entrypoint using [`serpent`](https://github.com/coder/serpent). Parses flags, constructs options, starts `incusLogStreamer`. |
| `logger.go` | All core logic: instance watcher, per-VM streaming goroutines, Coder log sender. |
| `install.sh` | One-liner installer for the systemd service. Downloads binary from GitHub Releases. |
| `.github/workflows/release.yml` | Builds `linux/amd64` + `linux/arm64` binaries and publishes a GitHub Release on `v*` tags. |

---

## How the daemon detects workspaces

The daemon calls `incus list` (via `GetInstances`) every 5 seconds. It looks for instances that have **either**:

- `user.coder-agent-token` in their config (set by the Terraform provider via `incus config set`)
- `environment.CODER_AGENT_TOKEN` (fallback for cloud-init environment injection)

Only instances with a non-empty token get a streaming goroutine. The token is read from `inst.Config` first, then `inst.ExpandedConfig` (which includes profile-inherited values).

---

## The `done` map — why it exists and what it tracks

`done map[string]string` maps **instance name → agent token that already completed streaming**.

When a workspace restarts, Incus reuses the same VM name but issues a new agent token. Without the `done` map the daemon would never restart streaming. With a naive `done map[string]struct{}` (name only) the daemon would also never restart — it would see the name as already done regardless of the new token.

The correct invariant: skip streaming only if `done[name] == currentToken`. On workspace restart the token changes, so the condition fails and a new streamer goroutine starts.

The `done` map is also garbage-collected in `reconcile`: entries for instances that have disappeared are deleted so the map doesn't grow unboundedly.

---

## Connection retry loop

`streamInstance` retries the Coder agent API (`ConnectRPC28WithRole` / `ConnectRPC20`) every 10 seconds until successful or the context is cancelled. This is necessary because:

1. The Coder build job may not have started yet when the VM first appears in the Incus list.
2. The agent token is not valid until the build job issues it.

The retry loop calls `BuildInfo` first to detect the Coder server version and select the right RPC variant. On Coder ≥ v2.31.0, `ConnectRPC28WithRole("logstream-incus")` is used to avoid triggering false agent connectivity state changes in the UI.

---

## Console log vs. cloud-init log

| Source | API | Works for |
|--------|-----|-----------|
| Console buffer | `GetInstanceConsoleLog` | Containers only |
| `/var/log/cloud-init-output.log` | `GetInstanceFile` | VMs and containers |

VMs do not expose the console log buffer via the Incus API — `GetInstanceConsoleLog` returns an error for VMs and that error is silently ignored at `Debug` level. The daemon always falls through to the cloud-init tail regardless.

The Incus file API does not support range reads, so the daemon manually discards already-sent bytes on each poll by reading and discarding `offset` bytes before processing new content.

---

## Graceful shutdown / flush

When the context is cancelled (daemon shutdown or workspace stop), `streamInstance` returns `false` (not completed). The goroutine wrapper does **not** add that instance to the `done` map, so the next reconcile cycle can restart streaming.

When cloud-init finishes normally, `tailCloudInit` returns `true`, `streamInstance` returns `true`, and the instance is added to `done[name] = token`. The daemon then calls `ls.Flush(sourceUUID)` + `ls.WaitUntilEmpty` with a 10-second timeout to drain the log queue before closing the RPC connection.

---

## systemd service

The production service on the ThinkStation runs as:

```
ExecStart=/usr/local/bin/coder-logstream-incus --coder-url https://coder.bpmct.net
Environment=INCUS_PROJECT=default
```

The binary must run as a user with access to the Incus Unix socket (`/var/lib/incus/unix.socket`). On most setups this means running as root or a member of the `incus-admin` group.

---

## Building

```bash
go build -o coder-logstream-incus .
```

Cross-compile for the ThinkStation (amd64):

```bash
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o coder-logstream-incus-linux-amd64 .
```

Requires Go 1.22+. The module is `github.com/bpmct/coder-logstream-incus`.

---

## Releasing

1. Commit and push to `main`.
2. Create and push a `v*` tag — the GitHub Actions workflow builds both arches and publishes the release automatically.

```bash
git tag v0.x.y
git push origin v0.x.y
```

Do **not** push local binary artifacts (`coder-logstream-incus`, `coder-logstream-incus-amd64`, `coder-logstream-incus-linux-amd64`) — they are in `.gitignore`.
