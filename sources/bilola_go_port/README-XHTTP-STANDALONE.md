# Bilola SSH_XHTTP standalone server

This package contains only the Go SSH_XHTTP v2 server replicated from the
DTunnel 5.0.0 transport. It listens with TLS + HTTP/2 on TCP 443 and 8080 and relays
each XHTTP session to the local OpenSSH daemon on `127.0.0.1:22`.

## Profile for the current VPS

- Server/Host/TLS Server Name: `flux.dtmod.shop`
- Ports: `443` and `8080`
- Transport: `SSH_XHTTP`
- TLS: enabled

The DTunnel-facing connection must use HTTP/2. A CDN such as Azion may use
HTTP/1.1 for its separate connection to the VPS origin; the server accepts
that origin hop while preserving the XHTTP v2 framing, ACK and resume logic.

## Install on another VPS

The VPS needs OpenSSH listening locally and a valid certificate whose name is
the same name configured in the app. Extract the ZIP and run as root:

```sh
BILOLA_TLS_CERT=/path/to/fullchain.pem \
BILOLA_TLS_KEY=/path/to/privkey.pem \
./deploy/install-xhttp.sh
```

The installer selects the included ARM64 or AMD64 binary, copies the
certificate and key to `/etc/bilola/tls`, installs
`bilola-xhttp-server.service`, and enables it immediately on TCP 443 and 8080.
Allow inbound TCP/443 and TCP/8080 in the VPS firewall/security group. The CDN
origin may use either listener. The app normally connects to the CDN's public
TLS port 443; port 8080 can be used for a direct connection or CDN origin hop.

Useful commands:

```sh
systemctl status bilola-xhttp-server
journalctl -u bilola-xhttp-server -f
systemctl restart bilola-xhttp-server
```

To verify ALPN externally:

```sh
openssl s_client -connect SERVER_IP:8080 -servername vpn.example.com \
  -alpn h2 </dev/null 2>&1 | grep 'ALPN protocol'
```

The expected result is `ALPN protocol: h2`.
