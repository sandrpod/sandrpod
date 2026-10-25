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
