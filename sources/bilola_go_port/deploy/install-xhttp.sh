#!/usr/bin/env bash
set -euo pipefail

prefix="${BILOLA_PREFIX:-/usr/local/lib/bilola}"
unit_dir="${BILOLA_SYSTEMD_DIR:-/etc/systemd/system}"
config_dir="${BILOLA_CONFIG_DIR:-/etc/bilola/tls}"
certificate="${BILOLA_TLS_CERT:-}"
private_key="${BILOLA_TLS_KEY:-}"

if [[ -z "$certificate" || -z "$private_key" ]]; then
    echo "Set BILOLA_TLS_CERT and BILOLA_TLS_KEY to a valid certificate and key." >&2
    exit 2
fi
test -f "$certificate"
test -f "$private_key"

arch="$(uname -m)"
case "$arch" in
  aarch64|arm64) binary="bilola-xhttp-server-linux-arm64" ;;
  x86_64|amd64) binary="bilola-xhttp-server-linux-amd64" ;;
  *) echo "unsupported architecture: $arch" >&2; exit 2 ;;
esac
root_dir="$(cd "$(dirname "$0")/.." && pwd)"

install -d -m 0755 "$prefix"
install -d -m 0700 "$config_dir"
install -m 0755 "$root_dir/build/$binary" "$prefix/bilola-xhttp-server"
install -m 0644 "$certificate" "$config_dir/fullchain.pem"
install -m 0600 "$private_key" "$config_dir/privkey.pem"
install -m 0644 "$root_dir/deploy/bilola-xhttp-server.service" \
    "$unit_dir/bilola-xhttp-server.service"
systemctl daemon-reload
systemctl enable --now bilola-xhttp-server.service
systemctl --no-pager --full status bilola-xhttp-server.service || true
