"""Unit tests for SandrPodSandbox — httpx 请求用 respx mock。"""

from __future__ import annotations

import json

import httpx
import pytest
import respx

from langchain_sandrpod.sandbox import SandrPodSandbox


BASE_URL = "http://localhost:18080"
SB_NAME = "test-sb"


def _make_sandbox(**kwargs) -> SandrPodSandbox:
    """构造 SandrPodSandbox，使用 mock HTTP client。"""
    return SandrPodSandbox(
        sandbox_name=SB_NAME,
        api_url=BASE_URL,
        **kwargs,
    )


# ------------------------------------------------------------------ #
# id property                                                          #
# ------------------------------------------------------------------ #

def test_id_returns_sandbox_name():
    sb = _make_sandbox()
    assert sb.id == SB_NAME


# ------------------------------------------------------------------ #
# execute()                                                            #
# ------------------------------------------------------------------ #

@respx.mock
def test_execute_success():
    respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(
            200,
            json={"exit_code": 0, "stdout": "hello\n", "stderr": ""},
        )
    )
    sb = _make_sandbox()
    result = sb.execute("echo hello")

    assert result.exit_code == 0
    assert result.output == "hello\n"


@respx.mock
def test_execute_sends_correct_payload():
    route = respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(
            200,
            json={"exit_code": 0, "stdout": "ok\n", "stderr": ""},
        )
    )
    sb = _make_sandbox()
    sb.execute("ls /workspace", timeout=60)

    request = route.calls.last.request
    body = json.loads(request.content)
    assert body["language"] == "bash"
    assert body["code"] == "ls /workspace"
    assert body["timeout"] == 60
    assert request.url.params["sandbox"] == SB_NAME


@respx.mock
def test_execute_appends_stderr_on_failure():
    respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(
            200,
            json={"exit_code": 1, "stdout": "", "stderr": "No such file"},
        )
    )
    sb = _make_sandbox()
    result = sb.execute("cat /nonexistent")

    assert result.exit_code == 1
    assert "No such file" in result.output


@respx.mock
def test_execute_returns_stderr_only_when_stdout_empty():
    respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(
            200,
            json={"exit_code": 0, "stdout": "", "stderr": "warning: something"},
        )
    )
    sb = _make_sandbox()
    result = sb.execute("some-tool")
    assert result.output == "warning: something"


@respx.mock
def test_execute_http_error_returns_exit_1():
    respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(502, text="Bad Gateway")
    )
    sb = _make_sandbox()
    result = sb.execute("echo hi")
    assert result.exit_code == 1
    assert "502" in result.output


@respx.mock
def test_execute_network_error_returns_exit_1():
    respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        side_effect=httpx.ConnectError("connection refused")
    )
    sb = _make_sandbox()
    result = sb.execute("echo hi")
    assert result.exit_code == 1


@respx.mock
def test_execute_uses_default_timeout():
    route = respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(
            200, json={"exit_code": 0, "stdout": "", "stderr": ""}
        )
    )
    sb = SandrPodSandbox(
        sandbox_name=SB_NAME,
        api_url=BASE_URL,
        default_timeout=120,
    )
    sb.execute("sleep 1")

    body = json.loads(route.calls.last.request.content)
    assert body["timeout"] == 120


# ------------------------------------------------------------------ #
# upload_files()                                                       #
# ------------------------------------------------------------------ #

@respx.mock
def test_upload_files_success():
    respx.post(
        f"{BASE_URL}/api/v1/sandboxes/{SB_NAME}/toolbox/files/upload"
    ).mock(
        return_value=httpx.Response(
            200,
            json={"success": True, "path": "/workspace/hello.py", "name": "hello.py", "size": 13},
        )
    )
    sb = _make_sandbox()
    results = sb.upload_files([("/workspace/hello.py", b"print('hello')")])

    assert len(results) == 1
    assert results[0].path == "/workspace/hello.py"
    assert results[0].error is None


@respx.mock
def test_upload_files_invalid_path():
    sb = _make_sandbox()
    results = sb.upload_files([("relative/path.py", b"content")])

    assert results[0].error == "invalid_path"


@respx.mock
def test_upload_files_multiple():
    respx.post(
        f"{BASE_URL}/api/v1/sandboxes/{SB_NAME}/toolbox/files/upload"
    ).mock(
        return_value=httpx.Response(
            200, json={"success": True, "path": "/workspace/a.py"}
        )
    )
    sb = _make_sandbox()
    results = sb.upload_files([
        ("/workspace/a.py", b"a"),
        ("/workspace/b.py", b"b"),
        ("bad/path.py", b"c"),
    ])

    assert len(results) == 3
    assert results[0].error is None
    assert results[1].error is None
    assert results[2].error == "invalid_path"


@respx.mock
def test_upload_files_server_error():
    respx.post(
        f"{BASE_URL}/api/v1/sandboxes/{SB_NAME}/toolbox/files/upload"
    ).mock(
        return_value=httpx.Response(500, text="Internal Server Error")
    )
    sb = _make_sandbox()
    results = sb.upload_files([("/workspace/fail.py", b"code")])
    assert results[0].error == "permission_denied"


# ------------------------------------------------------------------ #
# download_files()                                                     #
# ------------------------------------------------------------------ #

@respx.mock
def test_download_files_success():
    respx.get(
        f"{BASE_URL}/api/v1/sandboxes/{SB_NAME}/toolbox/files/download"
    ).mock(
        return_value=httpx.Response(200, content=b"print('hello')")
    )
    sb = _make_sandbox()
    results = sb.download_files(["/workspace/hello.py"])

    assert len(results) == 1
    assert results[0].content == b"print('hello')"
    assert results[0].error is None


