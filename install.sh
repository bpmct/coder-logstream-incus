#!/usr/bin/env bash
# install.sh — install coder-logstream-incus as a systemd service
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/bpmct/coder-logstream-incus/main/install.sh | \
#     CODER_URL=https://coder.example.com bash
#
# Optional env vars:
#   CODER_URL         (required) URL of your Coder deployment
#   INCUS_PROJECT     Incus project to watch (default: default)
#   INSTALL_DIR       Where to install the binary (default: /usr/local/bin)
#   VERSION           Release tag to install (default: latest)

set -euo pipefail

CODER_URL="${CODER_URL:-}"
INCUS_PROJECT="${INCUS_PROJECT:-default}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${VERSION:-}"

if [[ -z "$CODER_URL" ]]; then
  echo "Error: CODER_URL is required." >&2
  echo "  CODER_URL=https://coder.example.com bash install.sh" >&2
  exit 1
fi

# Detect arch
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  *)
    echo "Error: unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

BINARY="coder-logstream-incus-linux-${ARCH}"

# Resolve version
if [[ -z "$VERSION" ]]; then
  VERSION="$(curl -fsSL https://api.github.com/repos/bpmct/coder-logstream-incus/releases/latest | grep '"tag_name"' | cut -d'"' -f4)"
fi

echo "Installing coder-logstream-incus ${VERSION} (${ARCH})..."

# Download binary
TMP="$(mktemp)"
curl -fsSL "https://github.com/bpmct/coder-logstream-incus/releases/download/${VERSION}/${BINARY}" -o "$TMP"
chmod +x "$TMP"
mv "$TMP" "${INSTALL_DIR}/coder-logstream-incus"

echo "Binary installed to ${INSTALL_DIR}/coder-logstream-incus"

# Write systemd unit
cat > /etc/systemd/system/coder-logstream-incus.service <<EOF
[Unit]
Description=Coder Incus Log Streamer
After=network.target incus.service

[Service]
ExecStart=${INSTALL_DIR}/coder-logstream-incus --coder-url ${CODER_URL}
Restart=on-failure
RestartSec=5s
Environment=INCUS_PROJECT=${INCUS_PROJECT}

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now coder-logstream-incus

echo ""
echo "coder-logstream-incus is running."
echo "  Status: systemctl status coder-logstream-incus"
echo "  Logs:   journalctl -u coder-logstream-incus -f"
