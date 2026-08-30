"""SandrPod sandbox backend — deepagents BaseSandbox 实现。

设计与 deepagents.backends.sandbox.BaseSandbox 默认行为的差异
================================================================

BaseSandbox 默认通过 ``execute()`` 跑 ``python3 -c "..."`` 脚本来实现
``ls / read / write / edit / grep / glob``——这套在 Linux/macOS 沙箱上
凑合能跑，但在 Windows 沙箱上有三个并发问题：

  1. ``python3`` 通常不在 Windows PATH（Windows 装 Python 后是
     ``python.exe`` 或 ``py -3``），脚本一执行就 ``not recognized``。
  2. 脚本被嵌进 shell 字符串再丢给 toolbox ``execute()``；toolbox 在
     Windows 端走 PowerShell，引号、``$``、反引号、heredoc 语义跟 bash
     完全不同，Python 源码会被 PowerShell 二次解释错乱。
  3. 脚本里 ``open("/memories/foo")`` 在 Windows Python 里被解释为
     「当前盘符根目录下的 memories」（``C:\\memories\\foo``），跟 toolbox
     ``resolveSafePath`` 把 ``/memories`` join 到 work_dir 的语义完全
     不一致。

本实现把 **所有** 文件操作映射到 toolbox 原生 HTTP 端点（``/files`` /
``/files/info`` / ``/files/folder`` / ``/files/upload`` / ``/files/download``
/ ``/files/search`` / ``/files/find``），全部在服务端走 ``resolveSafePath``
+ permission gate。SDK 客户端只做请求/响应转换，不再依赖任何沙箱端的
脚本运行能力。

路径校验放宽：接受 POSIX 绝对路径（``/foo``）和 Windows 盘符路径
（``C:\\foo`` / ``D:/bar``）两种形式，仅拒纯相对路径。真正的安全决策回归
toolbox 服务端的 ``resolveAndAuthorize`` + permission_mode（off/prompt/
strict）。
"""

from __future__ import annotations

import base64
import fnmatch
import json
import logging
import ntpath
import os
import posixpath
import re

import httpx

try:
    from deepagents.backends.protocol import (
        EditResult,
        ExecuteResponse,
        FileData,
        FileDownloadResponse,
        FileInfo,
        FileUploadResponse,
        GlobResult,
        GrepMatch,
        GrepResult,
        LsResult,
        ReadResult,
        WriteResult,
    )
    from deepagents.backends.sandbox import BaseSandbox
except ImportError as exc:
    raise ImportError(
        "langchain-sandrpod requires the deepagents package.\n"
        "Install with:  pip install 'deepagents>=0.5.0,<0.8.0'"
    ) from exc

# DeleteResult is present only in deepagents versions that added the native
# `delete` backend method. Import it separately with a shim fallback so this
# package still imports on older deepagents (where the framework never calls
# delete()). Defining delete() is harmless there — nothing invokes it.
try:
    from deepagents.backends.protocol import DeleteResult
except ImportError:  # pragma: no cover - deepagents without native delete
    from dataclasses import dataclass as _dataclass

    @_dataclass
    class DeleteResult:  # type: ignore[no-redef]
        error: str | None = None
        path: str | None = None

logger = logging.getLogger(__name__)


# 合法路径：POSIX 绝对（``/foo``）或 Windows 盘符（``C:\\foo`` / ``D:/bar``）。
# 不接受纯相对路径——保留 SDK 协议「明确的绝对路径」原意。
_VALID_PATH = re.compile(r"^([a-zA-Z]:|/)")

# read() 单次输出上限，避免大文件 base64 后撑爆传输/上游缓冲。与
# BaseSandbox.MAX_OUTPUT_BYTES 保持一致以便 LLM 拿到的语义不漂移。
_MAX_OUTPUT_BYTES = 500 * 1024


def _is_valid_path(path: str) -> bool:
    return bool(_VALID_PATH.match(path))


def _is_windows_style(path: str) -> bool:
    """检测路径是否是 Windows 风格（盘符前缀或含反斜杠）。"""
    return bool(re.match(r"^[a-zA-Z]:", path)) or "\\" in path


def _split_dir_basename(path: str) -> tuple[str, str]:
    """跨平台路径切分。

    Windows 风格走 ``ntpath``（``C:\\foo\\bar.txt`` → ``C:\\foo``, ``bar.txt``），
    POSIX 风格走 ``posixpath``（``/foo/bar.txt`` → ``/foo``, ``bar.txt``）。
    避免 ``posixpath.dirname("C:\\foo\\bar.txt")`` 返回空串、整串当文件名
    的历史 bug。
    """
    if _is_windows_style(path):
        return ntpath.split(path)
    return posixpath.split(path)


