# Tutorial — BHTTP pré-ZTUN 1.0.44

## Como usar o binário

O binário BHTTP é um servidor, não um menu interativo. Na VPS instalada ele está em:

```bash
/usr/local/lib/bilola/bilola-server
```

Para administrar o serviço:

```bash
systemctl status bilola-go-server
systemctl restart bilola-go-server
journalctl -u bilola-go-server -f
```

Para executá-lo manualmente, primeiro pare o serviço para evitar conflito na porta 80:

```bash
systemctl stop bilola-go-server

/usr/local/lib/bilola/bilola-server \
  --listen 0.0.0.0:80 \
  --target 127.0.0.1:22
```

Para disponibilizar o comando `bhttp`:

```bash
ln -s /usr/local/lib/bilola/bilola-server /usr/local/bin/bhttp
bhttp --help
```

Não execute `bhttp` diretamente na porta 80 enquanto `bilola-go-server` estiver ativo.

## Instalação em outra VPS

Envie o pacote pré-ZTUN para a nova VPS:

```bash
scp /root/BTUN-v1.0.44-pre-ZTUN-server-source.tar.gz root@IP_DA_NOVA_VPS:/root/
```

Entre na nova VPS e instale as dependências:

```bash
apt update
apt install -y golang-go gcc libpam0g-dev openssh-server iptables
```

Extraia o código-fonte:

```bash
cd /root
tar -xzf BTUN-v1.0.44-pre-ZTUN-server-source.tar.gz
cd bilola_go_port
```

Identifique a arquitetura e compile os binários:

```bash
case "$(uname -m)" in
  x86_64)  ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) echo "Arquitetura não suportada"; exit 1 ;;
esac

mkdir -p build

go build -trimpath -ldflags="-s -w" \
  -o "build/bilola-server-linux-${ARCH}" ./cmd/bilola-server

go build -trimpath -ldflags="-s -w" \
  -o "build/bilola-xhttp-server-linux-${ARCH}" ./cmd/xhttp-server

./build-btun.sh
```

## Instalar somente BHTTP na porta 80

```bash
install -d /usr/local/lib/bilola

install -m 755 \
  "build/bilola-server-linux-${ARCH}" \
  /usr/local/lib/bilola/bilola-server

install -m 644 deploy/bilola-go-server.service \
  /etc/systemd/system/bilola-go-server.service

sed -i 's/0.0.0.0:80,0.0.0.0:53/0.0.0.0:80/' \
  /etc/systemd/system/bilola-go-server.service

systemctl daemon-reload
systemctl enable --now bilola-go-server
```

Abra a porta no firewall:

```bash
ufw allow 80/tcp
```

Confira a instalação:

```bash
systemctl status bilola-go-server --no-pager
ss -lntp '( sport = :80 )'
journalctl -u bilola-go-server -n 50 --no-pager
```

## Configuração do aplicativo

```text
Servidor: IP da nova VPS
Porta: 80
Modo: BHTTP
SSH: porta 22
Usuário/senha: uma conta válida da nova VPS
```

## Arquivo utilizado

O pacote desta versão está em:

```text
/root/BTUN-v1.0.44-pre-ZTUN-server-source.tar.gz
```

Esta é a versão 1.0.44 anterior à integração do ZTUN.
