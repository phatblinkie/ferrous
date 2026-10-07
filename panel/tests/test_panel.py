"""Panel unit tests: auth walls, CSRF, host CRUD, validation.

Run: panel/venv/bin/python -m pytest panel/tests -q
(env must be set BEFORE importing server — handled at module top)."""
import os
import tempfile
import threading
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_TMP = tempfile.mkdtemp(prefix="ferrous-test-")
os.environ["FERROUS_DB"] = os.path.join(_TMP, "test.db")
os.environ["PANEL_SECRET"] = "test-secret"
os.environ["PANEL_ADMIN_USER"] = "admin"
os.environ["PANEL_ADMIN_PASSWORD"] = "testpw-123"

import pytest

import server

CSRF = {"X-Requested-With": "ferrous"}


@pytest.fixture()
def client():
    server.app.config["TESTING"] = True
    return server.app.test_client()


@pytest.fixture()
def authed(client):
    r = client.post("/api/login", json={"username": "admin", "password": "testpw-123"})
    assert r.status_code == 200, r.get_json()
    return client


def test_login_wrong_password(client):
    r = client.post("/api/login", json={"username": "admin", "password": "nope"})
    assert r.status_code == 401
    assert "error" in r.get_json()


def test_login_and_me(client):
    r = client.post("/api/login", json={"username": "admin", "password": "testpw-123"})
    assert r.status_code == 200
    me = client.get("/api/me").get_json()
    assert me["auth"] is True and me["username"] == "admin"
    client.post("/api/logout", headers=CSRF)
    assert client.get("/api/me").get_json()["auth"] is False


def test_walls_unauthenticated(client):
    assert client.get("/api/hosts").status_code == 401
    assert client.get("/api/overview").status_code == 401
    assert client.post("/api/power", json={}).status_code == 401
    assert client.get("/api/logs?host=1&server=x").status_code == 401


def test_csrf_wall(authed):
    r = authed.post("/api/hosts", json={"name": "x", "base_url": "http://h:1", "token": "t"})
    assert r.status_code == 403  # no X-Requested-With header


def test_host_crud_and_token_masking(authed):
    r = authed.post("/api/hosts", headers=CSRF,
                    json={"name": "box-1", "base_url": "http://192.0.2.5:8710/", "token": "secret-token-9999"})
    assert r.status_code == 201, r.get_json()
    hid = r.get_json()["id"]

    lst = authed.get("/api/hosts").get_json()["hosts"]
    assert len(lst) == 1
    h = lst[0]
    assert h["base_url"] == "http://192.0.2.5:8710"  # trailing slash stripped
    assert "secret-token-9999" not in str(h)          # token never leaves the server
    assert h["token_hint"].endswith("9999")

    # duplicate name rejected
    r = authed.post("/api/hosts", headers=CSRF,
                    json={"name": "box-1", "base_url": "http://h2:8710", "token": "t2"})
    assert r.status_code == 400

    # update keeps token when blank
    r = authed.put(f"/api/hosts/{hid}", headers=CSRF, json={"name": "box-1b"})
    assert r.status_code == 200
    h = authed.get("/api/hosts").get_json()["hosts"][0]
    assert h["name"] == "box-1b"
    assert h["token_hint"].endswith("9999")

    assert authed.delete(f"/api/hosts/{hid}", headers=CSRF).status_code == 200
    assert authed.get("/api/hosts").get_json()["hosts"] == []


def test_host_validation(authed):
    bad = [
        {"name": "", "base_url": "http://h:8710", "token": "t"},
        {"name": "n", "base_url": "ftp://h", "token": "t"},
        {"name": "n", "base_url": "not-a-url", "token": "t"},
        {"name": "n", "base_url": "http://user:pw@h:8710", "token": "t"},
        {"name": "n", "base_url": "http://h:8710", "token": ""},
    ]
    for body in bad:
        r = authed.post("/api/hosts", headers=CSRF, json=body)
        assert r.status_code == 400, body


