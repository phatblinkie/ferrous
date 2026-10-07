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
| POST | `/api/v1/servers/{id}/power` | ✅ phase 2 | `{action: start\|stop\|restart}`, `?grace=<0..600s>` (default 180, SIGINT→SIGKILL) |
| GET | `/api/v1/servers/{id}/stats` | ✅ phase 2 | one-shot cpu/mem/net/pids sample |
| GET | `/api/v1/servers/{id}/logs?tail=N&follow=0\|1` | ✅ phase 2 | SSE stream of `docker logs -f` (throttled: 200 lines/s) |
| POST | `/api/v1/servers/{id}/rcon` | ✅ phase 4 | `{cmd, timeout_ms}` → correlated reply (hub held agent-side) |
| GET | `/api/v1/servers/{id}/rcon` | ✅ phase 4 | hub diagnostics `{connected, error, pushes}` (never password/addr) |
| GET | `/api/v1/servers/{id}/files?path=` | ✅ phase 5 | dir listing or file content under the container's data root |
| PUT | `/api/v1/servers/{id}/files` | ✅ phase 5 | `{path, content, encoding}` → atomic write |
| POST | `/api/v1/servers` | ✅ phase 5 | deploy: pull image → create (labels/bind/restart) → start |

## Data conventions (containers)

Managed servers are containers carrying label **`ferrous.managed=true`**.
Resolved contracts:

- **data root (phase 5):** `ferrous.datadir` (absolute host path, set by
  deploy) else the container's single writable mount. Files API paths are
  relative to it; `..`/absolute/symlink escapes are rejected.
- **rcon (phase 4):** password = env `RCON_PASSWORD`; port = label
  `ferrous.rcon_port` (label presence = capability flag in `/servers`);
  address = container IP, never published.
- phase 6 image will add the port contract (game UDP / rcon TCP publish
  rules) and its env contract (`SERVER_NAME`, …).

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

> **Signal note (phase 6, verified):** `stop`/`restart` send the container's
> `STOPSIGNAL` (SIGINT for `ferrous/rustserver`, proven `KillSignal=SIGINT`) →
> grace (default 180 s = proven `TimeoutStopSec`) → SIGKILL. The entrypoint
> **`exec`s RustDedicated**, so the game is PID 1 and receives the signal
> directly — no wrapper that could drop it. (A process that is PID 1 with no
> handler ignores SIGTERM — verified live: `sleep infinity` waited the full
> grace — which is why `STOPSIGNAL SIGINT` matters.) `docker stop` returns as
> soon as the process exits, so the 180 s default never slows fast-exiting
> containers.

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

### GET /api/v1/servers/{id}/files?path=…

The root is the container's data directory (`ferrous.datadir` label, else its
single writable mount — ambiguity without the label is a 400). Paths are
relative to that root; absolute inputs, `..` and symlinks leaving the root are
rejected (TOCTOU aside: the token is already docker-socket equivalent).

Directory:
```json
{"path": "oxide/plugins", "type": "dir",
 "entries": [{"name": "InvDump.dll", "dir": false, "size": 48210, "mtime": "2026-10-07T00:00:00Z"}]}
```
File (text as `utf8`, anything with NUL/invalid-UTF-8 as `base64`):
```json
{"path": "server/main/server.cfg", "type": "file", "size": 17,
 "mtime": "...", "encoding": "utf8", "content": "server.name \"x\"\n"}
```

### PUT /api/v1/servers/{id}/files
```json
{"path": "oxide/plugins/New.dll", "content": "TVqQAAMAAAA…", "encoding": "base64"}
```
→ `{"ok": true, "path": "...", "size": 123, "mtime": "..."}`. Writes are atomic
(temp + rename), parent dirs are created, max 8 MB.

Statuses: 400 bad path/encoding/body · 403 unmanaged · 404 unknown container
or missing path/root · 405 · 413 too large · 500 unclassified IO · 502 engine.

