"""SandrPod sandbox backend — deepagents BaseSandbox 实现。"""

from __future__ import annotations

import os
import posixpath

import httpx

try:
    from deepagents.backends.protocol import (
        ExecuteResponse,
        FileDownloadResponse,
        FileUploadResponse,
    )
    from deepagents.backends.sandbox import BaseSandbox
except ImportError as exc:
    raise ImportError(
        "langchain-sandrpod requires the deepagents package.\n"
        "Install with:  pip install 'deepagents>=0.5.0,<0.6.0'"
    ) from exc


class SandrPodSandbox(BaseSandbox):
    """SandrPod sandbox backend，适用于 deepagents / LangChain agents。

    继承 :class:`deepagents.backends.sandbox.BaseSandbox`，只需实现三个
    "传输层"方法（:meth:`execute`、:meth:`upload_files`、
    :meth:`download_files`），即可自动获得所有高层文件操作（ls、read、
    write、edit、grep、glob）以及通过 ``FilesystemMiddleware`` 暴露的
    全套 LangChain 工具。

    通过 SandrPod API Server 的隧道代理与沙箱内的 Toolbox 交互，无需
    直接访问容器 IP。

    用法示例::

        from langchain_sandrpod import SandrPodClient
        from deepagents import create_deep_agent
        from deepagents.middleware import FilesystemMiddleware

        client = SandrPodClient(api_url="http://localhost:18080")
        with client.sandbox("agent-sb") as sb:
            agent = create_deep_agent(
                middleware=[FilesystemMiddleware(backend=sb)]
            )
            result = agent.invoke({"messages": [...]})
    """

    # 每次 execute() 调用后额外留给 HTTP 层的缓冲时间（秒）
    _HTTP_BUFFER_SECS = 15

    def __init__(
        self,
        *,
        sandbox_name: str,
        api_url: str | None = None,
        api_token: str | None = None,
        default_timeout: int = 30 * 60,
        _http: httpx.Client | None = None,
    ) -> None:
        """
        Args:
            sandbox_name:    Sandbox 名称（在 API Server 中唯一）。
            api_url:         API Server 地址，默认读取
                             ``SANDRPOD_API_URL`` 环境变量，
                             再回退到 ``http://localhost:18080``。
            api_token:       Bearer 认证 token，默认读取
                             ``SANDRPOD_API_TOKEN`` 环境变量。
            default_timeout: execute() 未指定 timeout 时的默认超时（秒）。
            _http:           供测试注入的 httpx.Client 实例。
        """
        self._sandbox_name = sandbox_name
        self._base_url = (
            api_url or os.environ.get("SANDRPOD_API_URL") or "http://localhost:18080"
        ).rstrip("/")
        self._api_token = api_token or os.environ.get("SANDRPOD_API_TOKEN")
        self._default_timeout = default_timeout
        self._http = _http or self._build_http_client()

    # ------------------------------------------------------------------ #
    # Internal helpers                                                     #
    # ------------------------------------------------------------------ #

    def _build_http_client(self) -> httpx.Client:
        headers: dict[str, str] = {}
        if self._api_token:
            headers["Authorization"] = f"Bearer {self._api_token}"
        # timeout=None: 由调用方在每个请求上动态设置
        return httpx.Client(base_url=self._base_url, headers=headers, timeout=None)

    def _toolbox_url(self, sub_path: str) -> str:
        """构造通过 API Server 代理到 Toolbox 的 URL。

        例：sub_path="files/download" →
            "/api/v1/sandboxes/{name}/toolbox/files/download"
        """
        return f"/api/v1/sandboxes/{self._sandbox_name}/toolbox/{sub_path.lstrip('/')}"

    # ------------------------------------------------------------------ #
    # BaseSandbox interface                                                #
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

        命令以独立的 bash 进程执行（无持久 shell 状态）。若需在调用间
        保持 CWD 或环境变量，请将多条命令用 ``&&`` 连接，或通过
        :meth:`execute` 运行含状态的 bash 脚本。

        Args:
            command: Shell 命令字符串，如 ``"ls /workspace"`` 或
                     ``"python3 -c 'print(42)'"``。
            timeout: 最大等待秒数。``None`` 使用构造时的 default_timeout。

        Returns:
            包含 ``output``（stdout，失败时附加 stderr）、``exit_code``
            的 :class:`ExecuteResponse`。
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

    def upload_files(
        self,
        files: list[tuple[str, bytes]],
    ) -> list[FileUploadResponse]:
        """将文件上传至 sandbox。

        每个文件单独 POST（multipart），文件路径须以 ``/`` 开头。

        Args:
            files: ``[(绝对路径, 文件内容), ...]``。

        Returns:
            与输入顺序一致的 :class:`FileUploadResponse` 列表。
        """
        results: list[FileUploadResponse] = []
        for path, content in files:
            if not path.startswith("/"):
                results.append(FileUploadResponse(path=path, error="invalid_path"))
                continue

            dir_path = posixpath.dirname(path)
            filename = posixpath.basename(path)
            if not filename:
                results.append(FileUploadResponse(path=path, error="invalid_path"))
                continue

            try:
                resp = self._http.post(
                    self._toolbox_url("files/upload"),
                    params={"path": dir_path},
                    files={"file": (filename, content, "application/octet-stream")},
                    timeout=120.0,
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

    def download_files(
        self,
        paths: list[str],
    ) -> list[FileDownloadResponse]:
        """从 sandbox 下载文件。

        每个文件单独 GET，路径须以 ``/`` 开头。

        Args:
            paths: sandbox 内的绝对文件路径列表。

        Returns:
            与输入顺序一致的 :class:`FileDownloadResponse` 列表。
        """
        results: list[FileDownloadResponse] = []
        for path in paths:
            if not path.startswith("/"):
                results.append(
                    FileDownloadResponse(path=path, content=None, error="invalid_path")
                )
                continue

            try:
                resp = self._http.get(
                    self._toolbox_url("files/download"),
                    params={"path": path},
                    timeout=120.0,
                )
                if resp.status_code == 404:
                    results.append(
                        FileDownloadResponse(
                            path=path, content=None, error="file_not_found"
                        )
                    )
                elif resp.is_success:
                    results.append(
                        FileDownloadResponse(
                            path=path, content=resp.content, error=None
                        )
                    )
                else:
                    results.append(
                        FileDownloadResponse(
                            path=path, content=None, error="permission_denied"
                        )
                    )
            except httpx.RequestError:
                results.append(
                    FileDownloadResponse(
                        path=path, content=None, error="permission_denied"
                    )
                )

        return results
