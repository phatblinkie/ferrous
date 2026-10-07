# ferrous

Self-hosted game-server control plane: a **containerized central panel** (Flask + vanilla JS)
that dials out to **lightweight Go agents** on remote hosts, which manage **dockerized Rust
servers** — deploy, power, live logs, players, inventory, console, settings.

```
browser ── ferrous panel (central, docker compose)
              │ HTTPS + Bearer token
              ▼
         ferrous-agent (Go, static binary, listens)
              ├── docker: deploy / power / stats / files
              ├── logs: docker logs -f → throttled stream   (wings-style output)
              ├── rcon: hub on host, port never published   (correlated queries)
              └── data dir per server
                        │
                        └── ferrous/rustserver image (steamcmd + oxide match + start.sh)
```

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | repo scaffold, API contract, agent skeleton (token auth, `/ping`, docker discovery) | ✅ |
| 2 | agent MVP: power actions, stats, `docker logs -f` SSE stream (throttled) | ✅ |
| 3 | central panel MVP: host/server registry, status, power, live logs end-to-end | ✅ |
| 4 | RCON through agent: players, inventory (InvDump), console | ✅ |
| 5 | deployment wizard + server file API (configs, oxide plugins) | ✅ |
| 6 | `ferrous/rustserver` image: entrypoint from proven start.sh/auto-update.sh | ▢ |
| 7 | polish: install one-liner, docs, agent version-skew notice | ▢ |

## Quickstart (agent)

```sh
make build
FERROUS_TOKEN=changeme ./bin/ferrous-agent --listen 127.0.0.1:8710
# in another shell:
curl -H 'Authorization: Bearer changeme' http://127.0.0.1:8710/api/v1/ping
curl -H 'Authorization: Bearer changeme' http://127.0.0.1:8710/api/v1/system
curl -H 'Authorization: Bearer changeme' http://127.0.0.1:8710/api/v1/servers
```

If `FERROUS_TOKEN` is empty the agent generates one and prints it at startup (first-boot UX).

## Quickstart (panel)

```sh
docker compose up -d --build        # → http://localhost:8122
```

- First boot creates the admin user: credentials come from `PANEL_ADMIN_USER` /
  `PANEL_ADMIN_PASSWORD` (compose env), otherwise a random password is generated
  and printed in `docker logs ferrous-panel`.
- In the **Hosts** tab: `＋ add host` → name, agent base url (`http://ip:8710`),
  agent token (from the agent's startup log) → **test** → **save**.
- The **Servers** tab lists every container across hosts: power buttons, live console
  (agent `docker logs -f` → panel → browser, throttled at 200 lines/s), and
  `＋ deploy server` (image → data dir → ports/env → pull, create, start).
- The **Files** tab edits inside the selected server's data directory:
  configs as text (atomic save), oxide plugins as upload/download (base64).

**Panel env:** `FERROUS_DB` (default `ferrous.db`, set to `/data/…` in the image),
`PANEL_SECRET` (persisted next to the DB if unset), `PANEL_ADMIN_USER`, `PANEL_ADMIN_PASSWORD`,
`PANEL_BIND`/`PANEL_PORT` (default `0.0.0.0:8122`).

**Flags / env** (`flag > env > default`):

| flag | env | default |
|---|---|---|
| `--listen` | `FERROUS_LISTEN` | `127.0.0.1:8710` (use `0.0.0.0` + TLS for remote panel access) |
| `--token` | `FERROUS_TOKEN` | generated at startup |
| `--docker` | `FERROUS_DOCKER_SOCKET` | `/var/run/docker.sock` |
| `--tls-cert` / `--tls-key` | `FERROUS_TLS_CERT` / `FERROUS_TLS_KEY` | — (set both to enable HTTPS) |

## Repository layout

```
agent/    Go agent (ferrous-agent) — stdlib only, static binary
panel/    central panel (Flask + vanilla JS, containerized) — server.py, agents.py,
          UI, Dockerfile, pytest suite
image/    ferrous/rustserver Docker image                     [phase 6]
docs/     API contract (docs/api-v1.md)
```

## Security model (v1)

- Agent requires `Authorization: Bearer <token>` on every endpoint (constant-time compare,
  failed attempts logged with source IP).
- Item endpoints (`power`/`stats`/`logs`/`files`) only act on containers labeled
  `ferrous.managed=true` — the token never becomes arbitrary-container control.
  `POST /servers` (deploy) is the one token-wide capability: it creates new
  containers from a validated spec (agent adds the managed labels, data bind
  and `unless-stopped` restart itself — no privileged/raw create passthrough).
  Bind to localhost/VPN, or enable TLS (`--tls-cert/--tls-key`) before exposing
  across a network.
- The Docker socket is never exposed as HTTP itself — the agent proxies a narrow API
  (discovery → power → logs → files scoped to server data dirs).
- Panel: single DB-backed admin (scrypt hashes, 5-strike login lockout), signed
  session cookies, CSRF header (`X-Requested-With: ferrous`) on every mutation,
  agent tokens masked in API responses (`••••last4` — they never reach the browser).
  Put the panel behind a reverse proxy with TLS (or a VPN) before exposing it:
  sessions are plain HTTP in v1.
- The panel dials the agent (never the reverse): expose nothing on the agent beyond
  localhost/VPN/TLS as above.

## Acknowledgements

- Console streaming / docker lifecycle patterns reference **Pelican Wings**
  (<https://github.com/pelican/wings>, MIT license).
- Panel design, RCON layer, inventory (InvDump plugin), and log pipeline evolved from our
  internal rustpanel project.

## License

MIT — see [LICENSE](LICENSE).
