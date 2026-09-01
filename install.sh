#!/usr/bin/env bash
set -Eeuo pipefail

VERSION="1.0.44"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_TREE="$SCRIPT_DIR/sources/bilola_go_port"
MENU_SOURCE="$SCRIPT_DIR/bhttp-menu"

PUBLIC_HOST="${PUBLIC_HOST:-}"
SSH_PORT="${SSH_PORT:-22}"
BHTTP_PORT="${BHTTP_PORT:-80}"
XHTTP_PORT="${XHTTP_PORT:-443}"
BTUN_BHTTP_PORT="${BTUN_BHTTP_PORT:-7080}"
BTUN_PORT="${BTUN_PORT:-7300}"
BTUN_XHTTP_PORT="${BTUN_XHTTP_PORT:-7443}"
CONFIGURE_FIREWALL=1
RUN_TESTS=1

# Servicios opcionales (1 = activar, 0 = no activar).
ENABLE_XHTTP="${ENABLE_XHTTP:-1}"
ENABLE_BTUN_BHTTP="${ENABLE_BTUN_BHTTP:-1}"
ENABLE_BTUN="${ENABLE_BTUN:-1}"
ENABLE_BTUN_XHTTP="${ENABLE_BTUN_XHTTP:-1}"
# BTUN nativo es requerido por BTUN sobre BHTTP y BTUN sobre XHTTP.
if (( ENABLE_BTUN_BHTTP == 1 || ENABLE_BTUN_XHTTP == 1 )); then
    ENABLE_BTUN=1
fi

usage() {
    cat <<'EOF'
Instalador BHTTP + SSH_XHTTP + BTUN pré-ZTUN 1.0.44

Uso:
  sudo bash install.sh [opções]

Opções:
  --ssh-port PORTA      Porta do OpenSSH local (padrão: 22)
  --no-firewall         Não altera regras do UFW
  --skip-tests          Não executa os testes locais de protocolo
  -h, --help            Mostra esta ajuda

Serviços podem ser ativados/desativados por variáveis de ambiente:
  ENABLE_XHTTP=1|0          SSH_XHTTP/BTUN sobre TLS (porta XHTTP_PORT)
  ENABLE_BTUN_BHTTP=1|0     BTUN sobre BHTTP (porta BTUN_BHTTP_PORT)
  ENABLE_BTUN=1|0           BTUN nativo TCP/UDP (porta BTUN_PORT)
  ENABLE_BTUN_XHTTP=1|0     BTUN sobre XHTTP dedicado (porta BTUN_XHTTP_PORT)

As portas também podem ser configuradas por variáveis de ambiente:
  BHTTP_PORT=80 XHTTP_PORT=443 BTUN_PORT=7300
  BTUN_BHTTP_PORT=7080 BTUN_XHTTP_PORT=7443
EOF
}

die() {
    echo "ERRO: $*" >&2
    exit 1
}

valid_port() {
    [[ "$1" =~ ^[0-9]+$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 ))
}

while (( $# > 0 )); do
    case "$1" in
        "") shift ;; # ignora argumento vazio (ex.: vindo de update.sh)
        --ssh-port)
            (( $# >= 2 )) || die "--ssh-port requer um valor"
            SSH_PORT="$2"
            shift 2
            ;;
        --no-firewall) CONFIGURE_FIREWALL=0; shift ;;
        --skip-tests) RUN_TESTS=0; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "opção desconhecida: $1" ;;
    esac
done

(( EUID == 0 )) || die "execute como root"
[[ -d /run/systemd/system ]] || die "systemd não está ativo"
[[ -d "$SOURCE_TREE" ]] || die "source ausente: $SOURCE_TREE"
[[ -f "$MENU_SOURCE" ]] || die "arquivo ausente: $MENU_SOURCE"

for port in "$SSH_PORT" "$BHTTP_PORT" "$XHTTP_PORT" "$BTUN_BHTTP_PORT" "$BTUN_PORT" "$BTUN_XHTTP_PORT"; do
    valid_port "$port" || die "porta inválida: $port"
done

if [[ -z "$PUBLIC_HOST" ]]; then
    PUBLIC_HOST="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '/src/ {for (i=1; i<=NF; i++) if ($i == "src") {print $(i+1); exit}}')"
fi
if [[ -z "$PUBLIC_HOST" ]]; then
    PUBLIC_HOST="$(hostname -f 2>/dev/null || hostname)"
