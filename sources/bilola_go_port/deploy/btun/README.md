# Isolated BTUN server stack

This deployment runs the BTUN protocol server implemented in this source
tree. It does not install, invoke or require `proto-server`, a license token,
or any closed server component. Existing BHTTP and SSH_XHTTP production
services and their ports are not modified.

Listeners:

- BTUN TCP: `7300/tcp`
- BTUN UDP: `7300/udp`
- BTUN over BHTTP: `7080/tcp` -> `127.0.0.1:7300`
- BTUN over XHTTP: `7443/tcp` -> `127.0.0.1:7300`

The shared public TLS listener on port `443` can also serve both transports
without changing the legacy SSH_XHTTP protocol. Its HTTP Host selector routes
the configured BTUN-only Hosts to the core on `127.0.0.1:7300`; every other
Host keeps the original OpenSSH target on `127.0.0.1:22`. Multiple Hosts are
accepted in `--btun-hosts`, separated by `#` or comma. Matching is
case-insensitive and ignores an optional port suffix. Adding or removing a
Host only requires editing the service arguments and restarting the service;
the binary does not need to be rebuilt.

A Host that must support both BTUN and the old SSH_XHTTP mode belongs in
`--auto-hosts`, not `--btun-hosts`. For shared Hosts the first tunneled
`DTUNNEL/1.1 CLIENT_HELLO` selects BTUN/7300. An SSH client that waits for the
server banner automatically falls back to OpenSSH/22 after `--auto-delay`
(one second in the deployed unit). Shared Hosts also accept `#` or comma
separators.

The core creates `btun0`, authenticates users, assigns addresses from
`10.77.0.0/16`, and exchanges IPv4 packets with that interface. The routing
unit configures `10.77.0.1/16`, forwarding and NAT. Its WAN interface is
detected from the default IPv4 route; set `BTUN_WAN_INTERFACE` explicitly if
the host has more than one uplink.

Authentication defaults to the host's PAM `login` service. Alternatively,
set `BTUN_AUTH=file` and create `/etc/btun/users` (mode `0600`) with one
`username:password` entry per line. `BTUN_AUTH=allow` exists only for isolated
testing and disables authentication.

Build and install on the target host:

```sh
./build-btun.sh
sudo ./deploy/btun/install.sh
sudoedit /etc/btun/server.env
sudo systemctl enable --now btun-protocol btun-routing btun-bhttp btun-xhttp
```

XHTTP expects the existing certificate paths
`/etc/bilola/tls/fullchain.pem` and `/etc/bilola/tls/privkey.pem`. Change the
dedicated unit if the certificate is elsewhere.

Suggested app profiles:

- TCP: server port `7300`, protocol `TCP`.
- UDP: server port `7300`, protocol `UDP`.
- BHTTP: server port `7080`, protocol `BHTTP`.
- XHTTP: server port `7300`, protocol `XHTTP`, proxy port `7443`, and an SNI
  covered by the configured certificate.
- XHTTP through the Azion/public `443` endpoint: server port `7300`, protocol
  `XHTTP`, proxy port `443`, and an HTTP Host present in `--btun-hosts` or
  `--auto-hosts`. The deployed unit treats `flux.dtmod.shop` as BTUN-only and
  `f91gs78otu7k.azion.app` as shared BTUN/SSH_XHTTP. SNI is a separate TLS
  field and may be the edge SNI required by the profile.
Useful checks:

```sh
systemctl status btun-protocol btun-routing btun-bhttp btun-xhttp
journalctl -u btun-protocol -f
cat /var/lib/btun/stats.json
```

End-to-end routing checks from the server itself (replace the private address
if needed):

```sh
go run ./cmd/xhttp-smoke -host 10.0.0.162 -port 443 \
  -sni flux.dtmod.shop -host-header flux.dtmod.shop -insecure \
  -target-mode btun
go run ./cmd/xhttp-smoke -host 10.0.0.162 -port 443 \
  -sni flux.dtmod.shop -host-header legacy-ssh.example -insecure \
  -target-mode ssh
```
