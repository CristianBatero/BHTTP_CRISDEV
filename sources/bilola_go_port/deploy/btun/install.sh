#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
    echo "run as root" >&2
    exit 1
fi

source_dir="$(cd "$(dirname "$0")/../.." && pwd)"
install_dir="/usr/local/lib/btun"
config_dir="/etc/btun"
machine_arch="$(uname -m)"

case "$machine_arch" in
    aarch64|arm64) binary_arch="arm64" ;;
    x86_64|amd64) binary_arch="amd64" ;;
    *) echo "unsupported architecture: $machine_arch" >&2; exit 2 ;;
esac

test -x "$source_dir/build/btun-server-linux-$binary_arch"
test -x "$source_dir/build/bilola-server-linux-$binary_arch"
test -x "$source_dir/build/bilola-xhttp-server-linux-$binary_arch"

install -d -m 0755 "$install_dir" "$config_dir" /var/lib/btun
install -m 0755 "$source_dir/build/btun-server-linux-$binary_arch" \
    "$install_dir/btun-server"
install -m 0755 "$source_dir/build/bilola-server-linux-$binary_arch" \
    "$install_dir/bilola-server"
install -m 0755 "$source_dir/build/bilola-xhttp-server-linux-$binary_arch" \
    "$install_dir/bilola-xhttp-server"
install -m 0755 "$source_dir/deploy/btun/btun-routing" \
    "$install_dir/btun-routing"

if [[ ! -e "$config_dir/server.env" ]]; then
    install -m 0600 /dev/null "$config_dir/server.env"
    tee "$config_dir/server.env" >/dev/null <<'EOF'
BTUN_TCP_LISTEN=0.0.0.0:7300
BTUN_UDP_LISTEN=0.0.0.0:7300
BTUN_TUN=btun0
BTUN_SUBNET=10.77.0.0/16
BTUN_GATEWAY=10.77.0.1/16
BTUN_AUTH=pam
BTUN_PAM_SERVICE=login
BTUN_HANDSHAKE_TIMEOUT=15s
BTUN_IDLE_TIMEOUT=2m
BTUN_MAX_COVER_BYTES=65536
BTUN_STATS_FILE=/var/lib/btun/stats.json
# BTUN_WAN_INTERFACE=eth0
EOF
fi

for unit in btun-protocol btun-bhttp btun-xhttp btun-routing; do
    install -m 0644 "$source_dir/deploy/btun/${unit}.service" \
        "/etc/systemd/system/${unit}.service"
done

systemctl daemon-reload
echo "BTUN own server staged. Review /etc/btun/server.env, then enable the services."
