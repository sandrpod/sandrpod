"""Unit tests for SandrPodClient."""

from __future__ import annotations

import httpx
import pytest
import respx

from langchain_sandrpod.client import SandrPodClient
from langchain_sandrpod.sandbox import SandrPodSandbox

BASE_URL = "http://localhost:18080"


def _client(**kwargs) -> SandrPodClient:
    return SandrPodClient(api_url=BASE_URL, **kwargs)


# ------------------------------------------------------------------ #
# create_sandbox                                                       #
# ------------------------------------------------------------------ #

@respx.mock
def test_create_sandbox_returns_sandbox_instance():
    respx.post(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(
            201,
            json={"job_id": "job-1", "status": "created", "sandbox": {"name": "my-sb", "state": "RUNNING"}},
        )
    )
    client = _client()
    sb = client.create_sandbox("my-sb")
    assert isinstance(sb, SandrPodSandbox)
    assert sb.id == "my-sb"


@respx.mock
def test_create_sandbox_sends_correct_body():
    route = respx.post(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(201, json={"sandbox": {"name": "sb"}})
    )
    client = _client()
    client.create_sandbox("sb", region="us-west", provider_type="aws", image_id="img-123")

    import json
    body = json.loads(route.calls.last.request.content)
    assert body["name"] == "sb"
    assert body["region"] == "us-west"
    assert body["provider_type"] == "aws"
    assert body["image_id"] == "img-123"


@respx.mock
def test_create_sandbox_raises_on_error():
    respx.post(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(500, text="Internal Error")
    )
    client = _client()
    with pytest.raises(RuntimeError, match="500"):
        client.create_sandbox("fail-sb")


# ------------------------------------------------------------------ #
# delete_sandbox                                                       #
# ------------------------------------------------------------------ #

@respx.mock
def test_delete_sandbox():
    respx.delete(f"{BASE_URL}/api/v1/sandboxes/my-sb").mock(
        return_value=httpx.Response(200, json={"status": "deleted"})
    )
    client = _client()
    client.delete_sandbox("my-sb")   # no exception = success


@respx.mock
def test_delete_sandbox_raises_on_error():
    respx.delete(f"{BASE_URL}/api/v1/sandboxes/my-sb").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    client = _client()
    with pytest.raises(RuntimeError, match="404"):
        client.delete_sandbox("my-sb")


# ------------------------------------------------------------------ #
# list_sandboxes                                                       #
# ------------------------------------------------------------------ #

@respx.mock
def test_list_sandboxes():
    respx.get(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(
            200,
            json={"sandboxes": [{"name": "sb-1", "state": "RUNNING"}]},
        )
    )
    client = _client()
    sandboxes = client.list_sandboxes()
    assert len(sandboxes) == 1
    assert sandboxes[0]["name"] == "sb-1"


@respx.mock
def test_list_sandboxes_empty():
    respx.get(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(200, json={"sandboxes": []})
    )
    client = _client()
    assert client.list_sandboxes() == []


# ------------------------------------------------------------------ #
# context manager                                                      #
# ------------------------------------------------------------------ #

@respx.mock
def test_sandbox_context_manager_creates_and_deletes():
    respx.post(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(201, json={"sandbox": {"name": "ctx-sb"}})
    )
    delete_route = respx.delete(f"{BASE_URL}/api/v1/sandboxes/ctx-sb").mock(
        return_value=httpx.Response(200, json={"status": "deleted"})
    )

    client = _client()
    with client.sandbox("ctx-sb") as sb:
        assert isinstance(sb, SandrPodSandbox)
        assert sb.id == "ctx-sb"

    assert delete_route.called


@respx.mock
def test_sandbox_context_manager_deletes_on_exception():
    respx.post(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(201, json={"sandbox": {"name": "err-sb"}})
    )
    delete_route = respx.delete(f"{BASE_URL}/api/v1/sandboxes/err-sb").mock(
        return_value=httpx.Response(200, json={"status": "deleted"})
    )

    client = _client()
    with pytest.raises(ValueError, match="boom"):
        with client.sandbox("err-sb"):
            raise ValueError("boom")

    assert delete_route.called


@respx.mock
def test_sandbox_context_manager_no_delete_when_auto_delete_false():
    respx.post(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(201, json={"sandbox": {"name": "keep-sb"}})
    )
    delete_route = respx.delete(f"{BASE_URL}/api/v1/sandboxes/keep-sb").mock(
        return_value=httpx.Response(200, json={"status": "deleted"})
    )

    client = _client()
    with client.sandbox("keep-sb", auto_delete=False):
        pass

    assert not delete_route.called


# ------------------------------------------------------------------ #
# auth token                                                           #
# ------------------------------------------------------------------ #

@respx.mock
def test_client_token_forwarded_to_requests():
    route = respx.post(f"{BASE_URL}/api/v1/sandboxes").mock(
        return_value=httpx.Response(201, json={"sandbox": {"name": "auth-sb"}})
    )
    client = SandrPodClient(api_url=BASE_URL, api_token="tok-secret")
    client.create_sandbox("auth-sb")

    auth = route.calls.last.request.headers.get("authorization", "")
    assert auth == "Bearer tok-secret"