def _try_replace(
    text: str, old: str, new: str, replace_all: bool,
) -> tuple[str, int]:
    """字符串替换，尝试 as-is → CRLF 归一两轮，保留原文件行尾风格。

    与 BaseSandbox 的 _EDIT_COMMAND_TEMPLATE 语义对齐：read() 一般给 LLM 的
    是 LF 文本，但磁盘可能是 CRLF；按 as-is 找不到时再用 CRLF 变体重试，
    替换字符串也跟着 CRLF 化，避免把混合行尾写出去。

    Returns:
        (new_text, occurrences) — occurrences 为 0 表示没找到。
    """
    count = text.count(old)
    if count > 0:
        replaced = text.replace(old, new) if replace_all else text.replace(old, new, 1)
        return replaced, count

    # old 是 LF 风格、文件是 CRLF 风格的情形
    if "\n" in old and "\r\n" not in old:
        crlf_old = old.replace("\n", "\r\n")
        crlf_new = new.replace("\n", "\r\n")
        count = text.count(crlf_old)
        if count > 0:
            replaced = (
                text.replace(crlf_old, crlf_new)
                if replace_all
                else text.replace(crlf_old, crlf_new, 1)
            )
            return replaced, count

    return text, 0


class SandrPodSandbox(BaseSandbox):
    """SandrPod sandbox backend for deepagents / LangChain agents.

    Subclasses :class:`deepagents.backends.sandbox.BaseSandbox` and **overrides
    every** high-level file operation (``ls / read / write / edit / grep /
    glob``). Rather than the parent's ``python3 -c "..."`` implementations,
    these go to the toolbox's native HTTP endpoints, so behaviour is identical
    on Linux, macOS and Windows sandboxes.

    All traffic goes through the SandrPod API Server's tunnel proxy; the
    container IP is never needed.

    Example::

        from langchain_sandrpod import SandrPodSandbox
        from deepagents import create_deep_agent
        from deepagents.middleware import FilesystemMiddleware

        sb = SandrPodSandbox(sandbox_name="agent-sb",
                             api_url="http://localhost:8080",
                             api_token="...")
        agent = create_deep_agent(
            middleware=[FilesystemMiddleware(backend=sb)]
        )
    """

    # 每次 execute() 调用后额外留给 HTTP 层的缓冲时间（秒）
    _HTTP_BUFFER_SECS = 15
    # 文件类操作的固定 HTTP 超时
    _FILE_HTTP_TIMEOUT = 120.0

    def __init__(
        self,
        *,
        sandbox_name: str,
        api_url: str | None = None,
        api_token: str | None = None,
        mcp_token: str | None = None,
        default_timeout: int = 30 * 60,
        _http: httpx.Client | None = None,
    ) -> None:
        """
        Args:
            sandbox_name:    Sandbox 名称（在 API Server 中唯一）。
            api_url:         API Server 地址，默认读取 ``SANDRPOD_API_URL``
                             环境变量，再回退到 ``http://localhost:8080``。
            api_token:       平台 token（X-Sandrpod-Token），默认读取
                             ``SANDRPOD_API_TOKEN`` 环境变量。
            mcp_token:       个人 MCP token（agent bridge 共享密钥），仅在 /mcp
                             调用时作 ``Authorization: Bearer`` 透传给 agent 校验，
                             默认读取 ``SANDRPOD_MCP_TOKEN`` 环境变量。
            default_timeout: execute() 未指定 timeout 时的默认超时（秒）。
            _http:           供测试注入的 httpx.Client 实例。
        """
        self._sandbox_name = sandbox_name
        self._base_url = (
            api_url or os.environ.get("SANDRPOD_API_URL") or "http://localhost:8080"
        ).rstrip("/")
        self._api_token = api_token or os.environ.get("SANDRPOD_API_TOKEN")
        self._mcp_token = mcp_token or os.environ.get("SANDRPOD_MCP_TOKEN")
        self._default_timeout = default_timeout
        self._http = _http or self._build_http_client()

    # ------------------------------------------------------------------ #
    # Internal helpers                                                     #
    # ------------------------------------------------------------------ #

    def _build_http_client(self) -> httpx.Client:
        # 优先 X-Sandrpod-Token,让 Authorization 留给 MCP 资源层(agent
        # --mcp-token)。同时保留 Authorization 是为了兼容老服务端(还
        # 没合入 X-Sandrpod-Token 支持的版本)——服务端 authMiddleware 优先
        # 看 X-Sandrpod-Token,fallback 到 Authorization。
        # 见 docs/MCP_AUTH_HEADER_CONFLICT_FIX.md。
        headers: dict[str, str] = {}
        if self._api_token:
            headers["X-Sandrpod-Token"] = self._api_token
            headers["Authorization"] = f"Bearer {self._api_token}"
        # timeout=None: 由调用方在每个请求上动态设置
        return httpx.Client(base_url=self._base_url, headers=headers, timeout=None)

    # ------------------------------------------------------------------ #
    # MCP transport bridge — see docs/MCP_BRIDGE.md                       #
    # ------------------------------------------------------------------ #

    def mcp_url(self) -> str:
        """Return the URL of the sandbox's MCP transport bridge endpoint.

        The bridge runs inside every sandbox's toolbox (poder container or a
        bare ``sandrpod-agent``) and aggregates the stdio/remote MCP servers
        defined in the sandbox's ``mcp.json`` into a single Streamable-HTTP MCP
        endpoint. Manage that server set with :meth:`mcp_add` / :meth:`mcp_rm` /
        :meth:`mcp_ls` / :meth:`mcp_tools`. Hand this URL to any MCP-compatible
        client (e.g. ``langchain-mcp-adapters``).

        Example::

            from langchain_mcp_adapters.client import MultiServerMCPClient

            sb = SandrPodSandbox(sandbox_name="my-laptop")
            client = MultiServerMCPClient({
                "personal": {"url": sb.mcp_url(), "transport": "streamable_http"},
            })
            tools = await client.get_tools()
        """
        return f"{self._base_url}/api/v1/sandboxes/{self._sandbox_name}/mcp"

    def mcp_manifest_url(self) -> str:
        """URL of the bridge's introspection endpoint.

        ``GET`` returns a JSON payload listing every loaded MCP server with
        its state and tool count. Useful for health checks before opening
        an MCP session, or for surfacing "what tools are available" in a UI.
        """
        return f"{self._base_url}/api/v1/sandboxes/{self._sandbox_name}/mcp/manifest"

    _DEFAULT_MCP_CONFIG = "/workspace/.sandrpod/mcp.json"

    def mcp_manifest(self) -> dict:
        """Live MCP manifest: aggregated servers, each one's state and tool_count, and config_path.

        ``GET /mcp/manifest``, valid for both poder and agent sandboxes. When
        the agent guards ``/mcp`` with ``--mcp-token``, that token is sent as
        ``Authorization: Bearer`` — the server authenticates with
        X-Sandrpod-Token and passes this header through untouched.
        """
        headers = {}
        if self._mcp_token:
            headers["Authorization"] = f"Bearer {self._mcp_token}"
        resp = self._http.get(
            f"/api/v1/sandboxes/{self._sandbox_name}/mcp/manifest",
            headers=headers,
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()
        return resp.json()

    def mcp_tools(self) -> list[dict]:
        """List the live aggregated MCP servers, with state, tool_count and last_error."""
        return self.mcp_manifest().get("servers", [])

    def _mcp_config_path(self, override: str | None = None) -> str:
        """mcp.json 路径：override > manifest.config_path > 按 substrate 猜默认。

        优先用 bridge 自报的绝对路径(精确)。旧 bridge 不报时按 substrate 回退：
        poder(容器)沙箱用 /workspace/.sandrpod/mcp.json,direct agent(本机)
        用 ~/.sandrpod/mcp.json(agent 的 DefaultConfigPath)。
        """
        if override:
            return override
        try:
            path = self.mcp_manifest().get("config_path")
            if path:
                return path
        except Exception:  # noqa: BLE001 — best-effort discovery, fall back below
            pass
        try:
            resp = self._http.get(
                f"/api/v1/sandboxes/{self._sandbox_name}",
                timeout=self._FILE_HTTP_TIMEOUT,
            )
            if resp.is_success and str(resp.json().get("proxy_url", "")).startswith("direct://"):
                return os.path.expanduser("~/.sandrpod/mcp.json")
        except Exception:  # noqa: BLE001 — best-effort; fall back to the poder default
            pass
        return self._DEFAULT_MCP_CONFIG

    def _read_mcp_config(self, config_path: str) -> dict:
        resp = self._http.get(
            self._toolbox_url("files/download"),
            params={"path": config_path},
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        if not resp.is_success:
            return {"mcpServers": {}}
        text = resp.text.strip()
        if not text:
            return {"mcpServers": {}}
        cfg = json.loads(text)
        if not isinstance(cfg.get("mcpServers"), dict):
            cfg = {**cfg, "mcpServers": {}}
        return cfg

    def _write_mcp_config(self, config_path: str, cfg: dict) -> None:
        dir_path, filename = _split_dir_basename(config_path)
        resp = self._http.post(
            self._toolbox_url("files/upload"),
            params={"path": dir_path},
            files={"file": (filename, json.dumps(cfg, indent=2).encode(), "application/octet-stream")},
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()

    def mcp_ls(self, *, config_path: str | None = None) -> dict:
        """The MCP servers configured in the sandbox (reads mcp.json). Returns ``{name: server_config}``."""
        path = self._mcp_config_path(config_path)
        return self._read_mcp_config(path).get("mcpServers", {})

    def mcp_add(
        self,
        name: str,
        *,
        command: str | None = None,
        args: list[str] | None = None,
        url: str | None = None,
        env: dict[str, str] | None = None,
        headers: dict[str, str] | None = None,
        transport: str | None = None,
        config_path: str | None = None,
    ) -> None:
        """Add an MCP server to the sandbox's native bridge (edits mcp.json; the bridge hot-reloads).

        stdio ::

            sb.mcp_add("exa", command="npx", args=["-y", "exa-mcp-server"],
                       env={"EXA_API_KEY": "…"})

        remote ::

            sb.mcp_add("gh", url="https://api.githubcopilot.com/mcp/",
                       headers={"Authorization": "Bearer …"})
        """
        entry: dict = {}
        if url:
            entry["url"] = url
            if transport:
                entry["type"] = transport
            if headers:
                entry["headers"] = dict(headers)
        elif command:
            entry["command"] = command
            if args:
                entry["args"] = list(args)
        else:
            raise ValueError("mcp_add: 需要 command (stdio) 或 url (remote)")
        if env:
            entry["env"] = dict(env)
        path = self._mcp_config_path(config_path)
        cfg = self._read_mcp_config(path)
        servers = {**cfg.get("mcpServers", {}), name: entry}
        self._write_mcp_config(path, {**cfg, "mcpServers": servers})

    def mcp_rm(self, name: str, *, config_path: str | None = None) -> bool:
        """Remove an MCP server. Returns whether it existed and was removed."""
        path = self._mcp_config_path(config_path)
        cfg = self._read_mcp_config(path)
        if name not in cfg.get("mcpServers", {}):
            return False
        servers = {k: v for k, v in cfg["mcpServers"].items() if k != name}
        self._write_mcp_config(path, {**cfg, "mcpServers": servers})
        return True

    def _toolbox_url(self, sub_path: str) -> str:
        """构造通过 API Server 代理到 Toolbox 的 URL。

        例：sub_path="files/download" →
            "/api/v1/sandboxes/{name}/toolbox/files/download"
        """
        return f"/api/v1/sandboxes/{self._sandbox_name}/toolbox/{sub_path.lstrip('/')}"

    # ------------------------------------------------------------------ #
    # BaseSandbox interface — identity & execute                           #
    # ------------------------------------------------------------------ #

    @property
    def id(self) -> str:
        """The sandbox name, used as the unique identifier."""
        return self._sandbox_name

    def execute(
        self,
        command: str,
        *,
        timeout: int | None = None,
    ) -> ExecuteResponse:
        """Run a shell command in the sandbox, blocking until it finishes.

        Each command runs in its own bash / PowerShell process, so there is no
        persistent shell state. To keep a working directory or environment
        variable across steps, chain the commands with ``&&`` (POSIX) or ``;``
        (PowerShell), or run a script that carries the state itself.

        Args:
            command: The shell command.
            timeout: Seconds to wait. ``None`` uses the default_timeout given
                at construction.
        """
        effective_timeout = timeout if timeout is not None else self._default_timeout

        try:
            resp = self._http.post(
                "/api/v1/sandboxes/execute",
                params={"sandbox": self._sandbox_name},
                json={
                    "language": "bash",
                    "code": command,
                    "timeout": effective_timeout,
                },
                # HTTP 层超时应略大于执行超时，给网络/解析留余量
                timeout=effective_timeout + self._HTTP_BUFFER_SECS,
            )
        except httpx.TimeoutException:
            return ExecuteResponse(
                output=f"Command timed out after {effective_timeout} seconds",
                exit_code=124,
            )
        except httpx.RequestError as exc:
            return ExecuteResponse(
                output=f"Request error: {exc}",
                exit_code=1,
            )

        if not resp.is_success:
            return ExecuteResponse(
                output=f"Proxy error {resp.status_code}: {resp.text[:400]}",
                exit_code=1,
            )

        data = resp.json()
        stdout: str = data.get("stdout") or ""
        stderr: str = data.get("stderr") or ""
        exit_code: int | None = data.get("exit_code")

        # output 以 stdout 为主；失败时附上 stderr 供排查
        output = stdout
        if stderr.strip() and exit_code != 0:
            output = stdout + f"\n<stderr>{stderr.strip()}</stderr>"
        elif not stdout and stderr:
            # 命令无 stdout 输出但有 stderr（如 stderr-only 工具）
            output = stderr

        return ExecuteResponse(output=output, exit_code=exit_code)

    # ------------------------------------------------------------------ #
    # File transfer — download_files / upload_files                       #
    # ------------------------------------------------------------------ #

    def download_files(
        self,
        paths: list[str],
    ) -> list[FileDownloadResponse]:
        """Download files from the sandbox.

        Paths must be POSIX-absolute (``/foo``) or Windows
        drive-qualified. Whether the read is permitted is decided by
        the sandbox's permission gate.

        Args:
            paths: Absolute file paths inside the sandbox.
        """
        results: list[FileDownloadResponse] = []
        for path in paths:
            if not _is_valid_path(path):
                results.append(
                    FileDownloadResponse(path=path, content=None, error="invalid_path")
                )
                continue

            try:
                resp = self._http.get(
                    self._toolbox_url("files/download"),
                    params={"path": path},
                    timeout=self._FILE_HTTP_TIMEOUT,
                )
            except httpx.RequestError:
                results.append(
                    FileDownloadResponse(
                        path=path, content=None, error="permission_denied"
                    )
                )
                continue

            results.append(self._parse_download_response(path, resp))
        return results

    @staticmethod
    def _parse_download_response(
        path: str, resp: httpx.Response,
    ) -> FileDownloadResponse:
        if resp.status_code == 404 or (
            not resp.is_success and "no such file" in resp.text.lower()
        ):
            return FileDownloadResponse(
                path=path, content=None, error="file_not_found",
            )
        if resp.is_success:
            return FileDownloadResponse(
                path=path, content=resp.content, error=None,
            )
        return FileDownloadResponse(
            path=path, content=None, error="permission_denied",
        )

    def upload_files(
        self,
        files: list[tuple[str, bytes]],
    ) -> list[FileUploadResponse]:
        """Upload files to the sandbox.

        Each file is POSTed as its own multipart request to
        ``/files/upload`` and overwrites any existing file.
        :func:`_split_dir_basename` splits dir/basename in a cross-platform
        way, avoiding the bug where ``posixpath`` treats an entire Windows
        path as the basename.

        Args:
            files: ``[(absolute path, contents), …]``.
        """
        results: list[FileUploadResponse] = []
        for path, content in files:
            if not _is_valid_path(path):
                results.append(FileUploadResponse(path=path, error="invalid_path"))
                continue

            dir_path, filename = _split_dir_basename(path)
            if not filename:
                results.append(FileUploadResponse(path=path, error="invalid_path"))
                continue

            try:
                resp = self._http.post(
                    self._toolbox_url("files/upload"),
                    params={"path": dir_path},
                    files={"file": (filename, content, "application/octet-stream")},
                    timeout=self._FILE_HTTP_TIMEOUT,
                )
                if resp.is_success:
                    results.append(FileUploadResponse(path=path, error=None))
                else:
                    results.append(
                        FileUploadResponse(path=path, error="permission_denied")
                    )
            except httpx.RequestError:
                results.append(FileUploadResponse(path=path, error="permission_denied"))

        return results

    # ------------------------------------------------------------------ #
    # High-level file operations — ls / read / write / edit / grep / glob #
    # ------------------------------------------------------------------ #

    def ls(self, path: str) -> LsResult:
        """List a directory via the toolbox's ``GET /files``.

        Response shape: ``{"path": "...", "files": [{"name", "path", "is_dir", "size"}]}``.
        """
        if not _is_valid_path(path):
            return LsResult(error=f"Invalid path: {path}")

        try:
            resp = self._http.get(
                self._toolbox_url("files"),
                params={"path": path},
                timeout=self._FILE_HTTP_TIMEOUT,
            )
        except httpx.RequestError as exc:
            return LsResult(error=f"Request error: {exc}")

        if not resp.is_success:
            return LsResult(
                error=f"Server error {resp.status_code}: {resp.text[:200]}"
            )

        try:
            data = resp.json() or {}
        except ValueError as exc:
            return LsResult(error=f"Invalid response: {exc}")

        entries: list[FileInfo] = []
        for item in (data.get("files") or []):
            entry: FileInfo = {"path": item.get("path", "")}
            if "is_dir" in item:
                entry["is_dir"] = item["is_dir"]
            if "size" in item:
                entry["size"] = item["size"]
            entries.append(entry)
        return LsResult(entries=entries)

    def read(
        self,
        file_path: str,
        offset: int = 0,
        limit: int = 2000,
    ) -> ReadResult:
        """Read a file: ``GET /files/download`` for the whole file, then slice lines client-side.

        On paging: the toolbox has no native offset/limit endpoint, so the
        whole file is fetched and sliced here. The cost is one full transfer
        on first access; for the usual LLM editing case — code and documents
        of tens to hundreds of KB — that is acceptable.

        Binary files (those that fail UTF-8 decoding) come back base64-encoded
        and unpaged.
        """
        if not _is_valid_path(file_path):
            return ReadResult(error=f"Invalid path: {file_path}")

        try:
            resp = self._http.get(
                self._toolbox_url("files/download"),
                params={"path": file_path},
                timeout=self._FILE_HTTP_TIMEOUT,
            )
        except httpx.RequestError as exc:
            return ReadResult(error=f"File '{file_path}': request error: {exc}")

        if resp.status_code == 404 or (
            not resp.is_success and "no such file" in resp.text.lower()
        ):
            return ReadResult(error=f"File '{file_path}': not found")
        if not resp.is_success:
            return ReadResult(
                error=f"File '{file_path}': server error {resp.status_code}: "
                f"{resp.text[:200]}"
            )

        raw = resp.content

        # 二进制文件：base64 不分页（与 BaseSandbox.read 行为一致）
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError:
            return ReadResult(
                file_data=FileData(
                    content=base64.b64encode(raw).decode("ascii"),
                    encoding="base64",
                )
            )

        # 文本：按行切，保留行尾
        lines = text.splitlines(keepends=True)
        total = len(lines)
        offset = max(int(offset), 0)
        if limit is None or limit <= 0:
            page = lines[offset:]
        else:
            page = lines[offset : offset + int(limit)]
        content = "".join(page)

        # 单次输出上限保护
        encoded = content.encode("utf-8")
        if len(encoded) > _MAX_OUTPUT_BYTES:
            content = encoded[:_MAX_OUTPUT_BYTES].decode("utf-8", errors="ignore")
            content += (
                f"\n\n[... truncated at {_MAX_OUTPUT_BYTES} bytes. "
                f"Total lines: {total}. Use offset/limit for further pagination ...]"
            )

        return ReadResult(file_data=FileData(content=content, encoding="utf-8"))

    def write(
        self,
        file_path: str,
        content: str,
    ) -> WriteResult:
        """Create or **overwrite** a file — the deepagents write contract: create if absent, overwrite if present.

        Three steps:
          1. validate the path shape
          2. create parent directories with ``POST /files/folder`` (mkdir -p
             semantics, idempotent)
          3. write the contents with ``POST /files/upload``

        History: early deepagents made write fail on an existing file; the
        contract later became overwrite. The sandbox's ``/files/upload``
        already opens with ``O_CREATE|O_TRUNC``, so dropping the existence
        pre-check yields overwrite semantics — safe for both the old and new
        deepagents, and the less surprising behaviour.
        """
        if not _is_valid_path(file_path):
            return WriteResult(error=f"Invalid path: {file_path}")

        # 确保父目录存在（MkdirAll 幂等，父目录已在则不动）
        parent, _ = _split_dir_basename(file_path)
        if parent:
            mkdir_err = self._mkdir_p(parent)
            if mkdir_err is not None:
                return WriteResult(
                    error=f"Failed to create parent directory '{parent}': {mkdir_err}"
                )

        # 写入
        responses = self.upload_files([(file_path, content.encode("utf-8"))])
        if not responses:
            return WriteResult(error="Upload returned no response")
        r = responses[0]
        if r.error:
            return WriteResult(error=f"Failed to write file '{file_path}': {r.error}")
        return WriteResult(path=file_path)

    def _file_exists(self, file_path: str) -> bool:
        """通过 ``GET /files/info`` 判断目标是否存在。请求失败保守返回 False。"""
        try:
            resp = self._http.get(
                self._toolbox_url("files/info"),
                params={"path": file_path},
                timeout=self._FILE_HTTP_TIMEOUT,
            )
        except httpx.RequestError:
            return False
        return resp.status_code == 200

    def _mkdir_p(self, dir_path: str) -> str | None:
        """通过 ``POST /files/folder`` 创建目录树（幂等）。

        Returns:
            错误描述（失败）或 ``None``（成功）。
        """
        try:
            resp = self._http.post(
                self._toolbox_url("files/folder"),
                params={"path": dir_path},
                timeout=self._FILE_HTTP_TIMEOUT,
            )
        except httpx.RequestError as exc:
            return f"request error: {exc}"
        if resp.is_success:
            return None
        return f"server returned {resp.status_code}: {resp.text[:200]}"

    def delete(self, file_path: str) -> DeleteResult:
        """Delete a file or directory natively, via the toolbox's ``DELETE /files/delete``.

        Overrides :class:`BaseSandbox`, which shells out to
        ``execute("rm -rf …")``. Going through the file API means the request
        passes the sandbox's *file* permission gate rather than its command
        gate, and no shell is spawned. Directories are removed recursively.

        Contract-aligned with deepagents: a missing path returns a not-found
        error. The sandbox's ``/files/delete`` uses ``os.RemoveAll``, which is
        idempotent and does not fail on a missing path, so existence is
        checked first with ``GET /files/info`` to match the contract. A broken
        symlink is a known edge-case difference.
        """
        if not _is_valid_path(file_path):
            return DeleteResult(error=f"Invalid path: {file_path}")
        if not self._file_exists(file_path):
            return DeleteResult(error=f"File not found: '{file_path}'")
        try:
            resp = self._http.delete(
                self._toolbox_url("files/delete"),
                params={"path": file_path},
                timeout=self._FILE_HTTP_TIMEOUT,
            )
        except httpx.RequestError as exc:
            return DeleteResult(
                error=f"Failed to delete '{file_path}': request error: {exc}"
            )
        if resp.is_success:
            return DeleteResult(path=file_path)
        return DeleteResult(
            error=f"Failed to delete '{file_path}': "
            f"server returned {resp.status_code}: {resp.text[:200]}"
        )

    def edit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,  # noqa: FBT001, FBT002
    ) -> EditResult:
        """Edit a file: download, replace the string client-side, upload the result.

        Semantics match BaseSandbox.edit:
          - with replace_all=False, more than one match is an error
          - zero matches reports "String not found"
          - two passes, as-is then CRLF-normalised, preserving the file's own
            line endings

        Note: BaseSandbox uses ``_edit_via_upload``, uploading the old and new
        strings as temp files so a server-side script can replace in place and
        the source never leaves the sandbox. This implementation takes the
        simpler download/upload round trip — the bytes traverse the API server
        proxy either way, so there is no additional exposure, and it decouples
        the behaviour from the platform entirely.
        """
        if not _is_valid_path(file_path):
            return EditResult(error=f"Invalid path: {file_path}")

        # 1. download 当前内容
        dls = self.download_files([file_path])
        if not dls:
            return EditResult(error="Download returned no response")
        dl = dls[0]
        if dl.error == "file_not_found":
            return EditResult(error=f"Error: File '{file_path}' not found")
        if dl.error:
            return EditResult(
                error=f"Error reading file '{file_path}': {dl.error}"
            )
        if dl.content is None:
            return EditResult(error=f"Error: empty content for '{file_path}'")

        # 2. 解码（必须是 UTF-8 文本；二进制不支持 edit）
        try:
            text = dl.content.decode("utf-8")
        except UnicodeDecodeError:
            return EditResult(
                error=f"Error: File '{file_path}' is not a text file"
            )

        # 3. 字符串替换 + 行尾兼容
        new_text, count = _try_replace(text, old_string, new_string, replace_all)
        if count == 0:
            return EditResult(
                error=f"Error: String not found in file: '{old_string}'"
            )
        if not replace_all and count > 1:
            return EditResult(
                error=f"Error: String '{old_string}' appears multiple times "
                f"in '{file_path}'. Use replace_all=True to replace all occurrences."
            )

        # 4. upload 覆盖（不走 write() 的存在性 preflight，因为本来就要覆盖）
        ups = self.upload_files([(file_path, new_text.encode("utf-8"))])
        if not ups:
            return EditResult(error="Upload returned no response")
        u = ups[0]
        if u.error:
            return EditResult(
                error=f"Error writing edited file '{file_path}': {u.error}"
            )

        return EditResult(path=file_path, occurrences=count)

    def grep(
        self,
        pattern: str,
        path: str | None = None,
        glob: str | None = None,
    ) -> GrepResult:
        """Search a directory for lines containing pattern, via the toolbox's ``GET /files/find``.

        Args:
            pattern: A literal substring — not a regular expression.
            path:    Search root; defaults to the sandbox's work_dir.
            glob:    Optional filename filter (e.g. ``*.py``). The toolbox does
                     not support this server-side, so it is applied here with
                     ``fnmatch`` after the results come back.
        """
        search_path = path if path is not None else "."

        try:
            resp = self._http.get(
                self._toolbox_url("files/find"),
                params={"path": search_path, "pattern": pattern},
                timeout=self._FILE_HTTP_TIMEOUT,
            )
        except httpx.RequestError as exc:
            return GrepResult(error=f"Request error: {exc}")

        if not resp.is_success:
            return GrepResult(
                error=f"Server error {resp.status_code}: {resp.text[:200]}"
            )

        try:
            data = resp.json() or []
        except ValueError as exc:
            return GrepResult(error=f"Invalid response: {exc}")

        matches: list[GrepMatch] = []
        for item in data:
            file_path = item.get("file", "")
            if glob and not fnmatch.fnmatch(os.path.basename(file_path), glob):
                continue
            matches.append(
                {
                    "path": file_path,
                    "line": int(item.get("line", 0)),
                    "text": item.get("content", ""),
                }
            )
        return GrepResult(matches=matches)

    def glob(self, pattern: str, path: str = "/") -> GlobResult:
        """Match filenames by glob, via the toolbox's ``GET /files/search``.

        The toolbox implements this as
        ``filepath.Glob(filepath.Join(path, pattern))``, so the semantics are
        Go's ``filepath.Glob``: it does not recurse into subdirectories, and
        Go's implementation does not support ``**`` either. This is not fully
        equivalent to BaseSandbox's pathlib.glob — for recursive matching, use
        grep and de-duplicate on the path field.
        """
        try:
            resp = self._http.get(
                self._toolbox_url("files/search"),
                params={"path": path, "pattern": pattern},
                timeout=self._FILE_HTTP_TIMEOUT,
            )
        except httpx.RequestError as exc:
            return GlobResult(error=f"Request error: {exc}")

        if not resp.is_success:
            return GlobResult(
                error=f"Server error {resp.status_code}: {resp.text[:200]}"
            )

        try:
            data = resp.json() or {}
        except ValueError as exc:
            return GlobResult(error=f"Invalid response: {exc}")

        matches: list[FileInfo] = [
            {"path": fp} for fp in (data.get("files") or [])
        ]
        return GlobResult(matches=matches)

    # ------------------------------------------------------------------
    # Per-sandbox resource stats (toolbox /metrics)
    # ------------------------------------------------------------------
    def metrics(self) -> dict:
        """Live CPU, memory and disk usage for this sandbox.

        Returns {cpu_count, cpu_used_pct, mem_total, mem_used, disk_total, disk_used}.
        """
        resp = self._http.get(self._toolbox_url("metrics"), timeout=self._FILE_HTTP_TIMEOUT)
        resp.raise_for_status()
        return resp.json() or {}

    # ------------------------------------------------------------------
    # Stateful code interpreter (toolbox /code-interpreter/*)
    # ------------------------------------------------------------------
    def run_code(
        self,
        code: str,
        *,
        context: str | None = None,
        timeout: int | None = None,
    ) -> dict:
        """Run code in a **stateful** kernel; variables persist across calls in the same context.

        Returns ``{"stdout", "stderr", "text", "error"}``. Unlike
        :meth:`execute`, which starts a fresh process every time and keeps no
        state, run_code uses a persistent Python kernel in the Jupyter sense:
        set ``x = 1`` in one call and ``x + 1`` sees it in the next.
        """
        eff = timeout if timeout is not None else self._default_timeout
        body: dict = {"code": code}
        if context:
            body["context_id"] = context
        resp = self._http.post(
            self._toolbox_url("code-interpreter/execute"),
            json=body,
            timeout=eff + self._HTTP_BUFFER_SECS,
        )
        resp.raise_for_status()
        return resp.json() or {}

    def create_code_context(self, *, language: str = "python", cwd: str = "") -> dict:
        """Create a new stateful context with its own namespace. Returns {id, language, cwd}."""
        resp = self._http.post(
            self._toolbox_url("code-interpreter/contexts"),
            json={"language": language, "cwd": cwd},
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()
        return resp.json() or {}

    def list_code_contexts(self) -> list[dict]:
        """List every stateful context."""
        resp = self._http.get(
            self._toolbox_url("code-interpreter/contexts"),
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()
        return resp.json() or []

    def restart_code_context(self, context_id: str) -> None:
        """Restart a context's kernel, clearing its namespace but keeping the id."""
        resp = self._http.post(
            self._toolbox_url(f"code-interpreter/contexts/{context_id}/restart"),
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()

    def remove_code_context(self, context_id: str) -> None:
        """Destroy a context and its kernel."""
        resp = self._http.delete(
            self._toolbox_url(f"code-interpreter/contexts/{context_id}"),
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()

    # ------------------------------------------------------------------
    # Directory watch (toolbox /watch/*)
    # ------------------------------------------------------------------
    def watch_dir(self, path: str, *, recursive: bool = False) -> "WatchHandle":
        """Watch a directory. Returns a handle: ``get_new_events()`` polls for new events, ``stop()`` ends it.

        Also usable as a context manager: ``with sb.watch_dir("/x") as w: …``.
        """
        resp = self._http.post(
            self._toolbox_url("watch/create"),
            json={"path": path, "recursive": recursive},
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()
        watcher_id = (resp.json() or {}).get("watcher_id", "")
        return WatchHandle(self, watcher_id)


class WatchHandle:
    """A polling handle for a directory watcher.

    ``get_new_events()`` returns the filesystem events accumulated since the
    last call; ``stop()`` ends the watch. Supports the context-manager
    protocol, stopping automatically on exit.
    """

    def __init__(self, sandbox: "SandrPodSandbox", watcher_id: str) -> None:
        self._sandbox = sandbox
        self._watcher_id = watcher_id
        self._closed = False

    def get_new_events(self) -> list[dict]:
        """The events accumulated since the last call, as ``[{"name", "type"}]``."""
        if self._closed:
            return []
        resp = self._sandbox._http.get(
            self._sandbox._toolbox_url("watch/events"),
            params={"id": self._watcher_id},
            timeout=self._sandbox._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()
        return (resp.json() or {}).get("events", []) or []

    def stop(self) -> None:
        """Stop the watcher. Idempotent."""
        if self._closed:
            return
        self._closed = True
        try:
            self._sandbox._http.post(
                self._sandbox._toolbox_url("watch/remove"),
                json={"watcher_id": self._watcher_id},
                timeout=self._sandbox._FILE_HTTP_TIMEOUT,
            )
        except Exception:
            pass

    def __enter__(self) -> "WatchHandle":
        return self

    def __exit__(self, *exc: object) -> None:
        self.stop()