@respx.mock
def test_download_files_not_found():
    respx.get(
        f"{BASE_URL}/api/v1/sandboxes/{SB_NAME}/toolbox/files/download"
    ).mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    sb = _make_sandbox()
    results = sb.download_files(["/workspace/missing.py"])

    assert results[0].error == "file_not_found"
    assert results[0].content is None


@respx.mock
def test_download_files_invalid_path():
    sb = _make_sandbox()
    results = sb.download_files(["relative/path.py"])
    assert results[0].error == "invalid_path"


@respx.mock
def test_download_files_mixed():
    respx.get(
        f"{BASE_URL}/api/v1/sandboxes/{SB_NAME}/toolbox/files/download"
    ).mock(
        side_effect=[
            httpx.Response(200, content=b"content-a"),
            httpx.Response(404),
        ]
    )
    sb = _make_sandbox()
    results = sb.download_files([
        "/workspace/a.py",
        "bad/path.txt",
        "/workspace/missing.py",
    ])

    assert results[0].content == b"content-a"
    assert results[0].error is None
    assert results[1].error == "invalid_path"
    assert results[2].error == "file_not_found"


# ------------------------------------------------------------------ #
# token auth                                                           #
# ------------------------------------------------------------------ #

@respx.mock
def test_auth_token_in_header():
    route = respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(
            200, json={"exit_code": 0, "stdout": "ok", "stderr": ""}
        )
    )
    sb = SandrPodSandbox(
        sandbox_name=SB_NAME,
        api_url=BASE_URL,
        api_token="my-secret-token",
    )
    sb.execute("echo ok")

    auth_header = route.calls.last.request.headers.get("authorization", "")
    assert auth_header == "Bearer my-secret-token"


@respx.mock
def test_no_auth_header_when_no_token(monkeypatch):
    monkeypatch.delenv("SANDRPOD_API_TOKEN", raising=False)
    route = respx.post(f"{BASE_URL}/api/v1/sandboxes/execute").mock(
        return_value=httpx.Response(
            200, json={"exit_code": 0, "stdout": "ok", "stderr": ""}
        )
    )
    sb = SandrPodSandbox(sandbox_name=SB_NAME, api_url=BASE_URL)
    sb.execute("echo ok")

    assert "authorization" not in route.calls.last.request.headers


# ------------------------------------------------------------------ #
# metrics / run_code / contexts / watch  (promoted from E2B compat)    #
# ------------------------------------------------------------------ #

TB = f"{BASE_URL}/api/v1/sandboxes/{SB_NAME}/toolbox"


@respx.mock
def test_metrics_returns_usage():
    respx.get(f"{TB}/metrics").mock(
        return_value=httpx.Response(200, json={"cpu_count": 4, "cpu_used_pct": 1.5,
                                               "mem_total": 100, "mem_used": 10,
                                               "disk_total": 200, "disk_used": 20})
    )
    m = _make_sandbox().metrics()
    assert m["cpu_count"] == 4 and m["mem_used"] == 10


@respx.mock
def test_run_code_sends_context_and_returns_result():
    route = respx.post(f"{TB}/code-interpreter/execute").mock(
        return_value=httpx.Response(200, json={"stdout": "", "stderr": "", "text": "42", "error": ""})
    )
    res = _make_sandbox().run_code("a * 6", context="ctx1")
    assert res["text"] == "42"
    assert json.loads(route.calls[0].request.content) == {"code": "a * 6", "context_id": "ctx1"}


@respx.mock
def test_run_code_omits_context_when_none():
    route = respx.post(f"{TB}/code-interpreter/execute").mock(
        return_value=httpx.Response(200, json={"text": "1"})
    )
    _make_sandbox().run_code("1")
    assert json.loads(route.calls[0].request.content) == {"code": "1"}


@respx.mock
def test_code_context_lifecycle():
    respx.post(f"{TB}/code-interpreter/contexts").mock(
        return_value=httpx.Response(200, json={"id": "c1", "language": "python", "cwd": ""})
    )
    respx.get(f"{TB}/code-interpreter/contexts").mock(
        return_value=httpx.Response(200, json=[{"id": "c1", "language": "python", "cwd": ""}])
    )
    restart = respx.post(f"{TB}/code-interpreter/contexts/c1/restart").mock(
        return_value=httpx.Response(204)
    )
    remove = respx.delete(f"{TB}/code-interpreter/contexts/c1").mock(
        return_value=httpx.Response(204)
    )
    sb = _make_sandbox()
    assert sb.create_code_context()["id"] == "c1"
    assert [c["id"] for c in sb.list_code_contexts()] == ["c1"]
    sb.restart_code_context("c1")
    sb.remove_code_context("c1")
    assert restart.called and remove.called


@respx.mock
def test_watch_dir_handle():
    respx.post(f"{TB}/watch/create").mock(
        return_value=httpx.Response(200, json={"watcher_id": "w1"})
    )
    respx.get(f"{TB}/watch/events").mock(
        return_value=httpx.Response(200, json={"events": [{"name": "a.txt", "type": "create"}]})
    )
    removed = respx.post(f"{TB}/watch/remove").mock(return_value=httpx.Response(204))
    sb = _make_sandbox()
    with sb.watch_dir("/tmp") as h:
        events = h.get_new_events()
    assert events == [{"name": "a.txt", "type": "create"}]
    assert removed.called  # context manager stopped it
