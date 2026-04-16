# coder-logstream-incus

Stream Incus VM console and cloud-init logs to [Coder](https://coder.com) workspace startup logs — analogous to [`coder-logstream-kube`](https://github.com/coder/coder-logstream-kube) for Kubernetes.

## How it works

The daemon runs on the same host as `incusd`. It polls the Incus API every 5 seconds for instances that have the `user.coder-agent-token` config key set. When a new instance is found, it:

1. **Dumps the console log buffer** (full boot output via `incus console --show-log`)
2. **Tails `/var/log/cloud-init-output.log`** via the Incus file API, streaming new lines every 2 seconds
3. Stops streaming once `/run/cloud-init/result.json` exists (cloud-init has finished) or the instance is removed

All output is sent to the Coder agent's startup logs so it appears in the workspace build log UI.

## Usage

### Running the daemon

```bash
coder-logstream-incus \
  --coder-url https://coder.example.com \
  --project default
```

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--coder-url` | `CODER_URL` | — | URL of the Coder deployment |
| `--socket` | `INCUS_SOCKET` | (auto) | Path to Incus Unix socket |
| `--project` | `INCUS_PROJECT` | `default` | Incus project to watch |
| `--poll-interval` | `CODER_INCUS_POLL_INTERVAL` | `5s` | How often to poll for instances |

### Coder template integration

In your Incus VM Coder template, set the `user.coder-agent-token` config on the instance so the daemon can detect it:

```hcl
resource "incus_instance" "vm" {
  name    = "coder-${data.coder_workspace.me.id}"
  image   = "ubuntu/jammy/cloud"
  type    = "virtual-machine"
  project = "default"

  config = {
    "user.coder-agent-token" = data.coder_workspace_agent.agent.token
    # cloud-init for agent startup
    "user.user-data" = templatefile("${path.module}/cloud-init.yaml", {
      agent_token = data.coder_workspace_agent.agent.token
      coder_url   = data.coder_workspace.me.access_url
    })
  }
}
```

### Running as a systemd service

```ini
[Unit]
Description=Coder Incus Log Streamer
After=network.target incus.service

[Service]
ExecStart=/usr/local/bin/coder-logstream-incus --coder-url https://coder.example.com
Restart=on-failure
RestartSec=5s
Environment=INCUS_PROJECT=default

[Install]
WantedBy=multi-user.target
```

## Building

```bash
go build -o coder-logstream-incus .
```

Requires Go 1.22+.

## Architecture

- **`main.go`** — CLI entrypoint using [`serpent`](https://github.com/coder/serpent)
- **`logger.go`** — Core logic: instance watcher, per-VM streaming goroutines, log sender

The daemon uses the [Coder agent SDK](https://pkg.go.dev/github.com/coder/coder/v2/codersdk/agentsdk) to POST logs. On Coder v2.31.0+, it uses `ConnectRPC28WithRole("logstream-incus")` to avoid triggering false agent connectivity state changes.
