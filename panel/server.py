"""ferrous panel — central web control plane for dockerized game servers.

- single admin login (DB-backed; first-boot password bootstrap)
- host registry: agent base_url + bearer token, stored in SQLite
- overview / power / stats proxied to agents (tokens never reach the browser)
- live console: agent SSE bytes forwarded to the browser
- rcon: players / inventory / console commands proxied through agent hubs
  (504 = no reply → InvDump degradation, 503 = not connected → friendly retry)
- CSRF: non-GET /api/* requires the X-Requested-With: ferrous header

Config (env): FERROUS_DB, PANEL_SECRET, PANEL_ADMIN_USER, PANEL_ADMIN_PASSWORD,
PANEL_BIND, PANEL_PORT.
"""
import datetime
import json
import os
import re
import secrets
import time
import urllib.parse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager

from flask import (Flask, Response, jsonify, render_template, request,
                   session, stream_with_context)
from werkzeug.security import check_password_hash, generate_password_hash

import agents

DB_PATH = os.environ.get("FERROUS_DB", "ferrous.db")
BIND = os.environ.get("PANEL_BIND", "0.0.0.0")
PORT = int(os.environ.get("PANEL_PORT", "8122"))

# mirrors the agent's id rule
VALID_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
CSRF_HEADER = "ferrous"

app = Flask(__name__)
app.config.update(
    SESSION_COOKIE_HTTPONLY=True,
    SESSION_COOKIE_SAMESITE="Lax",
    PERMANENT_SESSION_LIFETIME=datetime.timedelta(days=30),
)

# ------------------------------------------------------------------ db / boot
@contextmanager
def dbh():
    conn = sqlite3_connect()
    try:
        yield conn
        conn.commit()
    finally:
        conn.close()


def sqlite3_connect():
    import sqlite3
    conn = sqlite3.connect(DB_PATH, timeout=10)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    return conn


def now_iso():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def load_secret():
    if env := os.environ.get("PANEL_SECRET"):
        return env
    path = DB_PATH + ".secret"
    try:
        with open(path) as f:
            s = f.read().strip()
            if s:
                return s
    except FileNotFoundError:
        pass
    s = secrets.token_hex(32)
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(s)
        return s
    except FileExistsError:  # another worker won the race
        with open(path) as f:
            return f.read().strip()


def init_db():
    d = os.path.dirname(os.path.abspath(DB_PATH))
    os.makedirs(d, exist_ok=True)
    with dbh() as c:
        c.executescript("""
        CREATE TABLE IF NOT EXISTS hosts (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          name TEXT NOT NULL UNIQUE,
          base_url TEXT NOT NULL,
          token TEXT NOT NULL,
          enabled INTEGER NOT NULL DEFAULT 1,
          created_at TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT NOT NULL UNIQUE,
          password_hash TEXT NOT NULL,
          created_at TEXT NOT NULL
        );
        """)
    bootstrap_admin()


def bootstrap_admin():
    with dbh() as c:
        if c.execute("SELECT id FROM users LIMIT 1").fetchone():
            return
        user = os.environ.get("PANEL_ADMIN_USER", "admin")
        pw = os.environ.get("PANEL_ADMIN_PASSWORD", "")
        generated = not pw
        if generated:
            pw = secrets.token_urlsafe(12)
        # race-safe: two gunicorn workers may bootstrap concurrently on a fresh
        # volume — only the winner reports it (INSERT OR IGNORE + rowcount)
        cur = c.execute("INSERT OR IGNORE INTO users(username, password_hash, created_at) VALUES (?,?,?)",
                        (user, generate_password_hash(pw), now_iso()))
        created = cur.rowcount == 1
    if not created:
        return
    if generated:
        print(f"[boot] first boot — admin '{user}' password: {pw}  (set PANEL_ADMIN_PASSWORD to pick your own)",
              flush=True)
    else:
        print(f"[boot] admin user '{user}' created from PANEL_ADMIN_PASSWORD", flush=True)


init_db()
app.secret_key = load_secret()

# --------------------------------------------------------------------- helpers
def list_hosts():
    with dbh() as c:
        return [dict(r) for r in c.execute("SELECT * FROM hosts ORDER BY id")]


