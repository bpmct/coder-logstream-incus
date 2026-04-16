# Changelog

## [v0.1.2] - 2026-04-16

### Added
- GitHub Actions release workflow (`.github/workflows/release.yml`) — triggers on `v*` tags, builds `linux/amd64` and `linux/arm64` binaries, and publishes them via `softprops/action-gh-release`
- `install.sh` one-liner installer — detects arch, downloads the latest binary from GitHub Releases, writes a systemd unit, and calls `systemctl enable --now`
- README: installation section with one-liner, options table, and CI badge

### Fixed
- Re-stream instances when the agent token changes on workspace restart. The `done` map was previously keyed by instance name alone, so a restarted workspace (same VM name, new token) would not be re-streamed. It is now keyed by `name → token`, so streaming only skips if the exact token already completed.

---

## [v0.1.1] - 2026-04-14

### Fixed
- `streamInstance` no longer returns early on connect errors. It now retries the Coder agent API connection every 10 seconds until the context is cancelled. This handles the case where the workspace token is not yet authorized when the VM first appears.
- Log source registration errors are now non-fatal (logs continue to stream without custom source metadata).
- Console log fetch errors for VMs are now logged at `Debug` instead of `Warn` — the console buffer API is only supported for containers, not VMs.
- `done` map was incorrectly preventing restart of completed streamers across reconcile cycles (related to the v0.1.2 fix above; partial fix introduced here).
- `sendLine` now accepts a `LogLevel` parameter so future log lines can be sent at `Warn`/`Error` when appropriate.
- `tailCloudInit` and `streamInstance` now return a boolean indicating whether cloud-init completed normally, enabling the `done` map to correctly skip re-streaming only on clean completion.
- Graceful flush uses `ls.WaitUntilEmpty` instead of `sl.Flush` to drain the log queue before closing the connection.

### Changed
- Upgraded to `ConnectRPC28WithRole("logstream-incus")` on Coder ≥ v2.31.0 to avoid triggering false agent connectivity state changes. Falls back to `ConnectRPC20` on older servers.
- Added `golang.org/x/mod/semver` for version comparison.
- Completion message changed from `"cloud-init: finished (result.json present)"` to `"cloud-init: finished ✓"`.

---

## [v0.1.0] - 2026-04-13

### Added
- Initial implementation of `coder-logstream-incus`.
- Polls the Incus API every 5 seconds for instances with `user.coder-agent-token` set.
- Per-instance goroutine that:
  1. Dumps the console log buffer (containers only).
  2. Tails `/var/log/cloud-init-output.log` via the Incus file API every 2 seconds.
  3. Stops when `/run/cloud-init/result.json` exists or the instance is removed.
- CLI entrypoint via [`serpent`](https://github.com/coder/serpent) with `--coder-url`, `--socket`, `--project`, and `--poll-interval` flags.
- Stable log source UUID (`a3bb5c89-7f3c-4f58-b6d3-a3c5e7b1f0d2`) for the Coder workspace log UI.
