#!/usr/bin/env bash
#
# setup.sh — Instalador único e INTERACTIVO de BHTTP_CRISDEV (motor BTUN pré-ZTUN).
#
# A diferencia del instalador clásico (que descargaba un ZIP desde Dropbox),
# este usa GIT como fuente y puente de actualizaciones: clona (o actualiza)
# el repositorio y ejecuta el install.sh del paquete.
#
# Uso en la VPS (como root):
#   wget https://raw.githubusercontent.com/CristianBatero/BHTTP_CRISDEV/main/setup.sh \
#       && chmod +x setup.sh && ./setup.sh
#
# O si ya tienes el repo clonado:
#   bash /opt/bhttp-crisdev/setup.sh
#
set -Eeuo pipefail

# ---- Configuración (edita si el repo/rama cambia) ----
REPO_URL="${BTUN_REPO_URL:-https://github.com/CristianBatero/BHTTP_CRISDEV.git}"
BRANCH="${BTUN_BRANCH:-main}"
INSTALL_DIR="${BTUN_INSTALL_DIR:-/opt/bhttp-crisdev}"
SCRIPT_NAME="$(basename "${BASH_SOURCE[0]}")"

C='\033[0m'; CY='\033[0;36m'; YE='\033[0;33m'; RE='\033[0;31m'; GR='\033[0;32m'
log()  { printf "${CY}[setup]${C} %s\n" "$*"; }
warn() { printf "${YE}[setup]${C} %s\n" "$*" >&2; }
die()  { printf "${RE}[setup ERROR]${C} %s\n" "$*" >&2; exit 1; }

(( EUID == 0 )) || die "Ejecuta como root:  sudo bash setup.sh  (o directo como root)"

# Auto-instalar dependencias básicas si faltan en la VPS
install_deps() {
    local missing=()
    for cmd in git sha256sum systemctl install; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            missing+=("$cmd")
        fi
    done

    if (( ${#missing[@]} > 0 )); then
        log "Instalando dependencias del sistema (${missing[*]})..."
        if command -v apt-get >/dev/null 2>&1; then
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -y && apt-get install -y git coreutils systemd iptables iproute2
        elif command -v dnf >/dev/null 2>&1; then
            dnf install -y git coreutils systemd iptables iproute
        elif command -v yum >/dev/null 2>&1; then
            yum install -y git coreutils systemd iptables iproute
        elif command -v apk >/dev/null 2>&1; then
            apk add --no-cache git coreutils iptables iproute2
        fi
    fi
}
install_deps

for cmd in git sha256sum systemctl install; do
    command -v "$cmd" >/dev/null 2>&1 || die "Comando obligatorio ausente: $cmd"
done

IS_IN_REPO=0
SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "$SCRIPT_PATH/install.sh" && -d "$SCRIPT_PATH/sources/bilola_go_port" ]]; then
    IS_IN_REPO=1
    INSTALL_DIR="$SCRIPT_PATH"
fi

# ---- 1. Obtener el paquete (git clone o git pull) ----
if (( IS_IN_REPO == 0 )); then
    mkdir -p "$INSTALL_DIR"
    if [[ -d "$INSTALL_DIR/.git" ]]; then
        log "Actualizando paquete existente en $INSTALL_DIR (git pull)..."
        git -C "$INSTALL_DIR" fetch --prune origin
        git -C "$INSTALL_DIR" checkout --force "$BRANCH"
        git -C "$INSTALL_DIR" pull --ff-only origin "$BRANCH" \
            || die "No se pudo actualizar el repo (hay cambios locales?): $INSTALL_DIR"
    elif [[ -d "$INSTALL_DIR" ]] && [[ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]]; then
        die "El directorio $INSTALL_DIR existe y no es un repo git. Muévelo o bórralo y reintenta."
    else
        log "Clonando paquete desde $REPO_URL ..."
        git clone --depth 1 --branch "$BRANCH" "$REPO_URL" "$INSTALL_DIR" \
            || die "Fallo al clonar el repositorio: $REPO_URL"
    fi
else
    log "Ejecutado desde el repositorio: $INSTALL_DIR"
fi

cd "$INSTALL_DIR"

# ---- 2. Verificar integridad (SHA256SUMS) ----
if [[ -f SHA256SUMS ]]; then
    if sha256sum -c SHA256SUMS --quiet >/dev/null 2>&1; then
        log "Integridad verificada (SHA256SUMS OK)."
    else
        warn "SHA256SUMS no coincide. Continúo de todos modos (revisa el repo)."
    fi
else
    warn "SHA256SUMS no encontrado; se omite la verificación de integridad."
fi

# ---- 3. Ejecutar el instalador del paquete (no interactivo si vienen variables) ----
# Cargar puertos por defecto (evitan chocar con SCRIP_BASICA: 22/80/90/110/443/7300)
if [[ -f "$INSTALL_DIR/ports.env" ]]; then
    set -a
    # shellcheck disable=SC1091
    source "$INSTALL_DIR/ports.env"
    set +a
fi
log "Ejecutando install.sh del paquete..."
bash "$INSTALL_DIR/install.sh" "$@"

cat <<EOF

${GR}============================================================${C}
${GR}  BHTTP_CRISDEV instalado / actualizado correctamente${C}
${GR}============================================================${C}
Paquete (git):    $INSTALL_DIR  (rama $BRANCH)

Para actualizar a una versión nueva del motor/servidor:
  bash $INSTALL_DIR/update.sh
  (o:  bash $INSTALL_DIR/setup.sh)

Menú de gestión:  bhttp
Estado:           bhttp status
EOF