def get_host(hid):
    try:
        hid = int(hid)
    except (TypeError, ValueError):
        return None
    with dbh() as c:
        row = c.execute("SELECT * FROM hosts WHERE id=?", (hid,)).fetchone()
        return dict(row) if row else None


def valid_base_url(u):
    p = urllib.parse.urlparse(u)
    return (p.scheme in ("http", "https") and bool(p.hostname)
            and p.path in ("", "/") and not p.username and not p.netloc.count("@"))


def agent_status(e):
    """Agent 401 = broken host config, never the panel's own auth — remap so the
    browser's api() helper doesn't pop the login overlay on a bad agent token."""
    return 502 if e.status in (0, 401) else e.status


def resolve_host():
    """-> (host_row, None) or (None, error_response)"""
    h = get_host(request.args.get("host")
                  or (request.get_json(silent=True) or {}).get("host"))
    if h is None:
        return None, (jsonify({"error": "unknown host"}), 404)
    return h, None


# --------------------------------------------------------------------- routes
@app.before_request
def guard():
    if request.path.startswith("/api/") and request.path not in ("/api/login", "/api/me"):
        if not session.get("user"):
            return jsonify({"error": "unauthorized"}), 401
        if request.method != "GET" and request.headers.get("X-Requested-With") != CSRF_HEADER:
            return jsonify({"error": "missing header"}), 403
    return None


_login_fail = {}  # ip -> (fails, lock_until)


@app.post("/api/login")
def api_login():
    ip = request.remote_addr or "?"
    fails, lock_until = _login_fail.get(ip, (0, 0))
    if time.time() < lock_until:
        return jsonify({"error": f"too many attempts, wait {int(lock_until - time.time())}s"}), 429
    body = request.get_json(silent=True) or {}
    user = str(body.get("username", ""))
    pw = str(body.get("password", ""))
    with dbh() as c:
        row = c.execute("SELECT * FROM users WHERE username=?", (user,)).fetchone()
    if row and check_password_hash(row["password_hash"], pw):
        _login_fail.pop(ip, None)
        session["user"] = row["username"]
        session.permanent = True
        return jsonify({"ok": True})
    fails += 1
    _login_fail[ip] = (fails, time.time() + 60 if fails >= 5 else 0)
    return jsonify({"error": "wrong username or password"}), 401


@app.post("/api/logout")
def api_logout():
    session.clear()
    return jsonify({"ok": True})


@app.get("/api/me")
def api_me():
    return jsonify({"auth": bool(session.get("user")), "username": session.get("user")})


@app.get("/")
def index():
    return render_template("index.html")


# --------------------------------------------------------------------- hosts
@app.get("/api/hosts")
def api_hosts():
    out = []
    for h in list_hosts():
        tok = h["token"]
        out.append({"id": h["id"], "name": h["name"], "base_url": h["base_url"],
                    "enabled": bool(h["enabled"]), "created_at": h["created_at"],
                    "token_hint": "••••" + tok[-4:] if len(tok) >= 4 else "••••"})
    return jsonify({"hosts": out})


@app.post("/api/hosts")
def api_host_create():
    b = request.get_json(silent=True) or {}
    name = str(b.get("name", "")).strip()
    base = str(b.get("base_url", "")).strip().rstrip("/")
    token = str(b.get("token", "")).strip()
    if not (1 <= len(name) <= 40):
        return jsonify({"error": "name must be 1-40 chars"}), 400
    if not valid_base_url(base):
        return jsonify({"error": "base_url must be http(s)://host:port"}), 400
    if not (1 <= len(token) <= 1024):
        return jsonify({"error": "token required"}), 400
    try:
        with dbh() as c:
            cur = c.execute("INSERT INTO hosts(name, base_url, token, created_at) VALUES (?,?,?,?)",
                            (name, base, token, now_iso()))
    except Exception:
        return jsonify({"error": f"host name '{name}' already exists"}), 400
    return jsonify({"ok": True, "id": cur.lastrowid}), 201


