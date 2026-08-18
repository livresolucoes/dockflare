# Configuração

[← README](../README.md)

- [Roteamento](#roteamento)
- [HTTPS](#https)
- [DNS](#dns)
- [Interface web](#interface-web)
- [Hot reload](#hot-reload)
- [Schema completo](#schema-completo)
- [Variáveis de ambiente](#variáveis-de-ambiente)
- [Permissões da Cloudflare](#permissões-da-cloudflare)

---

## Roteamento

Sem a seção `routes`, o DockFlare não faz nenhuma chamada à API e o dashboard Zero Trust continua no comando. Com ela, o `config.yml` é a fonte da verdade — incluindo remoções.

```yaml
containers: [meuapp_web, meuapp_api, loja_web]

routes:
  - { hostname: meuapp.example.com,     container: meuapp_web,   port: 4000 }
  - { hostname: api.meuapp.example.com, container: meuapp_api,   port: 3000 }
  - { hostname: loja.example.com,        container: loja_web, port: 80   }
```

Redes Docker diferentes, domínios diferentes, um único túnel. A ordem do arquivo é preservada: a Cloudflare avalia o ingress de cima para baixo.

### `port` é a porta de dentro

O erro mais comum. Se sua aplicação publica assim:

```yaml
# docker-compose.yml da SUA aplicação
services:
  api:
    container_name: meuapp_api
    ports:
      - "8090:3000"       #  host 8090  ──►  container 3000
```

O DockFlare usa **3000**:

```
    ┌─── servidor ────────────────────────────────┐
    │                                             │
    │   dockflare ──┐                             │
    │               │  rede Docker                │
    │               └──► http://meuapp_api:3000   │
    │                                             │
    │   :8090   x  nem precisa existir            │
    └─────────────────────────────────────────────┘
```

O tráfego nunca sai para o host. Você pode **apagar o bloco `ports:`** da sua aplicação — ela continua acessível pelo túnel, agora sem porta aberta no servidor.

### Regra catch-all

O ingress é sempre fechado com `- service: http_status:404`. Você não declara e não tem como omitir: hostname sem rota recebe 404 em vez de cair num serviço arbitrário.

### Validação antes de aplicar

Nada inválido chega na Cloudflare. Uma rota ruim aborta a atualização **inteira** — como o ingress é substituído por completo, aplicar só a parte válida derrubaria os hostnames restantes em silêncio.

| Problema | Mensagem |
|---|---|
| Container não existe | `Route api.example.com references container "api" but that container was not found.` |
| Porta não exposta | `... but port 3000 is not available. Exposed ports: 8080, 9090.` |
| Fora do alcance | `... but DockFlare is not connected to any of its Docker networks (private_net).` |
| Campo faltando | `route #2 is missing a hostname` |
| Hostname repetido | `route api.example.com is declared more than once` |
| Porta fora da faixa | `route api.example.com has an invalid port 70000 (must be 1-65535)` |

Erro de configuração no **startup** para o processo — só você corrige. Falha de **rede/API** gera `[ERROR]` e segue: o túnel sobe e a próxima sincronização tenta de novo.

Container que não declara nenhuma porta (sem `EXPOSE`) passa na validação de porta: não há como provar que está errada, e bloquear seria pior que aceitar.

---

## HTTPS

São **três trechos independentes**, e dois já vêm prontos:

```
   Navegador          Cloudflare         cloudflared        container
       │                  │                   │                 │
       │─── trecho 1 ────►│                   │                 │
       │  🔒 automático   │                   │                 │
       │                  │─── trecho 2 ─────►│                 │
       │                  │  🔒 é o túnel     │                 │
       │                  │                   │─── trecho 3 ───►│
       │                  │                   │  origin_scheme  │
```

| Trecho | Estado | Campo |
|---|---|---|
| Navegador → Cloudflare | ✅ pronto | — |
| Cloudflare → `cloudflared` | ✅ pronto | — |
| `cloudflared` → container | HTTP por padrão | `origin_scheme` |

**`https://app.example.com` já responde** sem configuração. Os dois campos abaixo resolvem casos específicos.

Duas ressalvas de plataforma: o certificado automático cobre `app.example.com` e o domínio raiz, mas **não** subdomínio de segundo nível (`a.b.example.com`) — isso exige o Advanced Certificate Manager, pago. E o modo SSL/TLS da zona deve estar em **Full (strict)**.

### `force_https` — redirecionar quem chega por HTTP

Link antigo no favorito, URL colada num chat, `curl http://...`. Hoje esses são atendidos em HTTP: a página carrega, mas sem cadeado.

```yaml
routes:
  - hostname: grafana.example.com
    container: grafana
    port: 3000
    force_https: yes        # true / on / yes
```

```
  http://grafana.example.com  ──►  301  ──►  https://grafana.example.com
```

**Por hostname, não por domínio.** É uma Redirect Rule escopada nesse hostname — não a chave "Always Use HTTPS" da zona, que valeria para todos os hostnames do domínio, inclusive os que não são seus.

Como as Redirect Rules são uma lista única por domínio, o DockFlare lê a lista, preserva o que não é dele e regrava. Regras alheias voltam **idênticas** (ele identifica as próprias pela descrição começando com `dockflare:`) e as dele vão **no fim** — regra pré-existente mantém prioridade.

**Precisa de:** `Zone · Single Redirect · Edit`. Sem isso, o resto funciona e você vê um `[WARN]`.

> Ao remover `force_https` de **todos** os hostnames de um domínio e reiniciar o DockFlare no mesmo movimento, a última regra pode ficar para trás — a memória de quais zonas foram visitadas é por processo. Apague pelo dashboard (Rules → Redirect Rules) ou remova uma por vez.

### `origin_scheme` — quando o container só fala HTTPS

Vault, nginx só com `listen 443 ssl`, API .NET com `UseHttpsRedirection`. Sintoma: `502 Bad Gateway` ou loop infinito de redirect.

```yaml
routes:
  - hostname: vault.example.com
    container: vault
    port: 8200
    origin_scheme: https      # padrão: http
```

Gera:

```yaml
- hostname: vault.example.com
  service: https://vault:8200
  originRequest:
    noTLSVerify: true         # adicionado automaticamente
```

O `noTLSVerify` é obrigatório: o certificado de um container nunca bate com o nome dele na rede Docker. Não é perda relevante — esse trecho não sai do seu servidor.

> **Na dúvida, deixe `http`.** Grafana, Node, Django e nginx padrão falam HTTP; marcar `https` neles quebra a conexão.

O DockFlare é dono apenas da chave `noTLSVerify` dentro de `originRequest`. Outras opções configuradas por fora (`connectTimeout`, `httpHostHeader`) são preservadas.

---

## DNS

O ingress diz ao túnel para onde mandar o tráfego. O hostname ainda precisa de um CNAME apontando para o túnel.

```yaml
manage_dns: true
```

```
  api.example.com  ──CNAME──►  8a7b6c5d-....cfargotunnel.com   (proxied 🟠)
```

**Padrão `false`**, porque escrever na zona DNS é um efeito maior que escrever o ingress. Com `false`, crie o CNAME uma vez pelo dashboard.

**Precisa de:** `Zone · DNS · Edit`.

Registro que já existe e **não** aponta para um túnel é deixado intacto, com aviso — o DockFlare não repointa o `A` de produção de ninguém:

```
[WARN] DNS for api.example.com: DNS record for api.example.com already exists
       as A → 203.0.113.10 and does not point at a tunnel; leaving it untouched
```

Domínio raiz funciona: a Cloudflare faz *CNAME flattening* automático em registros proxied.

---

## Interface web

Editar tudo pelo navegador, com validação antes de gravar.

```
  ┌──────────────────────────────────────────────────────────────┐
  │ DockFlare                              [tunel ativo]  [Sair] │
  ├──────────────────────────────────────────────────────────────┤
  │ STATUS    cloudflared   rodando (pid 10)                     │
  │           redes         app_net, loja_net                    │
  │           config.yml    gravavel                             │
  ├──────────────────────────────────────────────────────────────┤
  │ CONTAINERS   ( meuapp_web x )   ( meuapp_api x )   [ + v ]   │
  ├──────────────────────────────────────────────────────────────┤
  │ ROTAS   HOSTNAME                 CONTAINER      PORTA  ORIG  │
  │         meuapp.example.com       meuapp_web  v  4000   http v│
  │         api.meuapp.example.com   meuapp_api  v  3000   http v│
  │         [ Adicionar rota ]                                   │
  ├──────────────────────────────────────────────────────────────┤
  │                  [Validar]  [Recarregar]  [Salvar e aplicar] │
  └──────────────────────────────────────────────────────────────┘
```

- Picker de containers alimentado pelo Docker — nome que não existe aparece sinalizado
- **Validar** mostra os mesmos erros que apareceriam no log, antes de gravar
- **Salvar e aplicar** grava e recarrega **sem reiniciar o túnel**
- Salvar com a tabela vazia exige confirmação nominal, para um erro de tela não derrubar tudo

**Desligada por padrão.** A tela reescreve o roteamento de produção; ligar é uma decisão, não um acidente.

### Ligando

**1 · Token** — mínimo 32 caracteres, o DockFlare recusa subir com menos:

```bash
echo "DOCKFLARE_UI_TOKEN=$(openssl rand -hex 32)" >> .env
```

**2 · Monte o DIRETÓRIO do config, read-write** — obrigatório:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro
  - ./config:/config          # o diretório, sem :ro
```

Seu arquivo vai para `./config/config.yml`. Montar o **arquivo** impede a gravação atômica (temp + rename) e impede o hot reload de receber os eventos de inotify.

**3 · Ligue:**

```yaml
web_ui:
  enabled: true
  port: 8080
```

### Chegando na tela

**A · Túnel SSH** (padrão, nada exposto):

```yaml
ports:
  - "127.0.0.1:8080:8080"     # preso no localhost do servidor
```

```bash
ssh -L 8080:127.0.0.1:8080 usuario@servidor
# abra http://localhost:8080
```

**B · Publicada pelo próprio túnel** (segundo opt-in, separado):

```yaml
web_ui:
  enabled: true
  hostname: dockflare.example.com
```

A origem é `http://localhost:8080` — o `cloudflared` roda ao lado da interface, no mesmo container, então não há rede Docker envolvida. Requer `routes:` configurado: publicar a interface grava o ingress, e sem rotas o DockFlare não gerencia ingress — começar a gerenciar apagaria o que o dashboard está servindo.

> ⚠️ **Proteja com Cloudflare Access.** Zero Trust → Access → Applications → Add an application → Self-hosted → seu domínio → Policy: Allow → Include → Emails → o seu. Com Access na frente, um estranho não chega nem na tela de login.

### O que protege a interface

| Camada | Comportamento |
|---|---|
| 🔑 Token | 32+ caracteres, comparado em tempo constante |
| 🍪 Sessão | Cookie `HttpOnly` + `SameSite=Strict` com id aleatório, nunca o token. 12h |
| 🔒 HTTPS | Login por `http://` através do túnel é **recusado** com 403 |
| 🙈 Segredos | Nenhum token aparece em resposta da API nem é gravado no arquivo |
| 🛡️ Headers | CSP `default-src 'self'`, `X-Frame-Options: DENY`, `nosniff` |
| 🤖 Automação | `Authorization: Bearer $DOCKFLARE_UI_TOKEN` também funciona |

As configurações da própria interface são somente leitura, de propósito: desligá-la por ali te trancaria fora.

### Consequência

A primeira gravação pela interface substitui os **comentários** do arquivo por um header de 4 linhas. A configuração é preservada, incluindo o campo `token:` se você usa o fallback em arquivo. `cp config/config.yml config/config.yml.bak` se quiser guardar o original.

### API

| Método | Rota | O que faz |
|---|---|---|
| `POST` | `/api/login` | troca o token por um cookie de sessão |
| `POST` | `/api/logout` | invalida a sessão |
| `GET` | `/api/config` | configuração atual, sem segredos |
| `POST` | `/api/validate` | valida sem gravar; devolve a lista de problemas |
| `PUT` | `/api/config` | valida → grava → recarrega. `422` inválido, `409` se removeria todas as rotas |
| `POST` | `/api/reload` | re-executa o pipeline |
| `GET` | `/api/status` | `cloudflared`, redes, se o config é gravável |
| `GET` | `/api/containers` | containers do host, com portas e alcançabilidade |

```bash
curl -s http://localhost:8080/api/status \
  -H "Authorization: Bearer $DOCKFLARE_UI_TOKEN"
```

---

## Hot reload

Salvou o arquivo? Aplicado. Sem reiniciar container, sem reiniciar túnel.

```
   config.yml muda
        │
        ├──► recarrega a config e valida as rotas
        ├──► sincroniza as redes Docker
        ├──► atualiza o ingress  ── só se mudou algo
        └──► o TUNNEL_TOKEN mudou? ── não ──► pronto, zero downtime
                    │
                   sim ──► reinicia o cloudflared
```

O `cloudflared` só é reiniciado quando o `TUNNEL_TOKEN` muda — é a única coisa que ele lê da linha de comando. Rota, DNS e redirect chegam pela config remota da Cloudflare.

Chamada à API só quando algo muda de verdade:

```
[INFO] Config changed, reloading
[INFO] Ingress unchanged, skipping Cloudflare API (8 routes)
```

---

## Schema completo

```yaml
# Só `containers` é típico. O resto é opcional e vem desligado.

containers:                       # containers a tornar alcançáveis
  - meuapp_web
  - meuapp_api

routes:                           # ausente → dashboard no comando
  - hostname: meuapp.example.com     # domínio público (aceita *.example.com)
    container: meuapp_web       # container de destino
    port: 4000                    # porta DENTRO do container
    origin_scheme: http           # opcional · http (padrão) | https
    force_https: no               # opcional · redirect http→https

manage_dns: false                 # opcional · cria o CNAME de cada hostname

web_ui:                           # opcional · ausente → desligada
  enabled: true
  port: 8080                      # dentro do container
  hostname: ui.example.com        # opcional · publica pelo túnel
```

Uma rota tem exatamente um destino nesta versão. A estrutura interna já trabalha com lista, então a forma abaixo pode entrar depois — **ainda não é aceita**:

```yaml
# ainda NÃO suportado
routes:
  - hostname: api.example.com
    targets:
      - { container: api_1, port: 3000 }
      - { container: api_2, port: 3000 }
```

---

## Variáveis de ambiente

| Variável | Quando | Para quê |
|---|---|---|
| `TUNNEL_TOKEN` | **sempre** | Connector token do Zero Trust dashboard |
| `CLOUDFLARE_API_TOKEN` | com `routes` | API token da Cloudflare |
| `DOCKFLARE_UI_TOKEN` | com `web_ui` | Senha da interface. 32+ chars: `openssl rand -hex 32` |

Nenhuma tem campo no `config.yml`, por design. Nenhuma aparece em log, erro ou resposta de API.

`TUNNEL_TOKEN` também aceita o campo `token:` no arquivo como fallback — a variável tem prioridade.

---

## Permissões da Cloudflare

**No API token** (`dash.cloudflare.com/profile/api-tokens` → Create Custom Token):

| Escopo | Grupo | Nível | Quando |
|---|---|---|---|
| `Account` | `Cloudflare Tunnel` | `Edit` | sempre que usar `routes` |
| `Zone` | `DNS` | `Edit` | com `manage_dns: true` |
| `Zone` | `Single Redirect` | `Edit` | com `force_https` |

> O grupo do `force_https` confunde: a API chama a fase de `http_request_dynamic_redirect`, mas o dashboard batizou o produto de **Single Redirects**. Digite `redi` no seletor — `Single Redirect` é o certo.

Precisa ser **API Token**, não a Global API Key: o DockFlare autentica via `Authorization: Bearer`.

### Se a conta Cloudflare é de outra pessoa

São duas camadas, e confundi-las é a causa mais comum de `403`:

```
   Roles de membro          ─── limitam ───►   Permissões do token
   (o dono da conta define)                    (você define)
```

Um token criado no seu perfil **não pode exceder seus roles de membro**. A caixinha aparece marcável, o token é criado sem erro, e ainda assim vem `403`.

Peça ao dono da conta:

| Role | Quando |
|---|---|
| `Cloudflare Zero Trust` | sempre |
| `DNS` | com `manage_dns` |
| `Zone Settings Admin` | com `force_https` |

Não precisa `Super Administrator` nem `Administrator`.

**Melhor alternativa:** o dono cria um **account-owned token** em `Manage Account → API Tokens` e te entrega só o valor. Não fica preso a uma pessoa e não amplia as permissões de ninguém.

### ⚠️ Túnel e domínio na mesma conta

Um CNAME `hostname → <tunnel-id>.cfargotunnel.com` só resolve se a zona e o túnel estiverem na **mesma conta** Cloudflare. Se os domínios estão na conta compartilhada, **crie o túnel lá**, não na sua conta pessoal.

O account ID sai do próprio token:

```bash
echo "$TUNNEL_TOKEN" | base64 -d
# {"a":"<account_id>","t":"<tunnel_id>","s":"..."}
```

---

[← README](../README.md) · [Problemas comuns](problemas.md) · [Arquitetura](arquitetura.md)