fi
[[ "$PUBLIC_HOST" =~ ^[A-Za-z0-9._-]+$ ]] || die "host inválido: $PUBLIC_HOST"

case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "arquitetura não suportada: $(uname -m)" ;;
esac
BIN_DIR="$SCRIPT_DIR/bin/$ARCH"

if systemctl is-active --quiet ztun.service 2>/dev/null ||
   systemctl is-active --quiet ztun-server.service 2>/dev/null; then
    die "ZTUN está ativo. Este pacote é exclusivamente pré-ZTUN; desative-o antes."
fi

echo "[1/8] Validando os requisitos locais (sem internet)..."
for required_command in systemctl ip iptables sshd install cp awk grep ss ldd; do
    command -v "$required_command" >/dev/null 2>&1 ||
        die "comando obrigatório ausente: $required_command"
done
[[ -c /dev/net/tun ]] || die "dispositivo TUN ausente: /dev/net/tun"

echo "[2/8] Validando a source pré-ZTUN incluída..."
SOURCE_DIR="$SOURCE_TREE"
[[ -f "$SOURCE_DIR/go.mod" ]] || die "pacote-fonte inválido"
if find "$SOURCE_DIR" -iname '*ztun*' -print -quit | grep -q .; then
    die "o pacote contém arquivos ZTUN e foi recusado"
fi

echo "[3/8] Selecionando binários offline Linux/$ARCH..."
chmod +x "$BIN_DIR"/bilola-server "$BIN_DIR"/bilola-xhttp-server \
    "$BIN_DIR"/btun-server "$BIN_DIR"/bhttp-smoke \
    "$BIN_DIR"/xhttp-smoke "$BIN_DIR"/certgen 2>/dev/null || true
for binary in bilola-server bilola-xhttp-server btun-server \
    bhttp-smoke xhttp-smoke certgen; do
    [[ -x "$BIN_DIR/$binary" ]] || die "binário offline ausente: bin/$ARCH/$binary"
done
if ldd "$BIN_DIR/btun-server" 2>&1 | grep -q 'not found'; then
    ldd "$BIN_DIR/btun-server" >&2 || true
    die "o runtime PAM/glibc necessário ao BTUN não está instalado"
fi

echo "[4/8] Salvando configuração anterior..."
BACKUP_DIR="/root/bhttp-preztun-backup-$(date -u +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"
for item in \
    /etc/btun /etc/bilola \
    /usr/local/lib/btun /usr/local/lib/bilola \
    /usr/local/bin/bhttp \
    /etc/systemd/system/bilola-go-server.service \
    /etc/systemd/system/bilola-xhttp-server.service \
    /etc/systemd/system/btun-protocol.service \
    /etc/systemd/system/btun-routing.service \
    /etc/systemd/system/btun-bhttp.service \
    /etc/systemd/system/btun-xhttp.service; do
    if [[ -e "$item" || -L "$item" ]]; then
        cp -a --parents "$item" "$BACKUP_DIR"
    fi
done

# Desativa tudo para refletir as escolhas (reativamos só o necessário).
echo "[5/8] Parando serviços anteriores..."
for _u in bilola-go-server bilola-xhttp-server btun-protocol btun-routing btun-bhttp btun-xhttp; do
    systemctl disable --now "$_u" >/dev/null 2>&1 || true
done

echo "[6/8] Instalando binários, menu e configuração..."
install -d -m 0755 /usr/local/lib/bilola /usr/local/lib/btun /var/lib/btun
install -d -m 0700 /etc/bilola/tls
install -d -m 0755 /etc/btun
install -m 0755 "$BIN_DIR/bilola-server" /usr/local/lib/bilola/bilola-server
install -m 0755 "$BIN_DIR/bilola-xhttp-server" /usr/local/lib/bilola/bilola-xhttp-server
install -m 0755 "$BIN_DIR/bilola-server" /usr/local/lib/btun/bilola-server
install -m 0755 "$BIN_DIR/bilola-xhttp-server" /usr/local/lib/btun/bilola-xhttp-server
install -m 0755 "$BIN_DIR/btun-server" /usr/local/lib/btun/btun-server
install -m 0755 "$SOURCE_DIR/deploy/btun/btun-routing" /usr/local/lib/btun/btun-routing
install -m 0755 "$MENU_SOURCE" /usr/local/bin/bhttp