@app.put("/api/hosts/<int:hid>")
def api_host_update(hid):
    h = get_host(hid)
    if h is None:
        return jsonify({"error": "unknown host"}), 404
    b = request.get_json(silent=True) or {}
    fields, vals = [], []
    if "name" in b:
        name = str(b["name"]).strip()
        if not (1 <= len(name) <= 40):
            return jsonify({"error": "name must be 1-40 chars"}), 400
        fields.append("name=?"); vals.append(name)
    if "base_url" in b:
        base = str(b["base_url"]).strip().rstrip("/")
        if not valid_base_url(base):
            return jsonify({"error": "base_url must be http(s)://host:port"}), 400
        fields.append("base_url=?"); vals.append(base)
    if "token" in b and str(b["token"]).strip():
        fields.append("token=?"); vals.append(str(b["token"]).strip())
    if "enabled" in b:
        fields.append("enabled=?"); vals.append(1 if b["enabled"] else 0)
    if not fields:
        return jsonify({"error": "nothing to update"}), 400
    vals.append(hid)
    try:
        with dbh() as c:
            c.execute(f"UPDATE hosts SET {', '.join(fields)} WHERE id=?", vals)
    except Exception:
        return jsonify({"error": "host name already exists"}), 400
    return jsonify({"ok": True})


@app.delete("/api/hosts/<int:hid>")
def api_host_delete(hid):
    if get_host(hid) is None:
        return jsonify({"error": "unknown host"}), 404
    with dbh() as c:
        c.execute("DELETE FROM hosts WHERE id=?", (hid,))
    return jsonify({"ok": True})


@app.post("/api/hosts/test")
def api_host_test():
    b = request.get_json(silent=True) or {}
    if b.get("id") is not None:
        h = get_host(b["id"])
        if h is None:
            return jsonify({"error": "unknown host"}), 404
        base, token = h["base_url"], h["token"]
    else:
        base = str(b.get("base_url", "")).strip().rstrip("/")
        token = str(b.get("token", "")).strip()
        if not valid_base_url(base) or not token:
            return jsonify({"error": "base_url and token required"}), 400
    try:
        p = agents.ping(base, token, timeout=5)
        s = agents.system(base, token, timeout=5)
    except agents.AgentError as e:
        return jsonify({"ok": False, "error": e.message}), agent_status(e)
    return jsonify({"ok": True, "ping": p, "system": s})


# ------------------------------------------------------------------ aggregate
@app.get("/api/overview")
def api_overview():
    hosts = list_hosts()

    def probe(h):
        base = {"id": h["id"], "name": h["name"], "base_url": h["base_url"],
                "enabled": bool(h["enabled"])}
        if not h["enabled"]:
            return {**base, "ok": None, "error": None, "servers": []}
        try:
            srv = agents.servers(h["base_url"], h["token"], timeout=6)
            return {**base, "ok": True, "error": None,
                    "servers": srv.get("servers", [])}
        except agents.AgentError as e:
            return {**base, "ok": False, "error": e.message, "servers": []}

    with ThreadPoolExecutor(max_workers=8) as pool:
        out = list(pool.map(probe, hosts))
    return jsonify({"hosts": out, "time": now_iso()})


# ---------------------------------------------------------------------- proxy
@app.post("/api/power")
def api_power():
    h, resp = resolve_host()
    if resp:
        return resp
    b = request.get_json(silent=True) or {}
    sid = str(b.get("server", ""))
    action = b.get("action")
    if not VALID_ID.match(sid):
        return jsonify({"error": "bad server id"}), 400
    if action not in ("start", "stop", "restart"):
        return jsonify({"error": "bad action"}), 400
    grace = b.get("grace")
    if grace is not None and (not isinstance(grace, int) or not 0 <= grace <= 600):
        return jsonify({"error": "grace must be 0..600"}), 400
    try:
        data = agents.power(h["base_url"], h["token"], sid, action, grace)
    except agents.AgentError as e:
        return jsonify({"error": e.message}), agent_status(e)
    return jsonify(data)


@app.get("/api/stats")
def api_stats():
    h, resp = resolve_host()
    if resp:
        return resp
    sid = request.args.get("server", "")
    if not VALID_ID.match(sid):
        return jsonify({"error": "bad server id"}), 400
    try:
        return jsonify(agents.stats(h["base_url"], h["token"], sid))
    except agents.AgentError as e:
        return jsonify({"error": e.message}), agent_status(e)