def test_power_validation(authed):
    r = authed.post("/api/power", headers=CSRF,
                    json={"host": 999, "server": "abc", "action": "start"})
    assert r.status_code == 404  # unknown host panel-side
    r = authed.post("/api/power", headers=CSRF,
                    json={"host": 1, "server": "bad id!", "action": "start"})
    assert r.status_code in (400, 404)
    r = authed.post("/api/power", headers=CSRF,
                    json={"host": 1, "server": "ok", "action": "explode"})
    assert r.status_code in (400, 404)


def test_logs_validation(authed):
    assert authed.get("/api/logs?host=999&server=abc").status_code == 404
    r = authed.get("/api/logs?host=1&server=bad%20id")
    assert r.status_code in (400, 404)
    r = authed.get("/api/logs?host=1&server=abc&tail=banana")
    assert r.status_code in (400, 404)


def test_overview_empty(authed):
    data = authed.get("/api/overview").get_json()
    assert data["hosts"] == []


# ------------------------------------------------------------ stub agent (rcon)
# A canned agent answers /api/v1/servers/{id}/rcon so the panel's degradation
# mapping (504→404 InvDump, 503→friendly, ok:false passthrough) is verifiable
# without a live agent.

STUB = {"responses": {}, "last": None}  # cmd -> (status, payload); last body seen


class _StubHandler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # keep pytest output clean
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        STUB["last"] = body
        assert self.headers.get("Authorization", "").startswith("Bearer ")
        code, payload = STUB["responses"].get(
            body.get("cmd"), (200, {"ok": True, "out": "", "ms": 1}))
        raw = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


@pytest.fixture()
def stub_host(authed):
    """(client, host_id) with a registered host pointing at the stub agent."""
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _StubHandler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    STUB["responses"] = {}
    STUB["last"] = None
    r = authed.post("/api/hosts", headers=CSRF, json={
        "name": f"stub-{threading.get_ident()}", "base_url": f"http://127.0.0.1:{srv.server_address[1]}",
        "token": "stub-token"})
    assert r.status_code == 201, r.get_json()
    hid = r.get_json()["id"]
    yield authed, hid
    authed.delete(f"/api/hosts/{hid}", headers=CSRF)  # unique name per run, but tidy anyway
    srv.shutdown()
    srv.server_close()


REAL_STATUS = ("hostname: Stub Rust Server\nversion : 2634 secure\n"
               "players : 2 (64 max) (1 queued) (0 joining)")
PLAYERLIST = json.dumps([{
    "SteamID": "76561198000000001", "DisplayName": "alice", "Health": 100.0,
    "Ping": 20, "Position": {"x": 1, "y": 2, "z": 3}, "ConnectedSeconds": 60,
    "TeamID": "0", "IsMuted": False}])


def test_players_success_and_status_parse(stub_host):
    client, hid = stub_host
    STUB["responses"] = {
        "playerlist verbose": (200, {"ok": True, "out": PLAYERLIST, "ms": 12}),
        "status": (200, {"ok": True, "out": REAL_STATUS, "ms": 9}),
    }
    r = client.get(f"/api/players?host={hid}&server=abc")
    assert r.status_code == 200, r.get_json()
    data = r.get_json()
    assert [p["DisplayName"] for p in data["players"]] == ["alice"]
    st = data["status"]
    assert st["players"] == 2 and st["max"] == 64 and st["queued"] == 1
    assert st["hostname"] == "Stub Rust Server"
    # the panel must pass an explicit reply budget to the agent
    assert STUB["last"]["timeout_ms"] == 6000


def test_players_agent_failure_is_503_with_rcon_message(stub_host):
    client, hid = stub_host
    STUB["responses"] = {"playerlist verbose": (
        503, {"error": "rcon not connected (container not running)"})}
    r = client.get(f"/api/players?host={hid}&server=abc")
    assert r.status_code == 503
    data = r.get_json()
    # UI contract: message contains "rcon" → friendly retry row
    assert "rcon unavailable:" in data["error"]
    assert data["players"] == [] and data["status"] is None


def test_players_validation(authed):
    # no/unknown host → 404 before anything is contacted
    assert authed.get("/api/players?host=999&server=abc").status_code == 404
    assert authed.get("/api/players?server=abc").status_code == 404


def test_players_bad_sid(stub_host):
    client, hid = stub_host
    assert client.get(f"/api/players?host={hid}&server=bad%20id").status_code == 400


