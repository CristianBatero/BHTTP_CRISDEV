# BTUN pré-ZTUN 1.0.44 — Instalador AIO (offline o vía GIT)

Este paquete instala localmente BHTTP, SSH_XHTTP y BTUN, sin ZTUN y sin descargar
archivos de internet (los binarios Linux AMD64 y ARM64, las pruebas, el generador de
certificado, el menú `bhttp` y el código fuente completo utilizado en la compilación
ya están incluidos).

También es el repositorio oficial de distribución/actualización vía **GIT**:
el `setup.sh` clona (o actualiza) el paquete y el `update.sh` hace `git pull` +
reinstala, sirviendo como puente para nuevas versiones del motor/servidor.

## Instalación vía GIT (recomendado) — una sola línea

```bash
curl -fsSL "https://raw.githubusercontent.com/CristianBatero/BHTTP_CRISDEV/main/setup.sh?v=$(date +%s)" -o setup.sh && bash setup.sh
```

El `setup.sh` descarga/actualiza el paquete en `/opt/bhttp-crisdev` (vía `git clone`
o `git pull`), verifica el `SHA256SUMS`, corrige los permisos y ejecuta el
`install.sh` con detección automática de IP/arquitectura.

Durante la instalación el script te preguntará:
- **Puerto BHTTP** — presiona `[ENTER]` para usar el valor por defecto (`7080`)
- **TLS/XHTTP** — presiona `[ENTER]` para activarlo, `n` para desactivarlo

Modo sin preguntas (no interactivo):

```bash
BHTTP_PORT=7080 ENABLE_XHTTP=1 sudo bash setup.sh
```

## Actualización (update.sh)

Cuando haya una nueva versión del motor/servidor publicada en el repositorio:

```bash
sudo bash /opt/bhttp-crisdev/update.sh
```

El `update.sh` hace `git pull --ff-only`, valida el `SHA256SUMS` y re-instala
(hace backup de la configuración actual y reinicia los servicios sin borrar tus
credenciales manuales).

> **Nota:** para cambiar MANUALMENTE la rama instalada (ej.: de `main` a otra),
> ejecuta `sudo bash /opt/bhttp-crisdev/setup.sh`.

## Instalación offline (archivo ZIP)

Envía el ZIP a la VPS, entra como `root` y ejecuta:

```bash
mkdir -p /root/btun-offline
cd /root/btun-offline
unzip /root/BTUN-pre-ZTUN-OFFLINE-AIO-v1.0.44.zip
bash install.sh
```

No uses `--host`: el instalador detecta automáticamente la dirección de la propia
máquina. Para cambiar solo el puerto SSH local, usa:

```bash
bash install.sh --ssh-port 22
```

Puertos por defecto:

| Puerto       | Protocolo | Uso                          |
|-------------|-----------|------------------------------|
| `80/tcp`    | TCP       | BHTTP para SSH               |
| `443/tcp`   | TCP       | SSH_XHTTP y BTUN compartidos por TLS |
| `7080/tcp`  | TCP       | BTUN sobre BHTTP             |
| `7300/tcp`  | TCP       | BTUN nativo                  |
| `7300/udp`  | UDP       | BTUN nativo                  |
| `7443/tcp`  | TCP       | BTUN sobre XHTTP dedicado    |

Después de instalar, ejecuta `bhttp` para abrir el menú o `bhttp status` para
consultar el servicio principal.

## Requisitos de la VPS

El paquete no necesita internet, compilador, Go ni OpenSSL. La imagen base de la VPS
necesita Linux AMD64 o ARM64, `systemd`, OpenSSH, PAM/glibc, `iproute2`,
`iptables` y `/dev/net/tun`. Estos componentes normalmente ya existen en
Ubuntu Server 22.04/24.04 y Debian 12 con OpenSSH instalado.

> El `setup.sh` instala automáticamente las dependencias básicas (`git`, `iptables`,
> `iproute2`) si detecta que faltan, usando `apt-get`, `dnf`, `yum` o `apk`.

El instalador verifica todo antes de modificar los servicios. Si algo está ausente,
termina con un mensaje claro.

## Contenido del paquete

| Archivo/Carpeta | Descripción |
|----------------|-------------|
| `bin/amd64/` | Binarios pre-compilados para x86-64 |
| `bin/arm64/` | Binarios pre-compilados para AArch64 |
| `sources/bilola_go_port/` | Código fuente completo de la versión pré-ZTUN 1.0.44 |
| `tools/certgen/main.go` | Fuente del generador de certificado incorporado |
| `bhttp-menu` | Menú local de gestión |
| `setup.sh` | Bootstrap de instalación/actualización vía GIT |
| `update.sh` | Actualizador automático vía GIT |
| `ports.env` | Configuración de puertos por defecto |
| `SHA256SUMS` | Hashes para verificar todos los archivos del paquete |

Para verificar la integridad del paquete:

```bash
sha256sum -c SHA256SUMS
```

El instalador guarda la configuración existente en
`/root/bhttp-preztun-backup-AAAAMMDD-HHMMSS` antes de reemplazarla.
