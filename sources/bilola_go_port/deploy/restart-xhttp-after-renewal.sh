#!/usr/bin/env bash
set -euo pipefail

# Certbot exposes the renewed lineage to deployment hooks. Ignore unrelated
# certificates on multi-domain VPS installations.
if [[ "${RENEWED_LINEAGE:-}" == */flux.dtmod.shop ]]; then
    systemctl try-restart bilola-xhttp-server.service
fi