@app.get("/api/logs")
def api_logs():
    """SSE proxy: browser ← panel ← agent. Agent bytes (events + hb comments)
    are forwarded untouched."""
    h, resp = resolve_host()
    if resp:
        return resp
    sid = request.args.get("server", "")
    if not VALID_ID.match(sid):
        return jsonify({"error": "bad server id"}), 400
    tail = request.args.get("tail", "200")
    if tail != "all":
        if not tail.isdigit() or not 0 <= int(tail) <= 10000:
            return jsonify({"error": "tail must be 0..10000 or 'all'"}), 400
    follow = request.args.get("follow", "1") != "0"

    def gen():
        try:
            yield from agents.stream_lines(h["base_url"], h["token"], sid,
                                            tail, follow, timeout=60)
        except agents.AgentError as e:
            yield f"data: {json.dumps({'kind': 'error', 'error': e.message})}\n\n"
        except (TimeoutError, OSError) as e:
            yield f"data: {json.dumps({'kind': 'error', 'error': f'agent connection lost: {e}'})}\n\n"

    return Response(stream_with_context(gen()), mimetype="text/event-stream",
                    headers={"Cache-Control": "no-cache",
                             "X-Accel-Buffering": "no",
                             "Connection": "keep-alive"})


# ------------------------------------------------------------------------ rcon
def parse_status(text):
    """port of the proven panel's `status` parser (players/max/queued/joining,
    hostname — best effort, raw kept for display)."""
    m = re.search(r"players\s*:\s*(\d+)\s*\((\d+) max\)(?:\s*\((\d+) queued\))?(?:\s*\((\d+) joining\))?",
                  text or "")
    out = {"players": None, "max": None, "queued": None, "joining": None, "raw": text or ""}
    if m:
        out.update(players=int(m.group(1)), max=int(m.group(2)),
                   queued=int(m.group(3) or 0), joining=int(m.group(4) or 0))
    h = re.search(r"hostname:\s*(.+)", text or "")
    if h:
        out["hostname"] = h.group(1).strip()
    return out


@app.get("/api/players")
def api_players():
    """playerlist verbose + status through the agent. Any failure answers 503
    with a message containing 'rcon' — the UI turns that into its friendly
    'server offline or restarting — retrying automatically…' row."""
    h, resp = resolve_host()
    if resp:
        return resp
    sid = request.args.get("server", "")
    if not VALID_ID.match(sid):
        return jsonify({"error": "bad server id"}), 400
    try:
        data = agents.rcon(h["base_url"], h["token"], sid,
                           "playerlist verbose", timeout_ms=6000, timeout=12)
    except agents.AgentError as e:
        return jsonify({"error": f"rcon unavailable: {e.message}",
                        "players": [], "status": None}), 503
    try:
        players = json.loads(data.get("out") or "") if (data.get("out") or "").strip() else []
    except ValueError:
        players = []
    st = None
    try:
        sd = agents.rcon(h["base_url"], h["token"], sid, "status",
                         timeout_ms=6000, timeout=12)
        st = parse_status(sd.get("out", ""))
    except Exception:
        pass  # status is decoration: players still render without it
    return jsonify({"players": players, "status": st})


@app.get("/api/inventory")
def api_inventory():
    """On-demand InvDump lookup. Degradation contract (ported verbatim):
    agent 504 (no reply) → 404 'needs uMod/InvDump'; connection trouble →
    503; unparseable reply → 502."""
    sid = request.args.get("sid", "")
    if not sid.isdigit() or len(sid) != 17:
        return jsonify({"error": "sid required"}), 400
    h, resp = resolve_host()
    if resp:
        return resp
    srv = request.args.get("server", "")
    if not VALID_ID.match(srv):
        return jsonify({"error": "bad server id"}), 400
    try:
        data = agents.rcon(h["base_url"], h["token"], srv, f"invdump.get {sid}",
                           timeout_ms=3000, timeout=10)
    except agents.AgentError as e:
        if e.status == 504:
            # command exists but no reply: vanilla server (no uMod) or still booting
            return jsonify({"error": "inventory unavailable — needs uMod/InvDump (or server still booting)"}), 404
        if e.status in (0, 502, 503):
            return jsonify({"error": e.message or "rcon unavailable — server restarting?"}), 503
        return jsonify({"error": e.message}), e.status  # config problems pass through
    raw = data.get("out") or ""
    try:
        parsed = json.loads(raw)
    except ValueError:
        return jsonify({"error": "bad reply from InvDump: " + raw[:80]}), 502
    return jsonify({"sid": sid,
                    "player": parsed.get("player") if isinstance(parsed, dict) else None})


