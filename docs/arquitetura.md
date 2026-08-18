# Arquitetura

[← README](../README.md)

## Pacotes

```
cmd/dockflare/
  main.go             entrypoint; conecta todos os pacotes
  reload.go           o reloader compartilhado por watcher e interface web

internal/
  config/             parsing YAML, tokens via env, validação estática, Save atômico
  docker/             SDK do Docker: inspecionar, listar, conectar/desconectar redes
  network/            orquestra as conexões de rede por container
  ingress/            routes → ingress rules; resolução do destino; sync com o túnel
  cloudflare/         cliente REST (ingress, DNS, redirect rules) — único lugar com HTTP
  cloudflared/        subprocesso cloudflared (start/stop/reload/crash recovery)
  watcher/            fsnotify; dispara reload ao mudar o config.yml
  web/                interface opcional: API JSON + página via go:embed
  logger/             logger estruturado [INFO]/[WARN]/[ERROR]
```

## Fluxo de dados

```
                       ┌──────────────┐
   📄 config.yml ─────►│   config     │  YAML + env vars + validação estática
                       └──────┬───────┘
                              │  Config{Token, Containers, Routes, ManageDNS, WebUI}
                              ▼
                       ┌──────────────┐         ┌──────────────┐
                       │   network    │◄───────►│    docker    │  redes, portas expostas
                       └──────┬───────┘         └──────▲───────┘
                              │  entra nas redes       │
                              ▼                        │
                       ┌──────────────┐                │
                       │   ingress    │────────────────┘  valida contra o Docker vivo
                       └──────┬───────┘
                              │  ingress rules + catch-all
                              ▼
                       ┌──────────────┐
                       │  cloudflare  │  PUT só se o estado desejado diferir
                       └──────┬───────┘
                              ▼
                     ☁️  Cloudflare empurra a config
                              ▼
                       ┌──────────────┐
                       │ cloudflared  │  subprocesso; não reinicia por mudança de rota
                       └──────────────┘

   ♻️  watcher  ──┐
   🖥️  web UI   ──┴──►  reload.Reload()  ── serializado por mutex ──► refaz o fluxo
```

Redes são sincronizadas **antes** da validação das rotas: verificar se o DockFlare alcança um container só faz sentido depois do join.

## Endpoints da Cloudflare usados

| Operação | Endpoint | Quando |
|---|---|---|
| Ler o ingress | `GET /accounts/{account}/cfd_tunnel/{tunnel}/configurations` | com `routes` |
| Gravar o ingress | `PUT /accounts/{account}/cfd_tunnel/{tunnel}/configurations` | com `routes`, se mudou |
| Localizar a zona | `GET /zones` | com `manage_dns` ou `force_https` |
| CNAME | `GET`/`POST`/`PATCH /zones/{zone}/dns_records` | com `manage_dns` |
| Redirect rules | `GET`/`PUT /zones/{zone}/rulesets/phases/http_request_dynamic_redirect/entrypoint` | com `force_https` |

É a mesma API que o dashboard usa ao salvar um Public Hostname. Todo o HTTP fica em `internal/cloudflare` — nenhum outro pacote monta requisição para a Cloudflare.

O `GET` antes de cada `PUT` é o que preserva `warp-routing`, `originRequest` e regras de redirect alheias. O endpoint de redirect responde 404 numa zona sem regras; `ErrNotFound` faz isso significar "vazio", não "quebrado".

## Decisões de design

**Token-only para o túnel.** Connector tokens do Zero Trust são o único método de autenticação do `cloudflared`. O DockFlare nunca escreve arquivo de ingress local: mesmo no modo automático, ele grava o estado *remoto* que a Cloudflare envia ao connector.

**O account ID e o tunnel ID saem do próprio token.** Ele é base64 de `{"a":account,"t":tunnel,"s":secret}`, então não há identificador extra no config.

**Tudo opcional é opt-in, e nada opcional é destrutivo por omissão.** Sem `routes`, zero chamadas à API. Sem `manage_dns`, nenhum registro DNS. Sem `web_ui`, nenhuma porta. Publicar a interface pelo túnel é um *segundo* opt-in, e exige `routes` — gravar ingress onde antes não havia apagaria o roteamento do dashboard.

**Estado completo, nunca parcial.** O ingress é substituído por inteiro, então qualquer falha de validação aborta a atualização. Aplicar o subconjunto válido derrubaria hostnames que estavam funcionando.

