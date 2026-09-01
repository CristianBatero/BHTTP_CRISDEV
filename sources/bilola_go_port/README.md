# Bilola BHTTP + SSH_XHTTP Go port

This tree contains Go ports of the BHP1/BHTTP and SSH_XHTTP transports on both
the server and Android GoJNI sides. BHTTP keeps the restored 1.0.11 wire
profile. SSH_XHTTP reproduces DTunnel 5.0.0's HTTP/2 v2 stream: POST modes,
32-hex session IDs, four upload workers, ordered upload sequences, SSE prefix,
12-byte framed downloads, cumulative ACK and reconnect/resume.

## BHTTP v2 lanes

The BHTTP listener keeps the original BHP1 wire behavior and additionally
supports an explicit BHP2 capability probe. BHTTP v2 clients attach persistent
upload/download lanes to the same SID, require positive resume confirmation on
replacement lanes, and receive cumulative upload ACKs for frames actually
committed in sequence. Old clients remain on the unchanged modes 0..4.

`bilola-server --bhttp-v2-max-lanes 128` controls the combined per-session lane
ceiling. A client that explicitly selects BHTTP v2 fails clearly when capability
negotiation is unavailable; it does not silently fall back to BHTTP v1.

The BHTTP server listens on TCP 80 and 53 by default. SSH_XHTTP is an optional,
independent TLS/HTTP2 listener, configured on TCP 443 and 8080. Both relay sessions to
`127.0.0.1:22` and may run from the same binary.

## Server

```sh
go test -race ./...
./build/bilola-server-linux-arm64 --listen 0.0.0.0:80,0.0.0.0:53
./build/bilola-server-linux-arm64 --listen= \
  --xhttp-listen=0.0.0.0:443 \
  --tls-cert=/path/to/fullchain.pem --tls-key=/path/to/privkey.pem
```

`deploy/install.sh` installs BHTTP. `deploy/install-xhttp.sh` installs the
standalone XHTTP-only binary on ports 443 and 8080 and requires a certificate/key through
`BILOLA_TLS_CERT` and `BILOLA_TLS_KEY`. Each service can be stopped or rolled
back without taking down the other.

Production smoke test:

```sh
go run ./cmd/xhttp-smoke --host SERVER_IP --port 8080 --sni vpn.example.com
```

## Android JNI

`android/build_jni.sh` produces `libgojni.so` for `arm64-v8a` and
`armeabi-v7a`. The Android project `/root/dtunnel_android_vpn` invokes it from
`build.sh` and packages the Go bridges in
`Bilola-Tunnel-v1.0.26-XHTTP-GoJNI.apk`. Java still owns VpnService, SSH/PAM,
SOCKS5, DNS, and tun2socks lifecycle; the selectable HTTP transport engines
run in GoJNI.

The Android SSH_XHTTP profile separates the DTunnel 5 fields: Server/Port are
the SSH destination and HTTP Host, Proxy Host/Port are the TLS/H2 endpoint,
and TLS SNI controls certificate negotiation. Multiple proxy hosts may be
listed with `#`; the client rotates to the next host after a failure. As in the
DTunnel 5 XHTTP implementation, SNI is sent but edge certificate hostname
verification is permissive. Before SSH starts, every proxy is probed
sequentially and live progress is reported as `Proxies testados: N/total`;
only endpoints that answer as XHTTP over HTTP/2 remain in the runtime pool.
For direct access, all host fields may use the
origin name and Proxy Port `443` or `8080`. For CDN access, Proxy Host is the edge address
and Proxy Port is normally `443`; configure the CDN-to-origin port as `443` or `8080`,
while Server/Host and SNI remain independently configurable.

## Standalone BTUN protocol server

`cmd/btun-server` is the open, in-tree server for the separate BTUN mode. It
implements the native TCP/UDP wire framing, PAM or file authentication, IPv4
address assignment and TUN packet routing itself; it neither embeds nor calls
`proto-server`. The isolated deployment adds BHTTP on `7080` and XHTTP on
`7443` without changing the existing transport services. See
`deploy/btun/README.md`.
