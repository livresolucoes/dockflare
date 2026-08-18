# Problemas comuns

[← README](../README.md)

## Que comando usar depois de mudar o quê

| Mudou | Comando |
|---|---|
| `config.yml` | nada — hot reload. Ou `docker compose restart dockflare` |
| `.env` (tokens) | `docker compose up -d` — o compose recria com o env novo |
| Código Go / Dockerfile | `docker compose up -d --build` |

`docker compose up -d` **não** funciona para mudança só no `config.yml`: o compose compara a definição do serviço, não o conteúdo dos arquivos montados, e responde `up-to-date`.

---

## YAML inválido — `did not find expected '-' indicator`

O container reinicia em loop repetindo a mesma linha. Duas causas:

**Faltou o `-` num item de `routes:`.** Cada rota é um item da lista:

```yaml
routes:
  - hostname: a.example.com     # ✅ todo bloco novo começa com "- "
    container: web              #    e os campos alinham sob o "h"
    port: 80

  - hostname: b.example.com     # ✅
    container: api
    port: 3000
```

**Descomentou os itens mas esqueceu a chave.** Clássico ao partir do `config.example.yml`:

```yaml
# routes:                       # ❌ ainda comentado
   - hostname: a.example.com    #    lista órfã
```

Diagnóstico:

```bash
grep -n '' config/config.yml     # arquivo com números de linha
grep -nP '\t' config/config.yml  # tab é proibido em YAML
```

Corrigido o arquivo, o container pega sozinho no próximo restart.

---

## `403 / 10000 Authentication error`

Falta uma permissão no API token. Qual, depende do endpoint no erro:

| No erro | Falta |
|---|---|
| `cfd_tunnel/.../configurations` | `Account · Cloudflare Tunnel · Edit` |
| `dns_records` | `Zone · DNS · Edit` |
| `rulesets/phases/http_request_dynamic_redirect` | `Zone · Single Redirect · Edit` |

Teste direto, sem adivinhar:

```bash
ZONE_ID=$(curl -s "https://api.cloudflare.com/client/v4/zones?name=example.com" \
  -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" | grep -o '"id":"[^"]*' | head -1 | cut -d'"' -f4)

curl -s "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/rulesets/phases/http_request_dynamic_redirect/entrypoint" \
  -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" | head -c 300
```

| Resposta | Significado |
|---|---|
| `"success": true` | permissão OK |
| `10007 / ruleset not found` | permissão OK, a zona só não tem regras ainda |
| `403 / 10000` | falta a permissão nesta zona |

**Se o `manage_dns` funciona mas o `force_https` dá 403**, o token vê a zona e tem DNS — falta só o grupo de Rules.

**Se já adicionou a permissão e continua 403**, é o teto dos seus roles de membro. Abra o dashboard → a zona → `Rules` → `Redirect Rules`. Se a seção não aparece, mexer no token não resolve — ver [Permissões](configuracao.md#permissões-da-cloudflare).

A permissão é concedida **por zona**: com vários domínios ela pode faltar em alguns. O DockFlare trata cada zona independentemente — as que têm permissão recebem suas regras, as que não têm aparecem no `[WARN]`.

---

## `502 Bad Gateway` ou loop de redirect num hostname

O container provavelmente só fala HTTPS. Marque `origin_scheme: https` naquela rota — ver [HTTPS](configuracao.md#origin_scheme--quando-o-container-só-fala-https).

Se **todos** os hostnames de uma stack derem 502 depois de um `docker compose down && up` nela: a rede Docker foi recriada com ID novo e o DockFlare ficou de fora. O cache dele é por nome, então nem um reload reconecta.

```bash
docker compose restart dockflare
```

---

## A interface web não salva

O banner vermelho diz `config.yml não é gravável`. O DockFlare também avisa no startup.

Monte o **diretório**, sem `:ro`:

```yaml
volumes:
  - ./config:/config     # ✅
# - ./config.yml:/config/config.yml:ro   # ❌
```

```bash
cd ~/dockflare && mkdir -p config && mv config.yml config/
# ajuste o volume no docker-compose.yml
docker compose up -d
```

---

## Hot reload não dispara ao editar o arquivo

Dois motivos, ambos ligados ao bind mount de **arquivo único**:

1. O watcher observa o diretório pai. Com um arquivo enxertado, escritas no host não geram evento inotify lá dentro.
2. `sed -i` e o `vim` salvam criando arquivo novo + rename. Isso **quebra** o mount: o container continua vendo o arquivo antigo para sempre.

Montar o diretório (`./config:/config`) resolve os dois. Enquanto isso, `docker compose restart dockflare` sempre funciona.

---

## `cloudflared` se atualiza e reinicia sozinho no startup

```
[INFO] [cloudflared] cloudflared has been updated version=2026.8.2
[WARN] cloudflared exited unexpectedly (exit status 11), restarting in 3s
```

O binário tem autoupdate ligado, se troca dentro do container e sai com código 11. O DockFlare o reinicia e tudo volta, mas você paga um ciclo em cada subida e a versão que roda não é a pinada no Dockerfile.

Correção: `--no-autoupdate` no comando e subir o `CLOUDFLARED_VERSION` no Dockerfile.

---

## Posso fechar todas as portas do servidor?

Para os hostnames que vão pelo túnel, sim — a conexão é **de dentro para fora**.

| Porta | Ação |
|---|---|
| 80, 443 entrada | ✅ pode fechar |
| Portas das aplicações | ✅ pode fechar |
| 8080 (interface web) | já fechada, presa em `127.0.0.1` |
| **22 · SSH** | ⚠️ **mantenha** — é seu acesso ao servidor |

Saída precisa estar liberada: UDP/TCP `7844` para `*.v2.argotunnel.com`, TCP `443` para `api.cloudflare.com`, e DNS.

> ⚠️ **`ufw` não bloqueia porta publicada pelo Docker.** O Docker escreve regras `iptables` avaliadas **antes** das do `ufw`, então `ufw deny 4000` não tem efeito. A correção real é remover o bloco `ports:` do compose da aplicação.

Conferindo:

```bash
docker ps --format 'table {{.Names}}\t{{.Ports}}'   # o que está publicado
ss -tulpn | grep -v '127.0.0.1\|::1'                # o que escuta em 0.0.0.0
```

Serviço que **não é HTTP** (banco, SMTP) não vai pelo túnel com o DockFlare — a porta dele continua necessária.

---

[← README](../README.md) · [Configuração](configuracao.md) · [Arquitetura](arquitetura.md)