if (( ENABLE_XHTTP == 1 || ENABLE_BTUN_XHTTP == 1 )); then
    if [[ ! -s /etc/bilola/tls/fullchain.pem || ! -s /etc/bilola/tls/privkey.pem ]]; then
        "$BIN_DIR/certgen" -host "$PUBLIC_HOST" \
            -cert /etc/bilola/tls/fullchain.pem \
            -key /etc/bilola/tls/privkey.pem
    fi
    chmod 0600 /etc/bilola/tls/privkey.pem
    chmod 0644 /etc/bilola/tls/fullchain.pem
fi

install -m 0644 /dev/null /etc/pam.d/btun
cat >/etc/pam.d/btun <<'EOF'
@include common-auth
@include common-account
EOF

install -m 0600 /dev/null /etc/btun/server.env
cat >/etc/btun/server.env <<EOF
BTUN_TCP_LISTEN=0.0.0.0:$BTUN_PORT
BTUN_UDP_LISTEN=0.0.0.0:$BTUN_PORT
BTUN_TUN=btun0
BTUN_SUBNET=10.77.0.0/16
BTUN_GATEWAY=10.77.0.1/16
BTUN_AUTH=pam
BTUN_PAM_SERVICE=btun
BTUN_HANDSHAKE_TIMEOUT=15s
BTUN_IDLE_TIMEOUT=2m
BTUN_MAX_COVER_BYTES=65536
BTUN_STATS_FILE=/var/lib/btun/stats.json
EOF

echo "[7/8] Criando serviços systemd..."

cat >/etc/systemd/system/bilola-go-server.service <<EOF
[Unit]
Description=Bilola BHTTP Go server
After=network-online.target ssh.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/lib/bilola/bilola-server --listen 0.0.0.0:$BHTTP_PORT --target 127.0.0.1:$SSH_PORT
Restart=on-failure
RestartSec=3
User=root
LimitNOFILE=65536
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

if (( ENABLE_XHTTP == 1 )); then
cat >/etc/systemd/system/bilola-xhttp-server.service <<EOF
[Unit]
Description=Bilola SSH_XHTTP and BTUN shared TLS server
After=network-online.target ssh.service btun-protocol.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/lib/bilola/bilola-xhttp-server --listen=0.0.0.0:$XHTTP_PORT --target=127.0.0.1:$SSH_PORT --auto-hosts=$PUBLIC_HOST --auto-delay=1s --btun-target=127.0.0.1:$BTUN_PORT --tls-cert=/etc/bilola/tls/fullchain.pem --tls-key=/etc/bilola/tls/privkey.pem
Restart=on-failure
RestartSec=3
User=root
LimitNOFILE=65536
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
fi

if (( ENABLE_BTUN == 1 )); then
cat >/etc/systemd/system/btun-protocol.service <<'EOF'
[Unit]
Description=BTUN native TCP and UDP protocol server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/btun/server.env
ExecStart=/usr/local/lib/btun/btun-server
Restart=on-failure
RestartSec=3
User=root
LimitNOFILE=65536
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

cat >/etc/systemd/system/btun-routing.service <<'EOF'
[Unit]
Description=BTUN forwarding and NAT rules
After=network-online.target btun-protocol.service
Requires=btun-protocol.service

[Service]
Type=oneshot
RemainAfterExit=true
EnvironmentFile=-/etc/btun/server.env
ExecStart=/usr/local/lib/btun/btun-routing start
ExecStop=/usr/local/lib/btun/btun-routing stop

[Install]
WantedBy=multi-user.target
EOF
fi

if (( ENABLE_BTUN_BHTTP == 1 )); then
cat >/etc/systemd/system/btun-bhttp.service <<EOF
[Unit]
Description=BTUN over BHTTP
After=network-online.target btun-protocol.service
Requires=btun-protocol.service

[Service]
Type=simple
ExecStart=/usr/local/lib/btun/bilola-server --listen 0.0.0.0:$BTUN_BHTTP_PORT --target 127.0.0.1:$BTUN_PORT
Restart=on-failure
RestartSec=3
User=root
LimitNOFILE=65536
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
fi

if (( ENABLE_BTUN_XHTTP == 1 )); then
cat >/etc/systemd/system/btun-xhttp.service <<EOF
[Unit]
Description=BTUN over dedicated XHTTP TLS
After=network-online.target btun-protocol.service
Requires=btun-protocol.service

