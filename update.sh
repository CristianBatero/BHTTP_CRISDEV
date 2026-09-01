#!/usr/bin/env bash
#
# update.sh — Actualizador automático de BHTTP_CRISDEV (motor BTUN).
#
# Usa GIT como puente: hace pull del repo y, si hay cambios, verifica la
# integridad y re-ejecuta install.sh (que hace backup, reinstala binarios y
# reinicia los servicios systemd sin tocar tus credenciales manuales).
#
# Uso (como root):
#   bash /opt/bhttp-crisdev/update.sh
#
set -Eeuo pipefail

REPO_URL="${BTUN_REPO_URL:-https://github.com/CristianBatero/BHTTP_CRISDEV.git}"
BRANCH="${BTUN_BRANCH:-main}"
INSTALL_DIR="${BTUN_INSTALL_DIR:-/opt/bhttp-crisdev}"

C='\033[0m'; CY='\033[0;36m'; YE='\033[0;33m'; RE='\033[0;31m'; GR='\033[0;32m'
log()  { printf "${CY}[update]${C} %s\n" "$*"; }
warn() { printf "${YE}[update]${C} %s\n" "$*" >&2; }
die()  { printf "${RE}[update ERROR]${C} %s\n" "$*" >&2; exit 1; }

(( EUID == 0 )) || die "Ejecuta como root:  sudo bash update.sh"
command -v git >/dev/null 2>&1 || die "git está ausente. Instálalo primero."
[[ -d "$INSTALL_DIR/.git" ]] || die "No hay repo git en $INSTALL_DIR. Ejecuta antes setup.sh (o clona el repo)."

cd "$INSTALL_DIR"

log "Verificando actualizaciones en $REPO_URL (rama $BRANCH)..."
git fetch --prune origin

LOCAL_HEAD="$(git rev-parse HEAD 2>/dev/null || true)"
REMOTE_HEAD="$(git rev-parse "origin/$BRANCH" 2>/dev/null || true)"

if [[ -z "$REMOTE_HEAD" ]]; then
    die "No se encontró la rama '$BRANCH' remota. Revisa REPO_URL/BRANCH."
fi

if [[ "$LOCAL_HEAD" == "$REMOTE_HEAD" ]]; then
    log "Ya estás en la última versión ($LOCAL_HEAD corto: ${LOCAL_HEAD:0:8})."
    log "No hay nada que actualizar."
    exit 0
fi

log "Nueva versión disponible: ${REMOTE_HEAD:0:8} (actual: ${LOCAL_HEAD:0:8})"
git pull --ff-only origin "$BRANCH" || die "git pull falló (¿cambios locales?)."

# ---- Verificar integridad tras el pull ----
if [[ -f SHA256SUMS ]]; then
    if sha256sum -c SHA256SUMS --quiet >/dev/null 2>&1; then
        log "Integridad verificada (SHA256SUMS OK)."
    else
        die "SHA256SUMS no coincide tras la actualización. Abortando (no se reinstaló)."
    fi
else
    warn "SHA256SUMS ausente; se omite la verificación."
fi

# ---- Reinstalar (idempotente: hace backup y reinicia servicios) ----
log "Reinstalando binarios y servicios..."
bash "$INSTALL_DIR/install.sh" "${@:-}"

log "Actualización completada a ${REMOTE_HEAD:0:8}."
