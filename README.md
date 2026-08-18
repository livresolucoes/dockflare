# DockFlare

**Cloudflare Tunnel para Docker, do jeito certo.**

Liste seus containers num arquivo. Seus domínios ficam no ar com HTTPS, sem abrir nenhuma porta no servidor.

```
   meuapp.example.com     ──┐                      ┌──►  meuapp_web:4000
                            ├──► Cloudflare Tunnel ┤
   api.meuapp.example.com ──┘                      └──►  meuapp_api:3000
                                         ▲
                                   config.yml
```

## O que você escreve

```yaml
containers:
  - meuapp_web
  - meuapp_api

routes:
  - hostname: meuapp.example.com
    container: meuapp_web
    port: 4000

  - hostname: api.meuapp.example.com
    container: meuapp_api
    port: 3000
```

## O que você recebe

- Os dois hostnames no ar, com HTTPS e certificado gerenciado pela Cloudflare
- Nenhuma porta aberta no servidor — nem 80, nem 443
- `cloudflared` alcançando os containers pelo nome, dentro da rede Docker
- Roteamento versionado no git, não clicado num dashboard

Salvou o arquivo, está publicado. Tirou a rota, está despublicado.

---

## Comece

**1.** Crie um túnel em [Zero Trust](https://one.dash.cloudflare.com/) → Networks → Tunnels → Cloudflared, e copie o **connector token**.

**2.** Crie um [API token](https://dash.cloudflare.com/profile/api-tokens) com a permissão `Account · Cloudflare Tunnel · Edit`.

**3.** Monte os arquivos:

```bash
mkdir -p dockflare/config && cd dockflare
cp docker-compose.example.yml docker-compose.yml
cp config.example.yml config/config.yml
```

```bash
# .env
TUNNEL_TOKEN=eyJhIjoiYjk3Yz...
CLOUDFLARE_API_TOKEN=v1.0-abc123...
```

**4.** Edite `config/config.yml` com seus containers e rotas.

**5.** Suba:

```bash
docker compose up -d --build
docker logs -f dockflare
```

```
[INFO] Connected container meuapp_web to network meuapp_default
[INFO] Ingress route meuapp.example.com → http://meuapp_web:4000
[INFO] Ingress updated: 2 routes
[INFO] cloudflared started (pid 10)
```

Pronto.

> **`port` é a porta de dentro do container.** Se sua app publica `"8090:3000"`, use **3000**. O tráfego não sai para o host — você pode até apagar o bloco `ports:` da sua aplicação.

---

## Features

Tudo opcional vem **desligado**. O mínimo acima já funciona.

| | O que faz | Como liga |
|---|---|---|
| 🐳 **Redes Docker** | Entra sozinho nas redes dos containers | automático |
| 🔀 **Roteamento** | `hostname → container:porta` pelo arquivo | `routes:` |
| 🔒 **HTTPS** | Certificado automático. Redirect `http→https` por hostname | `force_https: yes` |
| 🌐 **DNS** | Cria o CNAME de cada hostname | `manage_dns: true` |
| 🖥️ **Interface web** | Editar pelo navegador, com validação | `web_ui.enabled` |
| ♻️ **Hot reload** | Aplica mudanças sem reiniciar o túnel | automático |
| 🔐 **Segredos** | Só em variável de ambiente, nunca no arquivo | por design |

Detalhes de cada uma em **[docs/configuracao.md](docs/configuracao.md)**.

---

## Quando usar

| | DockFlare | nginx / Traefik + Let's Encrypt |
|---|---|---|
| Portas abertas no servidor | **nenhuma** | 80 e 443 |
| Certificado TLS | da Cloudflare | você renova |
| Config de proxy reverso | não existe | `nginx.conf` / labels |
| Precisa de IP público | **não** (funciona atrás de NAT) | sim |
| Roteamento por path, middlewares, auth | ❌ | ✅ |
| Serviços não-HTTP (banco, SMTP) | ❌ | ✅ |
| Independe da Cloudflare | ❌ | ✅ |

Se você precisa de middleware, roteamento por path ou serviço TCP, use Traefik. Se você quer **`hostname → container:porta` e mais nada**, o DockFlare é bem menos coisa para manter.

---

## Documentação

| | |
|---|---|
| **[docs/configuracao.md](docs/configuracao.md)** | Todas as features em detalhe, schema completo, permissões da Cloudflare, API da interface web |
| **[docs/problemas.md](docs/problemas.md)** | Erros comuns e como diagnosticar |
| **[docs/arquitetura.md](docs/arquitetura.md)** | Pacotes, fluxo de dados, decisões de design |

## Build

```bash
go build -o bin/dockflare ./cmd/dockflare
go test ./...
docker build -t dockflare:latest .
```

Go, binário único, sem banco de dados. A interface web entra no binário via `go:embed`.

## License

MIT