[Service]
Type=simple
ExecStart=/usr/local/lib/btun/bilola-xhttp-server --listen=0.0.0.0:$BTUN_XHTTP_PORT --target=127.0.0.1:$BTUN_PORT --tls-cert=/etc/bilola/tls/fullchain.pem --tls-key=/etc/bilola/tls/privkey.pem
Restart=on-failure
RestartSec=3
User=root
LimitNOFILE=65536
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
fi

echo "[8/8] Ativando serviços e firewall..."
SYSTEMD_UNITS=(bilola-go-server)
(( ENABLE_XHTTP == 1 ))      && SYSTEMD_UNITS+=(bilola-xhttp-server)
(( ENABLE_BTUN == 1 ))       && SYSTEMD_UNITS+=(btun-protocol btun-routing)
(( ENABLE_BTUN_BHTTP == 1 )) && SYSTEMD_UNITS+=(btun-bhttp)
(( ENABLE_BTUN_XHTTP == 1 )) && SYSTEMD_UNITS+=(btun-xhttp)

systemctl daemon-reload
systemctl enable "${SYSTEMD_UNITS[@]}" >/dev/null

if (( ENABLE_BTUN == 1 )); then
    systemctl start btun-protocol
    systemctl start btun-routing
fi
(( ENABLE_BTUN_BHTTP == 1 )) && systemctl start btun-bhttp
(( ENABLE_BTUN_XHTTP == 1 )) && systemctl start btun-xhttp
systemctl start bilola-go-server
(( ENABLE_XHTTP == 1 )) && systemctl start bilola-xhttp-server

if (( CONFIGURE_FIREWALL == 1 )) && command -v ufw >/dev/null 2>&1 &&
   ufw status | grep -q '^Status: active'; then
    ufw allow "$SSH_PORT/tcp" >/dev/null
    ufw allow "$BHTTP_PORT/tcp" >/dev/null
    (( ENABLE_XHTTP == 1 ))      && ufw allow "$XHTTP_PORT/tcp" >/dev/null
    (( ENABLE_BTUN_BHTTP == 1 )) && ufw allow "$BTUN_BHTTP_PORT/tcp" >/dev/null
    if (( ENABLE_BTUN == 1 )); then
        ufw allow "$BTUN_PORT/tcp" >/dev/null
        ufw allow "$BTUN_PORT/udp" >/dev/null
    fi
    (( ENABLE_BTUN_XHTTP == 1 )) && ufw allow "$BTUN_XHTTP_PORT/tcp" >/dev/null
fi

for service in "${SYSTEMD_UNITS[@]}"; do
    if ! systemctl is-active --quiet "$service"; then
        systemctl status "$service" --no-pager || true
        die "serviço não iniciou: $service"
    fi
done

if (( RUN_TESTS == 1 )); then
    "$BIN_DIR/bhttp-smoke" -host 127.0.0.1 -port "$BHTTP_PORT"
    if (( ENABLE_BTUN_BHTTP == 1 )); then
        "$BIN_DIR/bhttp-smoke" -host 127.0.0.1 -port "$BTUN_BHTTP_PORT"
    fi
    if (( ENABLE_XHTTP == 1 )); then
        "$BIN_DIR/xhttp-smoke" \
            -host 127.0.0.1 -port "$XHTTP_PORT" \
            -sni "$PUBLIC_HOST" -host-header "$PUBLIC_HOST" -insecure -target-mode btun
    fi
fi

cat <<EOF

Instalação pré-ZTUN $VERSION concluída.

Host:             $PUBLIC_HOST
BHTTP:            $BHTTP_PORT/tcp
EOF
if (( ENABLE_XHTTP == 1 )); then
cat <<EOF
SSH_XHTTP/BTUN:   $XHTTP_PORT/tcp
EOF
fi
if (( ENABLE_BTUN_BHTTP == 1 )); then
cat <<EOF
BTUN BHTTP:       $BTUN_BHTTP_PORT/tcp
EOF
fi
if (( ENABLE_BTUN == 1 )); then
cat <<EOF
BTUN nativo:      $BTUN_PORT/tcp e $BTUN_PORT/udp
EOF
fi
if (( ENABLE_BTUN_XHTTP == 1 )); then
cat <<EOF
BTUN XHTTP:       $BTUN_XHTTP_PORT/tcp
EOF
fi
cat <<EOF
OpenSSH local:    $SSH_PORT/tcp
Backup anterior:  $BACKUP_DIR

Abra o gerenciador com:
  bhttp

Veja o status com:
  bhttp status
EOF
