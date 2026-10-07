# ferrous agent API — v1

All endpoints are served by `ferrous-agent` on the listen address. Version prefix: `/api/v1`.

## Conventions

- **Auth:** every request needs `Authorization: Bearer <token>` (agent config). Missing/wrong
  token → `401 {"error":"unauthorized"}` (constant-time compare; failures logged with IP).
- **Errors:** JSON body `{"error": "human-readable"}`.
  - `401` bad/missing token · `404` unknown path · `405` wrong method
  - `502` docker layer failure (daemon down / API error) — panel shows it as a friendly
    host-level error, never a crash.
- **Timestamps:** RFC3339, UTC.
- **Transport:** plain HTTP on localhost; set `--tls-cert/--tls-key` for HTTPS across networks.

## Endpoints

| Method | Path | Status | Purpose |
|---|---|---|---|
| GET | `/api/v1/ping` | ✅ phase 1 | liveness: agent version, uptime |
| GET | `/api/v1/system` | ✅ phase 1 | docker version + host info (cpu, mem, containers, images) |
| GET | `/api/v1/servers` | ✅ phase 1 | list managed containers (`?all=1` includes unmanaged) |
| POST | `/api/v1/servers/{id}/power` | ▢ 2 | `{action: start\|stop\|restart}` |
| GET | `/api/v1/servers/{id}/stats` | ▢ 2 | live cpu/mem/net (docker stats) |
| GET | `/api/v1/servers/{id}/logs?tail=N` | ▢ 2 | SSE stream of `docker logs -f` (throttled) |
| POST | `/api/v1/servers/{id}/rcon` | ▢ 4 | `{cmd, timeout}` → correlated reply (hub held agent-side) |
| GET | `/api/v1/servers/{id}/rcon-push` | ▢ 4 | SSE of WebRCON Identifier:0 push lines |
| GET | `/api/v1/servers/{id}/files/...` | ▢ 5 | read file, scoped to server data dir |
| PUT | `/api/v1/servers/{id}/files/...` | ▢ 5 | write file (configs, oxide plugins) |
| POST | `/api/v1/servers` | ▢ 5 | deploy: pull image → create volumes/ports/env → start |

## Data conventions (containers)

Managed servers are containers carrying label **`ferrous.managed=true`**.
Phase 5+ will add: `ferrous.datadir` (bind-mounted data path), port contract
(game UDP / rcon TCP), env contract (`SERVER_NAME`, `RCON_PASSWORD`, …).

## Responses (phase 1)

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