def test_inventory_degradation_matrix(stub_host):
    client, hid = stub_host
    sid = "76561198000000001"
    base = f"/api/inventory?host={hid}&server=abc&sid={sid}"

    # vanilla server: agent 504 (no reply) → panel 404 friendly "needs InvDump"
    STUB["responses"] = {"invdump.get " + sid: (
        504, {"error": "no RCON reply for: invdump.get " + sid})}
    r = client.get(base)
    assert r.status_code == 404, r.get_json()
    assert "uMod/InvDump" in r.get_json()["error"]
    assert STUB["last"]["timeout_ms"] == 3000

    # plugin present: JSON payload passes through
    inv = {"player": {"belt": [{"name": "Rock", "amount": 1}], "main": []},
           "updated": "2026-10-07T00:00:00Z"}
    STUB["responses"] = {"invdump.get " + sid: (
        200, {"ok": True, "out": json.dumps(inv), "ms": 40})}
    r = client.get(base)
    assert r.status_code == 200, r.get_json()
    assert r.get_json()["player"]["belt"][0]["name"] == "Rock"

    # connection lost → 503 with the agent's reason
    STUB["responses"] = {"invdump.get " + sid: (
        503, {"error": "rcon not connected (server restarting)"})}
    r = client.get(base)
    assert r.status_code == 503
    assert "rcon not connected" in r.get_json()["error"]

    # unparseable reply → 502
    STUB["responses"] = {"invdump.get " + sid: (
        200, {"ok": True, "out": "<html>oops</html>", "ms": 1})}
    r = client.get(base)
    assert r.status_code == 502

    # sid validation
    assert client.get(f"/api/inventory?host={hid}&server=abc&sid=abc").status_code == 400
    assert client.get(f"/api/inventory?host={hid}&server=abc&sid=123").status_code == 400


def test_console_command_contract(stub_host):
    client, hid = stub_host

    # success → 200 ok:true out/ms
    STUB["responses"] = {"say hello": (200, {"ok": True, "out": "Say: hello", "ms": 7})}
    r = client.post("/api/rcon", headers=CSRF,
                    json={"host": hid, "server": "abc", "cmd": "say hello"})
    assert r.status_code == 200
    assert r.get_json() == {"ok": True, "out": "Say: hello", "ms": 7}

    # command-level agent failure → 200 ok:false (UI renders inline)
    STUB["responses"] = {"totally.unknown": (
        504, {"error": "no RCON reply for: totally.unknown", "ms": 600})}
    r = client.post("/api/rcon", headers=CSRF,
                    json={"host": hid, "server": "abc", "cmd": "totally.unknown"})
    assert r.status_code == 200
    body = r.get_json()
    assert body["ok"] is False and "no RCON reply" in body["error"]

    # not connected → 200 ok:false too (console shows the reason inline)
    STUB["responses"] = {"status": (
        503, {"error": "rcon not connected (container not running)", "ms": 1})}
    r = client.post("/api/rcon", headers=CSRF,
                    json={"host": hid, "server": "abc", "cmd": "status"})
    assert r.status_code == 200
    assert r.get_json()["ok"] is False

    # validation
    r = client.post("/api/rcon", headers=CSRF,
                    json={"host": hid, "server": "abc", "cmd": "   "})
    assert r.status_code == 400
    r = client.post("/api/rcon", headers=CSRF,
                    json={"host": hid, "server": "abc", "cmd": "x" * 501})
    assert r.status_code == 400
    r = client.post("/api/rcon", headers=CSRF,
                    json={"host": 999, "server": "abc", "cmd": "status"})
    assert r.status_code == 404
    # CSRF wall
    r = client.post("/api/rcon",
                    json={"host": hid, "server": "abc", "cmd": "status"})
    assert r.status_code == 403


def test_rcon_walls_unauthenticated(client):
    assert client.get("/api/players?host=1&server=x").status_code == 401
    assert client.get("/api/inventory?host=1&server=x&sid=76561198000000001").status_code == 401
    assert client.post("/api/rcon", json={"host": 1, "server": "x", "cmd": "status"}).status_code == 401
