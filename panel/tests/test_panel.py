"""Panel unit tests: auth walls, CSRF, host CRUD, validation.

Run: panel/venv/bin/python -m pytest panel/tests -q
(env must be set BEFORE importing server — handled at module top)."""
import os
import tempfile

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
