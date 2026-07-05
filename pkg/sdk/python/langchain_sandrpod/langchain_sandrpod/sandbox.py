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
        "Install with:  pip install 'deepagents>=0.5.0,<0.6.0'"
    ) from exc

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
    """SandrPod sandbox backend，适用于 deepagents / LangChain agents。

    继承 :class:`deepagents.backends.sandbox.BaseSandbox` 并 **覆盖全部**
    高层文件操作（``ls / read / write / edit / grep / glob``）——不再
    依赖父类基于 ``python3 -c "..."`` 的实现，全部走 toolbox 原生 HTTP
    端点，从而在 Linux / macOS / Windows 沙箱上行为一致。

    通过 SandrPod API Server 的隧道代理与沙箱内的 Toolbox 交互，无需
    直接访问容器 IP。

    用法示例::

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
        """实时 MCP 清单：聚合的 server、每个的 state/tool_count、以及 config_path。

        ``GET`` 走流式 ``/mcp/manifest``（poder 与 agent 沙箱都适用）。当 agent 以
        ``--mcp-token`` 守卫 /mcp 面时，把该 token 作 ``Authorization: Bearer``
        透传（server 用 X-Sandrpod-Token 鉴权，此头原样转给 agent 校验）。
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
        """列出实时聚合的 MCP server（含 state、tool_count、last_error）。"""
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
        """已配置的 MCP server（读 mcp.json）。返回 ``{name: server_config}``。"""
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
        """加一个 MCP server 到沙箱原生 bridge（改 mcp.json，bridge 热重载）。

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
        """移除一个 MCP server。返回是否存在并被移除。"""
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
        """返回 sandbox 名称作为唯一标识符。"""
        return self._sandbox_name

    def execute(
        self,
        command: str,
        *,
        timeout: int | None = None,
    ) -> ExecuteResponse:
        """在 sandbox 中执行 shell 命令（同步阻塞）。

        命令以独立的 bash / PowerShell 进程执行（无持久 shell 状态）。若
        需在调用间保持 CWD 或环境变量，请将多条命令用 ``&&`` (POSIX) 或
        ``;`` (PowerShell) 连接，或通过 :meth:`execute` 运行含状态的脚本。

        Args:
            command: Shell 命令字符串。
            timeout: 最大等待秒数。``None`` 使用构造时的 default_timeout。
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
        """从 sandbox 下载文件。

        路径必须是 POSIX 绝对（``/foo``）或 Windows 盘符（``C:\\foo``）。
        服务端的 permission gate 决定是否允许访问。

        Args:
            paths: sandbox 内的绝对文件路径列表。
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
        """将文件上传至 sandbox。

        每个文件单独 POST multipart 到 ``/files/upload``，存在则覆盖。
        用 :func:`_split_dir_basename` 跨平台拆 dir/basename，避免
        ``posixpath`` 处理 Windows 路径时把整串当成 basename 的 bug。

        Args:
            files: ``[(绝对路径, 文件内容), ...]``。
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
        """列目录，走 toolbox ``GET /files``。

        响应格式：``{"path": "...", "files": [{"name", "path", "is_dir", "size"}]}``。
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
        """读文件，走 toolbox ``GET /files/download`` 拿整文件 + 客户端切行。

        分页策略：toolbox 当前没有原生 offset/limit 端点，所以走「整文件
        下载 + 客户端切行」。代价是大文件首次访问会全传一次；对一般 LLM
        编辑场景（几十 KB 到几百 KB 的代码/文档）开销可接受。

        二进制文件（UTF-8 解码失败）按 base64 返回不分页。
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
        """创建新文件，目标已存在则报错。

        流程（与 BaseSandbox.write 语义对齐）：
          1. 校验路径形态
          2. 通过 ``GET /files/info`` 检查目标是否已存在（200 → 存在，报错）
          3. 通过 ``POST /files/folder`` 创建父目录（mkdir -p 语义，幂等）
          4. 通过 ``POST /files/upload`` 写入内容
        """
        if not _is_valid_path(file_path):
            return WriteResult(error=f"Invalid path: {file_path}")

        # Preflight: 目标不能已存在
        if self._file_exists(file_path):
            return WriteResult(error=f"Error: File already exists: '{file_path}'")

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

    def edit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,  # noqa: FBT001, FBT002
    ) -> EditResult:
        """编辑文件：download → 客户端字符串替换 → upload 覆盖。

        与 BaseSandbox.edit 语义对齐：
          - replace_all=False 时若有多个匹配则报错
          - 0 匹配时报 "String not found"
          - 尝试 as-is → CRLF 归一两轮，保留原文件行尾风格

        说明：BaseSandbox 用 ``_edit_via_upload`` 把 old/new 字符串当 tmp 文
        件上传、让服务端脚本就地替换以避免源文件离开沙箱。本实现选择更简
        单的 download/upload 双向传输——bytes 反正会经过 Acme 代理，没有
        额外隐私损失，换来跨平台彻底解耦。
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
        """在目录下搜文件内容含 pattern 的行，走 toolbox ``GET /files/find``。

        Args:
            pattern: 字面量子串（不是正则）。
            path:    搜索根目录，默认沙箱 work_dir。
            glob:    可选文件名 glob 过滤（如 ``*.py``）。toolbox 服务端不
                     支持该参数，本 SDK 在客户端用 ``fnmatch`` 后过滤。
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
        """文件名 glob 匹配，走 toolbox ``GET /files/search``。

        Toolbox 用 ``filepath.Glob(filepath.Join(path, pattern))`` 实现，
        语义与 Go ``filepath.Glob`` 一致（不递归子目录，需要的话 pattern
        里写 ``**/*.py``——但 Go 的 filepath.Glob 不支持 ``**``；这点跟
        BaseSandbox 的 pathlib.glob 不完全等价，递归需求建议改用 grep
        然后取 path 字段去重）。
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
        """返回该沙箱实时的 CPU/内存/磁盘用量

        {cpu_count, cpu_used_pct, mem_total, mem_used, disk_total, disk_used}。
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
        """在**有状态**内核里执行代码；同一 context 内变量跨调用保留。

        返回 ``{"stdout", "stderr", "text", "error"}``。与 :meth:`execute`
        （每次全新进程、无状态）不同，run_code 走持久 Python 内核（Jupyter
        式）：先 ``x = 1``，之后再调用 ``x + 1`` 能读到 ``x``。
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
        """创建一个新的有状态上下文（独立命名空间）；返回 {id, language, cwd}。"""
        resp = self._http.post(
            self._toolbox_url("code-interpreter/contexts"),
            json={"language": language, "cwd": cwd},
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()
        return resp.json() or {}

    def list_code_contexts(self) -> list[dict]:
        """列出所有有状态上下文。"""
        resp = self._http.get(
            self._toolbox_url("code-interpreter/contexts"),
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()
        return resp.json() or []

    def restart_code_context(self, context_id: str) -> None:
        """重启上下文的内核（清空其命名空间，保留 id）。"""
        resp = self._http.post(
            self._toolbox_url(f"code-interpreter/contexts/{context_id}/restart"),
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()

    def remove_code_context(self, context_id: str) -> None:
        """销毁一个上下文及其内核。"""
        resp = self._http.delete(
            self._toolbox_url(f"code-interpreter/contexts/{context_id}"),
            timeout=self._FILE_HTTP_TIMEOUT,
        )
        resp.raise_for_status()

    # ------------------------------------------------------------------
    # Directory watch (toolbox /watch/*)
    # ------------------------------------------------------------------
    def watch_dir(self, path: str, *, recursive: bool = False) -> "WatchHandle":
        """监视目录，返回一个句柄：``get_new_events()`` 轮询增量事件，``stop()`` 结束。

        也可当上下文管理器用：``with sb.watch_dir("/x") as w: ...``。
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
    """目录 watcher 的轮询句柄。

    ``get_new_events()`` 返回自上次调用以来累积的文件系统事件；``stop()``
    结束监视。支持上下文管理器协议（退出时自动 stop）。
    """

    def __init__(self, sandbox: "SandrPodSandbox", watcher_id: str) -> None:
        self._sandbox = sandbox
        self._watcher_id = watcher_id
        self._closed = False

    def get_new_events(self) -> list[dict]:
        """返回自上次调用以来累积的事件 ``[{"name", "type"}]``。"""
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
        """停止 watcher（幂等）。"""
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