**Listas compartilhadas são mescladas, nunca substituídas.** O ruleset de redirect e o `originRequest` de cada regra contêm configuração que o DockFlare não gerencia. Regras alheias voltam idênticas (menos `version`/`last_updated`), identificadas pelo prefixo `dockflare:` na descrição, e as do DockFlare vão **no fim** — regra pré-existente mantém prioridade. Dentro do `originRequest`, o DockFlare é dono apenas de `noTLSVerify`.

**Um único caminho de reload, e ele não reinicia o túnel.** `cmd/dockflare/reload.go` é compartilhado pelo watcher e pela interface, serializado por mutex. O `cloudflared` só cai quando o `TUNNEL_TOKEN` muda.

**Segredos não conseguem chegar no arquivo, por construção.** O `config.Save` serializa um tipo `fileDoc` separado do `Config` — campo novo em memória não aparece no arquivo por descuido — e o `token:` vem do arquivo em disco, nunca de `cfg.Token`, que pode conter o valor de `TUNNEL_TOKEN`. A escrita é atômica (temp + rename), o que é por que o *diretório* do config precisa estar montado, não o arquivo.

**A interface não consegue se trancar.** O `PUT /api/config` ignora o campo `webUi` do request e lê essas configurações do disco. Desligar a interface pela interface tiraria o usuário da página que ele está usando.

**A API nunca emite `null` onde promete lista.** Um slice `nil` em Go serializa como `null`, e o navegador então lê `.length` dele. Container sem porta publicada é o caso comum com túnel, não a exceção.

**Preocupações secundárias avisam, não falham.** Erros de DNS e de redirect rules são `[WARN]`: o ingress já está no ar, e ambos dependem de permissões de zona que o túnel não exige.

**Sem banco de dados.** A configuração é um arquivo YAML; a interface edita esse arquivo e nada mais.

## Formato dos logs

```
[INFO] Connected container meuapp_api to network meuapp_default
[WARN] Container grafana not found, skipping
[INFO] Network sync complete: 2 containers
[INFO] Ingress route api.meuapp.example.com → http://meuapp_api:3000
[INFO] Ingress updated: 2 routes
[INFO] DNS record api.meuapp.example.com → 8a7b....cfargotunnel.com (proxied)
[INFO] HTTPS redirect rules updated in zone meuapp.example.com
[INFO] Web UI listening on port 8080
[INFO] cloudflared started (pid 10)
[INFO] Config changed, reloading
[INFO] Ingress unchanged, skipping Cloudflare API (2 routes)
```

## Limitações conhecidas

**Recriar uma stack derruba o roteamento dela.** `docker compose down && up` numa aplicação recria a rede Docker com ID novo; o DockFlare fica de fora e o cache `joinedNets` (por nome) impede o rejoin. Precisa de `docker compose restart dockflare`. Resolver de verdade pede escutar eventos do Docker em vez de depender só do watcher de arquivo.

**Sem retry periódico.** Se o sync do ingress falhar por instabilidade da API, a próxima tentativa só acontece quando o `config.yml` mudar ou o container reiniciar. Um ticker de reconciliação resolveria.

**O limite de 5 tentativas do crash recovery não existe na prática.** `watchCrash` é sempre chamado com o contador em zero, então o retry é infinito com backoff fixo de 3s — na prática o comportamento desejável para um túnel, mas diferente do que o código sugere.

**`force_https` pode deixar uma regra órfã.** Remover o flag de todos os hostnames de um domínio e reiniciar no mesmo movimento perde a memória de quais zonas foram visitadas.

## Extensibilidade futura

A arquitetura não bloqueia: auto-discovery por labels do Docker, túneis TCP, rotas multi-target / Cloudflare Load Balancing, healthchecks, métricas Prometheus.

O campo `targets:` já tem o encaixe pronto — `config.Route.Targets()` devolve uma lista, então a forma multi-destino pode entrar sem remodelar o pipeline.

## Build

```bash
go build -o bin/dockflare ./cmd/dockflare              # binário local
CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
  -o bin/dockflare ./cmd/dockflare                     # estático, para Alpine
docker build -t dockflare:latest .                      # imagem
go test ./...                                           # testes
golangci-lint run                                       # lint
```

A imagem é multi-stage: compila o binário estático, baixa o `cloudflared` da release oficial, e copia os dois num `alpine`. Os arquivos da interface web entram no binário via `go:embed`, então a imagem final tem só dois executáveis.

---

[← README](../README.md) · [Configuração](configuracao.md) · [Problemas comuns](problemas.md)
