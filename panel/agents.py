"""HTTP client for ferrous agents — stdlib urllib only (no extra deps).

Every call carries the host's Bearer token. Failures raise AgentError with
the agent's own status code (0 == unreachable) so routes can proxy the
semantics through (404 unknown container, 403 not managed, 502 docker down).
"""
import json
import urllib.error
import urllib.parse
import urllib.request


class AgentError(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status  # agent's HTTP status, 0 = unreachable
        self.message = message


def _http_error(e):
    try:
        msg = json.loads(e.read().decode() or "{}").get("error")
    except Exception:
        msg = None
    return AgentError(e.code, str(msg or e.reason))


def _request(base, token, path, method="GET", body=None, timeout=8):
    url = base.rstrip("/") + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers={
        "Authorization": "Bearer " + token,
        "Content-Type": "application/json",
    })
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as e:          # must precede URLError (subclass)
        raise _http_error(e) from None
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        raise AgentError(0, f"agent unreachable: {getattr(e, 'reason', e)}") from None
    try:
        return json.loads(raw or b"{}")
    except ValueError:
        raise AgentError(502, "bad response from agent") from None


def ping(base, token, timeout=5):
    return _request(base, token, "/api/v1/ping", timeout=timeout)


def system(base, token, timeout=5):
    return _request(base, token, "/api/v1/system", timeout=timeout)


def servers(base, token, all_=False, timeout=6):
    path = "/api/v1/servers" + ("?all=1" if all_ else "")
    return _request(base, token, path, timeout=timeout)


def power(base, token, sid, action, grace=None, timeout=90):
    path = f"/api/v1/servers/{urllib.parse.quote(sid)}/power"
    body = {"action": action}
    if grace is not None:
        path += f"?grace={int(grace)}"
    return _request(base, token, path, method="POST", body=body, timeout=timeout)


def stats(base, token, sid, timeout=12):
    return _request(base, token, f"/api/v1/servers/{urllib.parse.quote(sid)}/stats",
                    timeout=timeout)


def rcon(base, token, sid, cmd, timeout_ms=None, timeout=15):
    """Run an RCON command through the agent.

    timeout_ms is the agent's server-side reply budget (it answers 504 after
    it — on Rust that also means "command does not exist"); `timeout` is this
    panel→agent socket budget and must exceed it.
    """
    path = f"/api/v1/servers/{urllib.parse.quote(sid)}/rcon"
    body = {"cmd": cmd}
    if timeout_ms is not None:
        body["timeout_ms"] = int(timeout_ms)
    return _request(base, token, path, method="POST", body=body, timeout=timeout)


def files_get(base, token, sid, path="", timeout=12):
    """List a directory or read a file from the container's data root."""
    q = urllib.parse.urlencode({"path": path})
    return _request(base, token,
                    f"/api/v1/servers/{urllib.parse.quote(sid)}/files?{q}",
                    timeout=timeout)


def files_put(base, token, sid, path, content, encoding="utf8", timeout=30):
    """Atomically write a file (or upload bytes as base64)."""
    return _request(base, token, f"/api/v1/servers/{urllib.parse.quote(sid)}/files",
                    method="PUT",
                    body={"path": path, "content": content, "encoding": encoding},
                    timeout=timeout)


def deploy(base, token, spec, timeout=900):
    """Pull (if needed) → create → start. Long call: image pulls run minutes."""
    return _request(base, token, "/api/v1/servers", method="POST", body=spec,
                    timeout=timeout)


def stream_lines(base, token, sid, tail, follow, timeout=60):
    """Yield raw SSE bytes from the agent's log stream.

    Bytes are forwarded untouched (agent events, `: hb` comments included) —
    no double parsing, no re-tagging. `timeout` is the socket read timeout;
    the agent heartbeats every ~20s, so a silent 60s means a dead link.
    """
    q = urllib.parse.urlencode({"tail": tail, "follow": 1 if follow else 0})
    url = (base.rstrip("/") + f"/api/v1/servers/{urllib.parse.quote(sid)}"
           + "/logs?" + q)
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        raise _http_error(e) from None
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        raise AgentError(0, f"agent unreachable: {getattr(e, 'reason', e)}") from None
    try:
        while True:
            line = resp.readline()
            if not line:
                break
            yield line
    finally:
        resp.close()
