#!/usr/bin/env bash
set -euo pipefail

prefix="${BILOLA_PREFIX:-/usr/local/lib/bilola}"
unit_dir="${BILOLA_SYSTEMD_DIR:-/etc/systemd/system}"
arch="$(uname -m)"
case "$arch" in
  aarch64|arm64) binary="bilola-server-linux-arm64" ;;
  x86_64|amd64) binary="bilola-server-linux-amd64" ;;
  *) echo "unsupported architecture: $arch" >&2; exit 2 ;;
esac
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
install -d -m 0755 "$prefix"
install -m 0755 "$root_dir/build/$binary" "$prefix/bilola-server"
install -m 0644 "$root_dir/deploy/bilola-go-server.service" "$unit_dir/bilola-go-server.service"
systemctl daemon-reload
systemctl enable --now bilola-go-server.service
systemctl --no-pager --full status bilola-go-server.service || true