@app.post("/api/rcon")
def api_rcon():
    """Console command. Success → 200 {ok,out,ms}; ANY agent-side failure →
    200 {ok:false,error,ms} so the console/drawer can render it inline (the
    proven UI's contract). Host-level problems keep their own status."""
    h, resp = resolve_host()
    if resp:
        return resp
    b = request.get_json(silent=True) or {}
    sid = str(b.get("server", ""))
    cmd = str(b.get("cmd", "")).strip()
    if not VALID_ID.match(sid):
        return jsonify({"error": "bad server id"}), 400
    if not cmd:
        return jsonify({"error": "empty"}), 400
    if len(cmd) > 500:
        return jsonify({"error": "too long"}), 400
    t0 = time.time()
    try:
        data = agents.rcon(h["base_url"], h["token"], sid, cmd,
                           timeout_ms=10000, timeout=20)
        return jsonify({"ok": True, "out": data.get("out", ""),
                        "ms": data.get("ms") or int((time.time() - t0) * 1000)})
    except agents.AgentError as e:
        return jsonify({"ok": False, "error": e.message,
                        "ms": int((time.time() - t0) * 1000)})


# ---------------------------------------------------------------------- files
@app.get("/api/files")
def api_files_list():
    """List/read inside a container's data directory (agent-scoped root).
    GET on a dir → {type:"dir", entries}; on a file → content (utf8/base64)."""
    h, resp = resolve_host()
    if resp:
        return resp
    srv = request.args.get("server", "")
    if not VALID_ID.match(srv):
        return jsonify({"error": "bad server id"}), 400
    path = request.args.get("path", "")
    if path.startswith("/") or "\x00" in path:
        return jsonify({"error": "path must be relative to the data directory"}), 400
    try:
        return jsonify(agents.files_get(h["base_url"], h["token"], srv, path))
    except agents.AgentError as e:
        return jsonify({"error": e.message}), agent_status(e)


@app.put("/api/files")
def api_files_write():
    """Write a file: {"server","path","content","encoding"} → atomic PUT."""
    h, resp = resolve_host()
    if resp:
        return resp
    b = request.get_json(silent=True) or {}
    srv = str(b.get("server", ""))
    path = str(b.get("path", ""))
    content = b.get("content")
    encoding = str(b.get("encoding", "utf8"))
    if not VALID_ID.match(srv):
        return jsonify({"error": "bad server id"}), 400
    if not path or path.startswith("/") or "\x00" in path:
        return jsonify({"error": "path must be a non-empty relative path"}), 400
    if not isinstance(content, str):
        return jsonify({"error": "content must be a string"}), 400
    if encoding not in ("utf8", "base64"):
        return jsonify({"error": "encoding must be utf8 or base64"}), 400
    try:
        return jsonify(agents.files_put(h["base_url"], h["token"], srv, path,
                                        content, encoding))
    except agents.AgentError as e:
        return jsonify({"error": e.message}), agent_status(e)


# --------------------------------------------------------------------- deploy
@app.post("/api/deploy")
def api_deploy():
    """Deployment wizard backend: proxy the spec to the agent, which pulls,
    creates and starts. The agent validates authoritatively — this check only
    gives the form a fast, friendly error for the obvious omissions."""
    h, resp = resolve_host()
    if resp:
        return resp
    b = request.get_json(silent=True) or {}
    missing = [k for k in ("name", "image", "data_dir") if not str(b.get(k, "")).strip()]
    if missing:
        return jsonify({"error": "missing required field(s): " + ", ".join(missing)}), 400
    if not str(b["data_dir"]).startswith("/"):
        return jsonify({"error": "data_dir must be an absolute path"}), 400
    spec = {k: v for k, v in b.items() if k != "host"}  # agent validates the rest
    try:
        return jsonify(agents.deploy(h["base_url"], h["token"], spec)), 201
    except agents.AgentError as e:
        # 409 name conflict / 400 validation / 502 engine / 504 timeout pass
        # through with the agent's message so the wizard can show it
        return jsonify({"error": e.message}), agent_status(e)


if __name__ == "__main__":
    app.run(host=BIND, port=PORT, threaded=True, debug=False)