### POST /api/v1/servers (deploy)
```json
{"name": "rust-main", "image": "ferrous/rustserver:latest",
 "data_dir": "/srv/ferrous/rust-main", "container_path": "/server",
 "env": {"RCON_PASSWORD": "...", "SERVER_NAME": "..."},
 "ports": [{"container": 28015, "host": 28015, "proto": "udp"}],
 "rcon_port": 28016, "memory_mb": 8192,
 "command": ["..."], "labels": {"ferrous.note": "..."}}
```
Agent behavior: mkdir `data_dir` → image already local? else pull (anonymous,
streamed; an `{"error":…}` stream line fails it) → create → start. The agent
adds `ferrous.managed=true`, `ferrous.datadir`, `ferrous.rcon_port`, a data
bind at `container_path`, and `unless-stopped`; reserved labels cannot be
overridden by `labels`. A failed start **keeps the container** (visible in
`/servers`) and names its id in the error.

→ 201 `{"ok": true, "res": {"id", "name", "image", "pulled", "started"}}`

Statuses: 400 validation / pull failure / engine 4xx / mkdir · 405 · 409 name
conflict · 413 body · 502 engine unreachable or start failure · 504 >15 min.

## `ferrous/rustserver` image contract (phase 6)

`make image` builds `ferrous/rustserver:latest` from `image/` — a dockerized
port of the proven local setup (`start.sh` + `steamcmd/update.sh` +
`auto-update.sh` + `oxide-install.sh`, all referenced from this repo's
`image/*.sh`; `InvDump.cs` is the proven plugin, verbatim).

**Lifecycle:** the image is small (steamcmd + helpers). First boot installs
Steam app 258550 **into the data dir** (`SERVER_DIR`, default `/server` — the
`container_path` deploy binds). Later boots run `app_update` unless
`AUTO_UPDATE=false`; a changed Steam `buildid` forces an Oxide re-apply.
Oxide (uMod) comes from the latest `OxideMod/Oxide.Rust` GitHub release;
`OXIDE=false` runs vanilla (panel inventory then degrades to a friendly 404).

**Env contract** (all optional except the password; world params only apply to
a fresh map — the identity's save wins afterwards):

| env | default | meaning |
|---|---|---|
| `RCON_PASSWORD` | *(required)* | entrypoint refuses to boot without it |
| `SERVER_NAME` | — | startup `+server.hostname` (a `server/<identity>/cfg/server.cfg` on disk overrides it — file is read after the command line) |
| `SERVER_IDENTITY` | `main` | save folder `server/<identity>/` |
| `SERVER_LEVEL` / `SERVER_SEED` / `SERVER_WORLDSIZE` / `SERVER_MAXPLAYERS` | `Procedural Map` / `20261007` / `3500` / `50` | proven start.sh defaults |
| `AUTO_UPDATE` | `true` | steamcmd update on boot; `false` pins the build |
| `OXIDE` | `true` | apply Oxide + seed `oxide/plugins/InvDump.cs`; `false` = vanilla |

**Ports:** `28015/udp` game and `28016/udp` query are published by the deploy
wizard; `28016/tcp` RCON is **never published** — it binds `0.0.0.0` *inside*
the container (proven host setup bound `127.0.0.1`; in a container the agent
must dial the container IP) and only the agent reaches it via the
`ferrous.rcon_port` label.

**Files tab paths:** server files sit at the data-dir root (`RustDedicated`,
`oxide/`, `server/<identity>/cfg/server.cfg` — note `cfg/server.cfg`, proven
gotcha: `server/<identity>/server.cfg` is ignored). Oxide updates, Steam
buildids and original-assembly backups live under `.ferrous/oxide-cache/`.

**Force-wipes** (first Thursday monthly) need a world reset: stop, delete the
identity's save files (`server/<identity>/*.map` + save dbs) or change
`SERVER_SEED`, start — same as the proven runbook.
