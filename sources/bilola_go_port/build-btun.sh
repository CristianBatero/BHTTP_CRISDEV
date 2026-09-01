#!/usr/bin/env bash
set -euo pipefail

source_dir="$(cd "$(dirname "$0")" && pwd)"
target_arch="${GOARCH:-$(go env GOARCH)}"
target_os="${GOOS:-linux}"
output="$source_dir/build/btun-server-${target_os}-${target_arch}"

if [[ "$target_os" != "linux" ]]; then
    echo "btun-server requires Linux TUN and PAM" >&2
    exit 2
fi
if [[ "${CGO_ENABLED:-$(go env CGO_ENABLED)}" != "1" ]]; then
    echo "btun-server requires CGO_ENABLED=1 for PAM" >&2
    exit 2
fi

mkdir -p "$source_dir/build"
CGO_ENABLED=1 GOOS="$target_os" GOARCH="$target_arch" \
    go build -trimpath -ldflags "-s -w -X main.version=1.0.0" \
    -o "$output" ./cmd/btun-server
echo "built $output"
