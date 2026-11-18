"""Client-level behaviour that does not need a server."""

import os

from sandrpod_cli.client import CLIClient, _is_loopback


def test_is_loopback():
    assert _is_loopback("http://127.0.0.1:8080")
    assert _is_loopback("http://localhost:8080")
    assert _is_loopback("http://[::1]:8080")
    assert not _is_loopback("https://api.example.com")
    assert not _is_loopback("http://10.0.0.5:8080")


def test_loopback_ignores_a_system_proxy(monkeypatch):
    # A developer machine often has an OS-wide proxy (Clash on 1082, Charles,
    # …). It must not capture traffic to the local API: the failure mode is a
    # timeout that names the proxy port instead of the server.
    monkeypatch.setenv("HTTP_PROXY", "http://127.0.0.1:1082")
    monkeypatch.setenv("HTTPS_PROXY", "http://127.0.0.1:1082")
    client = CLIClient(api_url="http://127.0.0.1:8080")
    assert client.session.trust_env is False


def test_remote_api_still_honours_proxy(monkeypatch):
    monkeypatch.setenv("HTTP_PROXY", "http://proxy.example:3128")
    client = CLIClient(api_url="https://api.example.com")
    assert client.session.trust_env is True


# ─── egress ────────────────────────────────────────────────────────────────


class _Resp:
    def __init__(self, payload):
        self._payload = payload

    def json(self):
        return self._payload


def test_egress_endpoints(monkeypatch):
    client = CLIClient(api_url="http://127.0.0.1:8080")
    calls = []

    def fake_request(method, path, **kwargs):
        calls.append((method, path, kwargs.get("json")))
        if method == "GET":
            return _Resp({"sandbox": "s1", "policy": {"allowed_domains": ["example.com"]}})
        return _Resp({"sandbox": "s1", "policy": kwargs.get("json")})

    monkeypatch.setattr(client, "_request", fake_request)

    assert client.get_egress("s1")["policy"]["allowed_domains"] == ["example.com"]
    client.set_egress("s1", {"allowed_domains": ["example.org"]})
    client.clear_egress("s1")

    assert calls == [
        ("GET", "/api/v1/sandboxes/s1/egress", None),
        ("PUT", "/api/v1/sandboxes/s1/egress", {"allowed_domains": ["example.org"]}),
        ("DELETE", "/api/v1/sandboxes/s1/egress", None),
    ]


def test_create_sends_network(monkeypatch):
    client = CLIClient(api_url="http://127.0.0.1:8080")
    sent = {}

    def fake_request(method, path, **kwargs):
        sent.update(kwargs.get("json") or {})
        return _Resp({"job_id": "j1"})

    monkeypatch.setattr(client, "_request", fake_request)
    client.create_sandbox("s2", network={"allowed_domains": ["example.com"]})
    assert sent["network"] == {"allowed_domains": ["example.com"]}

    # No policy must mean no field at all, not an empty object: {} is "allow
    # nothing", None is "unconfigured".
    sent.clear()
    client.create_sandbox("s3")
    assert "network" not in sent


def test_snapshot_uses_a_long_timeout(monkeypatch):
    # A docker commit plus a registry push outlives the 30s ordinary timeout;
    # the default must leave room, and --timeout must be honoured.
    client = CLIClient(api_url="http://127.0.0.1:8080")
    seen = []

    def fake_request(method, path, timeout=None, **kwargs):
        seen.append((method, path, timeout))
        return _Resp({"image": "repo/snap:latest"})

    monkeypatch.setattr(client, "_request", fake_request)
    client.snapshot("s1")
    client.snapshot("s1", "repo/snap:2", timeout=42)
    assert seen == [
        ("POST", "/api/v1/sandboxes/s1/snapshot", 600),
        ("POST", "/api/v1/sandboxes/s1/snapshot?image=repo%2Fsnap%3A2", 42),
    ]


# ─── routing ids ───────────────────────────────────────────────────────────


def test_resolve_refs_rewrites_sandbox_paths(monkeypatch):
    client = CLIClient(api_url="http://127.0.0.1:8080", resolve_refs=True)
    calls = []

    def fake_send(method, path, timeout=None, **kwargs):
        calls.append((method, path))
        if path == "/api/v1/sandboxes/display":
            return _Resp({"name": "display", "routing_id": "sb-0123456789abcdef"})
        return _Resp({"ok": True})

    monkeypatch.setattr(client, "_send", fake_send)
    client.list_files("display", "/tmp")
    assert calls == [
        ("GET", "/api/v1/sandboxes/display"),
        ("GET", "/api/v1/sandboxes/sb-0123456789abcdef/toolbox/files"),
    ]


def test_resolve_refs_rewrites_execute_query(monkeypatch):
    client = CLIClient(api_url="http://127.0.0.1:8080", resolve_refs=True)
    calls = []

    def fake_send(method, path, timeout=None, **kwargs):
        calls.append((method, path))
        if path == "/api/v1/sandboxes/display":
            return _Resp({"name": "display", "routing_id": "sb-0123456789abcdef"})
        return _Resp({"output": "", "exit_code": 0})

    monkeypatch.setattr(client, "_send", fake_send)
    client.execute("display", "true")
    assert calls[-1][0] == "POST"
    assert "sandbox=sb-0123456789abcdef" in calls[-1][1]
    assert "sandbox=display" not in calls[-1][1]


def test_resolve_refs_caches_the_lookup(monkeypatch):
    client = CLIClient(api_url="http://127.0.0.1:8080", resolve_refs=True)
    calls = []

    def fake_send(method, path, timeout=None, **kwargs):
        calls.append(path)
        if path == "/api/v1/sandboxes/display":
            return _Resp({"name": "display", "routing_id": "sb-0123456789abcdef"})
        return _Resp({})

    monkeypatch.setattr(client, "_send", fake_send)
    client.stop_sandbox("display")
    client.start_sandbox("display")
    assert calls.count("/api/v1/sandboxes/display") == 1
    assert calls.count("/api/v1/sandboxes/sb-0123456789abcdef/stop") == 1
    assert calls.count("/api/v1/sandboxes/sb-0123456789abcdef/start") == 1
