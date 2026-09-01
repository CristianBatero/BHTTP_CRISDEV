# DT5 XHTTP handshake validation — 2026-08-23

## Symptom

DTunnel 5 reached an OpenSSH target through XHTTP and rejected its banner:

```text
expected "DTUNNEL/1.1 SERVER_HELLO\r\n", got "SSH-2.0-OpenSSH_8.2p1 Ubun"
```

## Cause and correction

The shared TLS/XHTTP listener on port 443 used OpenSSH (`127.0.0.1:22`) as
its only stream target. It now selects the stream target from the normalized
HTTP Host:

- BTUN-only Hosts (`--btun-hosts`) -> BTUN protocol core at
  `127.0.0.1:7300`
- shared Hosts (`--auto-hosts`) -> BTUN/7300 after a
  `DTUNNEL/1.1 CLIENT_HELLO`, otherwise OpenSSH/22 after the configured delay
- every other Host -> legacy OpenSSH at `127.0.0.1:22`

Both configured lists accept `#` and comma separators and can be extended
without rebuilding the executable. `f91gs78otu7k.azion.app` is deployed as a
shared Host so existing DT5 SSH_XHTTP profiles and BTUN XHTTP can use the same
Azion domain.

The legacy BHTTP and SSH_XHTTP framing and targets were not changed.

## End-to-end result

Tests were executed against the private listener address to avoid the host's
intentional loopback redirect from `127.0.0.1:443` to its authorization
service.

```text
BTUN XHTTP OK: TLS/H2 10.0.0.162:443 SNI=flux.dtmod.shop Host=flux.dtmod.shop, target=127.0.0.1:7300
SSH_XHTTP OK: TLS/H2 10.0.0.162:443 SNI=flux.dtmod.shop Host=legacy-ssh.dtmod.shop, target=SSH-2.0-OpenSSH_8.2p1 Ubuntu-4ubuntu0.13
```

The shared Azion Host was also tested in both modes:

```text
BTUN XHTTP OK: TLS/H2 10.0.0.162:443 SNI=f91gs78otu7k.azion.app Host=f91gs78otu7k.azion.app, target=127.0.0.1:7300
SSH_XHTTP OK: TLS/H2 10.0.0.162:443 SNI=f91gs78otu7k.azion.app Host=f91gs78otu7k.azion.app, target=SSH-2.0-OpenSSH_8.2p1 Ubuntu-4ubuntu0.13
```

The service journal independently recorded the selected destinations:

```text
XHTTP session baab91d0 opened -> 127.0.0.1:7300
XHTTP session 65ccfa69 opened -> 127.0.0.1:22
```

All Go tests passed with `go test ./...`.

## Required DT5 XHTTP fields

- protocol: XHTTP
- proxy port: 443 when using the public Azion route
- HTTP Host: `f91gs78otu7k.azion.app` (or another Host configured in
  `--btun-hosts`)
- server/core port: 7300
- SNI: the TLS/edge SNI required by the selected proxy profile

HTTP Host and SNI are independent. A legacy SSH_XHTTP Host intentionally
continues to select OpenSSH and therefore cannot be used as the BTUN Host.
