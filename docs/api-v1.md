# ferrous agent API — v1

All endpoints are served by `ferrous-agent` on the listen address. Version prefix: `/api/v1`.

## Conventions

- **Auth:** every request needs `Authorization: Bearer <token>` (agent config). Missing/wrong
  token → `401 {"error":"unauthorized"}` (constant-time compare; failures logged with IP).
- **Errors:** JSON body `{"error": "human-readable"}`.
  - `401` bad/missing token · `403` container exists but is **not ferrous-managed** ·
    `404` unknown path/container · `405` wrong method · `400` bad parameter/body
  - `502` docker layer failure (daemon down / API error) — panel shows it as a friendly
    host-level error, never a crash.
- **Managed gate:** `/{id}/power|stats|logs` only operate on containers labeled
  `ferrous.managed=true` (defense in depth: the token never becomes "arbitrary
  container control"). `?all=1` on the list endpoint is display/adoption only.
- **Timestamps:** RFC3339, UTC.
- **Transport:** plain HTTP on localhost; set `--tls-cert/--tls-key` for HTTPS across networks.

## Endpoints

| Method | Path | Status | Purpose |
|---|---|---|---|
| GET | `/api/v1/ping` | ✅ phase 1 | liveness: agent version, uptime |
| GET | `/api/v1/system` | ✅ phase 1 | docker version + host info (cpu, mem, containers, images) |
| GET | `/api/v1/servers` | ✅ phase 1 | list managed containers (`?all=1` includes unmanaged) |
| POST | `/api/v1/servers/{id}/power` | ✅ phase 2 | `{action: start\|stop\|restart}`, `?grace=<0..120s>` (default 15, SIGTERM→SIGKILL) |
| GET | `/api/v1/servers/{id}/stats` | ✅ phase 2 | one-shot cpu/mem/net/pids sample |
| GET | `/api/v1/servers/{id}/logs?tail=N&follow=0\|1` | ✅ phase 2 | SSE stream of `docker logs -f` (throttled: 200 lines/s) |
| POST | `/api/v1/servers/{id}/rcon` | ✅ phase 4 | `{cmd, timeout_ms}` → correlated reply (hub held agent-side) |
| GET | `/api/v1/servers/{id}/rcon` | ✅ phase 4 | hub diagnostics `{connected, error, pushes}` (never password/addr) |
| GET | `/api/v1/servers/{id}/files/...` | ▢ 5 | read file, scoped to server data dir |
| PUT | `/api/v1/servers/{id}/files/...` | ▢ 5 | write file (configs, oxide plugins) |
| POST | `/api/v1/servers` | ▢ 5 | deploy: pull image → create volumes/ports/env → start |

## Data conventions (containers)

Managed servers are containers carrying label **`ferrous.managed=true`**.
Phase 5+ will add: `ferrous.datadir` (bind-mounted data path), port contract
(game UDP / rcon TCP), env contract (`SERVER_NAME`, `RCON_PASSWORD`, …).

## Responses

### GET /api/v1/ping
```json
{"ok": true, "version": "dev", "started_at": "2026-10-07T05:20:00Z", "uptime_s": 42, "time": "..."}
```

### GET /api/v1/system
```json
{"docker": {"version": "29.1.3", "api_version": "1.51", "go_version": "go1.27", "os": "linux", "arch": "amd64"},
 "host": {"name": "box", "ncpu": 8, "mem_total_mb": 47000,
          "containers": {"total": 2, "running": 2, "stopped": 0, "paused": 0},
          "images": 3, "storage_driver": "overlay2"},
 "time": "..."}
```

### GET /api/v1/servers
```json
{"count": 1, "filter": "managed",
 "servers": [{"id": "a8d997bd40d6…", "short_id": "a8d997bd40d6", "name": "ferrous-p1-test",
              "image": "ubuntu:24.04", "state": "running", "status": "Up 4 seconds",
              "created": "2026-10-07T05:21:10Z", "managed": true,
              "labels": {"ferrous.managed": "true"},
              "ports": [{"private": 28015, "proto": "udp", "public": 28015, "ip": "0.0.0.0"}]}]}
```
`filter` is `"managed"` (default) or `"all"` (`?all=1`).

### POST /api/v1/servers/{id}/power
```json
{"ok": true, "action": "restart", "id": "ferrous-x", "state": "running", "time": "..."}
```
`state` is a best-effort post-action confirmation (empty string if the follow-up
inspect hiccups). The action itself already succeeded in that case.

> **Signal note for the image phase:** `stop`/`restart` rely on SIGTERM → grace → SIGKILL.
> A process that is PID 1 inside the container and doesn't install a SIGTERM handler
> **ignores SIGTERM** (verified live: `sleep infinity` waited the full grace). The
> `ferrous/rustserver` entrypoint must forward signals (tini or a trapping wrapper)
> so RustDedicated can save before shutdown.

### GET /api/v1/servers/{id}/stats
```json
{"ok": true, "id": "ferrous-x", "state": "running",
 "stats": {"cpu_percent": 100.1, "mem_used_mb": 412.6, "mem_limit_mb": 48040.8,
           "mem_percent": 0.9, "net_rx_mb": 1.2, "net_tx_mb": 3.4, "pids": 12},
 "time": "..."}
```
CPU uses the CLI formula (cpu_delta/system_delta × online_cpus); if the engine's
built-in `precpu` is unusable (single-sample mode) a second sample is taken
~500ms later and the delta is computed agent-side.

### GET /api/v1/servers/{id}/logs (SSE)
```
data: {"kind":"hello","server":"ferrous-x","tail":100,"follow":true,"tty":false,"time":"..."}
data: {"kind":"log","t":"2026-10-07T06:00:00.282502864Z","text":"3000"}
data: {"kind":"log","t":"...","text":"[ferrous] console output too fast — throttling…","throttle":true}
data: {"kind":"log","t":"...","text":"[ferrous] 2801 lines dropped (console throttle)","throttle":true}
data: {"kind":"end","time":"..."}
```
Sequence: one `hello`, zero+ `log`, then a terminal event — `end` (container
stream closed), `error` (stream failure), or nothing (client disconnected).
Throttle: fixed window, 200 lines/s; on strike it notices once, drops, and
summarizes at window rollover or stream end. `tail` is `0..10000` or `all`
(default 100); `follow=0` fetches history and ends.

### POST /api/v1/servers/{id}/rcon

Request: `{"cmd": "status", "timeout_ms": 8000}` — `cmd` ≤ 500 chars,
`timeout_ms` 500..30000 (default 8000).

```json
{"ok": true, "out": "hostname: …\nplayers : 3 (75 max)", "ms": 4}
```

Errors (agent-side contract the panel maps):

| status | when |
|---|---|
| 400 | empty/too-long cmd, bad `timeout_ms`, or container config missing (`ferrous.rcon_port` label absent / `RCON_PASSWORD` env absent) |
| 403 | container not labeled `ferrous.managed=true` |
| 404 | unknown container id |
| 405 | method other than POST/GET |
| 503 | hub not connected / connection lost / container not running — message contains lowercase `rcon` |
| 504 | no reply within `timeout_ms` — on Rust this also means **the command does not exist** (vanilla servers stay silent) |

Config contract: password comes from env `RCON_PASSWORD`; port from label
`ferrous.rcon_port` (label presence = capability flag in `GET /servers`;
empty value → 28016); address is the container IP, re-resolved per reconnect
(root `NetworkSettings.IPAddress`, else first `Networks` entry) and never
published. Handshake: `GET /{password}` (current Rust, not `/websocket/…`).

### GET /api/v1/servers/{id}/rcon

```json
{"connected": true, "error": "", "pushes": 99, "time": "..."}
```

`pushes` counts WebRCON `Identifier:0` messages seen and discarded — console
output is already delivered by the `logs` SSE endpoint, so there is **no
`rcon-push` endpoint by design** (a second stream of the same text would
double the browser's SSE load and race the docker-logs one).
