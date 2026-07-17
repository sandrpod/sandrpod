# Copyright 2026 SandrPod Contributors
# CLI Client

import io
import urllib.parse
import requests
from typing import Optional, Dict, Any, List, Tuple


class CLIClient:
    """SandrPod CLI 客户端"""

    def __init__(
        self,
        api_url: str = "http://localhost:8080",
        timeout: int = 30,
        token: Optional[str] = None,
        mcp_token: Optional[str] = None,
    ):
        self.api_url = api_url.rstrip("/")
        self.timeout = timeout
        self.mcp_token = mcp_token
        self.session = requests.Session()
        # Two-tier auth (see docs MCP_AUTH_HEADER_CONFLICT_FIX): the platform token
        # goes in X-Sandrpod-Token, the server's preferred header. Authorization:
        # Bearer is kept as a legacy fallback for older servers — but crucially it
        # stays free to carry the *personal* mcp_token through to the sandbox's
        # agent bridge on /mcp calls (the server passes Authorization through the
        # tunnel untouched). Content-Type is set per-request so multipart uploads
        # can override it.
        if token:
            self.session.headers["X-Sandrpod-Token"] = token
            self.session.headers["Authorization"] = f"Bearer {token}"

    def _request(self, method: str, path: str, timeout: int = None, **kwargs) -> requests.Response:
        """发送 HTTP 请求"""
        url = f"{self.api_url}{path}"
        if timeout is None:
            timeout = self.timeout
        # Set Content-Type: application/json for non-file requests
        if "files" not in kwargs and "data" not in kwargs:
            kwargs.setdefault("headers", {})["Content-Type"] = "application/json"
        resp = self.session.request(method, url, timeout=timeout, **kwargs)
        if resp.status_code >= 400:
            try:
                error_body = resp.json().get("message", resp.text)
            except:
                error_body = resp.text
            status_names = {
                400: "Bad Request",
                401: "Unauthorized",
                403: "Forbidden",
                404: "Not Found",
            }
            status_name = status_names.get(resp.status_code, "Error")
            raise requests.HTTPError(
                f"{status_name} (HTTP {resp.status_code}): {error_body}",
                response=resp
            )
        return resp

    def _handle_error(self, resp: requests.Response) -> None:
        """处理 HTTP 错误响应"""
        if resp.status_code >= 400:
            try:
                error_body = resp.json().get("message", resp.text)
            except:
                error_body = resp.text
            status_names = {
                400: "Bad Request",
                401: "Unauthorized",
                403: "Forbidden",
                404: "Not Found",
            }
            status_name = status_names.get(resp.status_code, "Error")
            raise requests.HTTPError(
                f"{status_name} (HTTP {resp.status_code}): {error_body}",
                response=resp
            )

    def health(self) -> Dict[str, Any]:
        """检查服务健康状态"""
        resp = self._request("GET", "/health")
        return resp.json()

    # ========== Sandbox 操作 ==========

    def list_sandboxes(self) -> List[Dict[str, Any]]:
        """列出所有 Sandbox"""
        resp = self._request("GET", "/api/v1/sandboxes")
        return resp.json().get("sandboxes", [])

    def get_sandbox(self, name: str) -> Dict[str, Any]:
        """获取 Sandbox 信息"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}")
        return resp.json()

    def create_sandbox(
        self,
        name: str,
        region: str = "local",
        provider_type: str = "local",
        instance_type: str = "",
        image: str = "",
        async_: bool = False,
        ttl_seconds: int = 0,
        cpu_cores: float = 0,
        memory_mb: int = 0,
    ) -> Dict[str, Any]:
        """
        创建 Sandbox

        Args:
            name: 名称
            region: 区域
            provider_type: Provider 类型 (local, aws, aliyun, azure, gcp)
            instance_type: 实例类型 (可选)
            image: 容器镜像 ID (可选，空则使用 Poder 默认镜像)
            async_: True 时 server 立即返回 job，后台完成开通
                (轮询 get_job/get_sandbox 跟进；旧版 server 会忽略该字段并同步阻塞)

        Returns:
            {job_id, status, sandbox} 字典
        """
        data: Dict[str, Any] = {
            "name": name,
            "region": region,
            "provider_type": provider_type,
        }
        if instance_type:
            data["instance_type"] = instance_type
        if image:
            data["image_id"] = image
        if async_:
            data["async"] = True
        if ttl_seconds:
            data["ttl_seconds"] = int(ttl_seconds)
        if cpu_cores:
            data["cpu_cores"] = float(cpu_cores)
        if memory_mb:
            data["memory_mb"] = int(memory_mb)
        resp = self._request("POST", "/api/v1/sandboxes", json=data)
        return resp.json()

    def preview(self, name: str, port: int, path: str = "/") -> bytes:
        """访问沙箱内 localhost:{port} 上的 web 服务(经隧道代理)"""
        if not path.startswith("/"):
            path = "/" + path
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/proxy/{port}{path}")
        return resp.content

    def snapshot(self, name: str, image: str = "") -> Dict[str, Any]:
        """把沙箱当前状态提交为镜像(docker commit),返回 {image, sandbox}"""
        path = f"/api/v1/sandboxes/{name}/snapshot"
        if image:
            path += "?image=" + urllib.parse.quote(image, safe="")
        resp = self._request("POST", path)
        return resp.json()

    def get_job(self, job_id: str) -> Dict[str, Any]:
        """查询 Job 状态（async create 的进度/错误）"""
        resp = self._request("GET", f"/api/v1/jobs/{job_id}")
        return resp.json()

    def metrics(self) -> str:
        """获取 Prometheus 文本格式的 /metrics(需要 admin token)"""
        resp = self._request("GET", "/metrics")
        return resp.text

    def pty_url(self, name: str) -> str:
        """交互式 PTY 的 WebSocket URL(http→ws, https→wss)"""
        base = self.api_url
        if base.startswith("https://"):
            base = "wss://" + base[len("https://"):]
        elif base.startswith("http://"):
            base = "ws://" + base[len("http://"):]
        return f"{base}/api/v1/sandboxes/{name}/pty"

    def auth_header(self) -> Optional[str]:
        """当前会话的 Authorization 头值（供 WebSocket 复用）"""
        return self.session.headers.get("Authorization")

    def delete_sandbox(self, name: str) -> None:
        """删除 Sandbox（同时清理容器，tunnel 不可用时也会删除记录）"""
        self._request("DELETE", f"/api/v1/sandboxes/{name}")

    def start_sandbox(self, name: str) -> Dict[str, Any]:
        """启动 Sandbox"""
        resp = self._request("POST", f"/api/v1/sandboxes/{name}/start")
        return resp.json()

    def stop_sandbox(self, name: str) -> Dict[str, Any]:
        """停止 Sandbox"""
        resp = self._request("POST", f"/api/v1/sandboxes/{name}/stop")
        return resp.json()

    def get_sandbox_logs(self, name: str, tail: str = "100") -> str:
        """获取 Sandbox 日志"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/logs?tail={tail}")
        return resp.text

    # ========== 代码执行 ==========

    def execute(
        self,
        name: str,
        command: str,
        timeout: int = 30
    ) -> Dict[str, Any]:
        """
        在指定 Sandbox 中执行 shell 命令

        Args:
            name: Sandbox 名称
            command: shell 命令
            timeout: 超时时间(秒)

        Returns:
            ExecuteResponse (output, exit_code, truncated)
        """
        return self.execute_code(name, "bash", command, timeout)

    def execute_code(
        self,
        name: str,
        language: str,
        code: str,
        timeout: int = 30
    ) -> Dict[str, Any]:
        """
        在指定 Sandbox 中执行代码

        Args:
            name: Sandbox 名称
            language: 语言 (python, node, bash)
            code: 代码
            timeout: 超时时间(秒)

        Returns:
            执行结果
        """
        data = {
            "language": language,
            "code": code,
            "timeout": timeout,
        }
        resp = self._request("POST", f"/api/v1/sandboxes/execute?sandbox={name}", json=data, timeout=timeout)
        return resp.json()

    def stream_execute(
        self,
        name: str,
        language: str,
        code: str,
        timeout: int = 30,
    ):
        """
        流式执行代码，逐个 yield 解析后的 SSE 事件。

        server 的 /api/v1/sandboxes/stream 通过 query 参数接收 language/code/timeout，
        并以 SSE 回传（`event: <type>\\ndata: <data>\\n\\n`，type ∈ stdout/stderr/
        error/exit）。这里按行解析，屏蔽 SSE 协议噪声。

        Yields:
            dict: {"event": stdout|stderr|error|exit, "data": str}
        """
        params = {
            "sandbox": name,
            "language": language,
            "code": code,
            "timeout": str(timeout),
        }
        url = f"{self.api_url}/api/v1/sandboxes/stream"
        with self.session.get(url, params=params, stream=True, timeout=timeout) as resp:
            self._handle_error(resp)
            yield from self._iter_sse_events(resp.iter_lines(decode_unicode=True))

    @staticmethod
    def _iter_sse_events(lines):
        """
        把 SSE 文本行流解析成 {"event", "data"} 事件。

        Toolbox 的格式为 `event: <type>\\ndata: <data>\\n\\n`；多行输出块的
        续行没有 `data:` 前缀，按当前 event 的续行处理。
        """
        event = None
        for line in lines:
            if line is None or line == "":
                event = None  # blank line ends the current SSE event
                continue
            if line.startswith("event:"):
                event = line[len("event:"):].strip()
            elif line.startswith("data:"):
                data = line[len("data:"):]
                if data.startswith(" "):
                    data = data[1:]
                yield {"event": event or "stdout", "data": data}
            else:
                # Continuation line of a multi-line data block.
                yield {"event": event or "stdout", "data": line}

    # ========== 文件操作 ==========

    def list_files(self, name: str, path: str = "") -> Dict[str, Any]:
        """列出目录文件"""
        # path 为空时不传参，让 server 使用项目目录
        params = None if not path else {"path": path}
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/files", params=params)
        return resp.json()

    def read_file(self, name: str, path: str) -> bytes:
        """读取文件内容"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/files/download", params={"path": path})
        return resp.content

    def write_file(self, name: str, path: str, content: str) -> Dict[str, Any]:
        """写入文件 (通过 upload)"""
        files = {"file": (path, content.encode(), "application/octet-stream")}
        encoded_path = urllib.parse.quote(path, safe="")
        resp = self.session.post(
            f"{self.api_url}/api/v1/sandboxes/{name}/toolbox/files/upload?path={encoded_path}",
            files=files,
            timeout=self.timeout
        )
        self._handle_error(resp)
        return resp.json()

    def mcp_manifest(self, name: str) -> Dict[str, Any]:
        """沙箱原生 MCP bridge 的实时清单 (聚合 server、工具数、配置文件路径)"""
        # /mcp (not /toolbox/mcp): the streaming bridge proxy, which flushes SSE —
        # the path a real MCP client uses. Works for poder + agent sandboxes.
        # When the agent guards its bridge with a personal mcp_token, carry it in
        # Authorization: Bearer — the server auths via X-Sandrpod-Token and passes
        # this through to the agent's mcpTokenMiddleware untouched.
        headers = {}
        if self.mcp_token:
            headers["Authorization"] = f"Bearer {self.mcp_token}"
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/mcp/manifest", headers=headers)
        return resp.json()

    def mcp_url(self, name: str) -> str:
        """沙箱原生 MCP 端点 URL (给 MCP 客户端连接；需带鉴权头)"""
        return f"{self.api_url}/api/v1/sandboxes/{name}/mcp"

    def upload_files(self, name: str, files: List[Tuple[str, bytes]], path: str = "/") -> Dict[str, Any]:
        """上传文件列表到指定目录"""
        file_dict = {}
        for fname, fcontent in files:
            file_dict[fname] = (fname, io.BytesIO(fcontent), "application/octet-stream")
        encoded_path = urllib.parse.quote(path, safe="")
        resp = self.session.post(
            f"{self.api_url}/api/v1/sandboxes/{name}/toolbox/files/bulk-upload?path={encoded_path}",
            files=file_dict,
            timeout=self.timeout
        )
        self._handle_error(resp)
        return resp.json()

    def download_files(self, name: str, paths: List[str]) -> List[bytes]:
        """下载文件列表"""
        results = []
        for path in paths:
            results.append(self.read_file(name, path))
        return results

    def create_folder(self, name: str, path: str) -> Dict[str, Any]:
        """创建目录"""
        resp = self._request("POST", f"/api/v1/sandboxes/{name}/toolbox/files/folder", params={"path": path})
        return resp.json()

    def move_file(self, name: str, source: str, destination: str) -> Dict[str, Any]:
        """移动/重命名文件"""
        resp = self._request("POST", f"/api/v1/sandboxes/{name}/toolbox/files/move", params={"source": source, "destination": destination})
        return resp.json()

    def search_files(self, name: str, path: str = "", pattern: str = "*") -> List[str]:
        """搜索文件 (glob)"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/files/search", params={"path": path, "pattern": pattern})
        return resp.json()

    def find_in_files(self, name: str, path: str = "", pattern: str = "") -> List[Dict[str, Any]]:
        """在文件中搜索内容"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/files/find", params={"path": path, "pattern": pattern})
        return resp.json()

    def replace_in_files(self, name: str, files: List[str], pattern: str, new_value: str) -> Dict[str, Any]:
        """替换文件中的文本"""
        payload = {"files": files, "pattern": pattern, "newValue": new_value}
        resp = self._request("POST", f"/api/v1/sandboxes/{name}/toolbox/files/replace", json=payload)
        return resp.json()

    def get_file_info(self, name: str, path: str) -> Dict[str, Any]:
        """获取文件信息"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/files/info", params={"path": path})
        return resp.json()

    # ---- per-sandbox resource stats (toolbox /metrics) ----
    def get_sandbox_stats(self, name: str) -> Dict[str, Any]:
        """单个沙箱的 CPU/内存/磁盘用量（区别于服务端 Prometheus metrics）。"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/metrics")
        return resp.json()

    # ---- stateful code interpreter (toolbox /code-interpreter/*) ----
    def run_code(self, name: str, code: str, context_id: str = "") -> Dict[str, Any]:
        """在有状态内核里执行代码；同一 context 内变量跨调用保留。返回 {stdout,stderr,text,error}。"""
        body: Dict[str, Any] = {"code": code}
        if context_id:
            body["context_id"] = context_id
        resp = self._request(
            "POST", f"/api/v1/sandboxes/{name}/toolbox/code-interpreter/execute", json=body)
        return resp.json()

    def create_code_context(self, name: str, language: str = "python", cwd: str = "") -> Dict[str, Any]:
        """创建一个新的有状态执行上下文（独立命名空间）。返回 {id,language,cwd}。"""
        resp = self._request(
            "POST", f"/api/v1/sandboxes/{name}/toolbox/code-interpreter/contexts",
            json={"language": language, "cwd": cwd})
        return resp.json()

    def list_code_contexts(self, name: str) -> list:
        """列出所有有状态执行上下文。"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/code-interpreter/contexts")
        return resp.json() or []

    def restart_code_context(self, name: str, context_id: str) -> None:
        """重启上下文的内核（清空其命名空间，保留 id）。"""
        self._request(
            "POST", f"/api/v1/sandboxes/{name}/toolbox/code-interpreter/contexts/{context_id}/restart")

    def remove_code_context(self, name: str, context_id: str) -> None:
        """销毁一个上下文及其内核。"""
        self._request(
            "DELETE", f"/api/v1/sandboxes/{name}/toolbox/code-interpreter/contexts/{context_id}")

    # ---- directory watch (toolbox /watch/*) ----
    def watch_create(self, name: str, path: str, recursive: bool = False) -> str:
        """开始监视目录，返回 watcher_id。"""
        resp = self._request(
            "POST", f"/api/v1/sandboxes/{name}/toolbox/watch/create",
            json={"path": path, "recursive": recursive})
        return resp.json().get("watcher_id", "")

    def watch_events(self, name: str, watcher_id: str) -> list:
        """拉取自上次调用以来累积的文件系统事件 [{name,type}]。"""
        resp = self._request(
            "GET", f"/api/v1/sandboxes/{name}/toolbox/watch/events", params={"id": watcher_id})
        return resp.json().get("events", []) or []

    def watch_remove(self, name: str, watcher_id: str) -> None:
        """停止一个 watcher。"""
        self._request(
            "POST", f"/api/v1/sandboxes/{name}/toolbox/watch/remove", json={"watcher_id": watcher_id})

    def delete_file(self, name: str, path: str) -> Dict[str, Any]:
        """删除文件/目录"""
        resp = self._request("DELETE", f"/api/v1/sandboxes/{name}/toolbox/files/delete", params={"path": path})
        return resp.json()

    # ========== Poder 操作 ==========

    def get_sandbox_env(self, name: str) -> Dict[str, Any]:
        """
        获取 Sandbox 容器运行环境信息（供 AI 生成脚本时参考）

        从容器内 Toolbox 直接读取，比沙箱元数据更精确，
        包含 arch、os、os_version、kernel_version、shell、work_dir。

        Args:
            name: Sandbox 名称

        Returns:
            EnvironmentInfo dict
        """
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/toolbox/info")
        return resp.json()

    def list_poders(self) -> List[Dict[str, Any]]:
        """列出所有 Poder"""
        resp = self._request("GET", "/api/v1/poders")
        return resp.json().get("poders", [])

    def get_poder(self, poder_id: str) -> Dict[str, Any]:
        """获取单个 Poder 信息"""
        resp = self._request("GET", f"/api/v1/poders/{poder_id}")
        return resp.json()

    def delete_poder(self, poder_id: str, keep_vm: bool = False) -> None:
        """
        删除 Poder 记录（若在线则同时断开 tunnel）。

        对云 provider（aws/aliyun/azure/gcp），默认同时终止其底层 VM；
        keep_vm=True 时只删记录、保留 VM。
        """
        path = f"/api/v1/poders/{poder_id}"
        if keep_vm:
            path += "?keep_vm=true"
        self._request("DELETE", path)

    # ---- API tokens (admin) ----

    def create_token(self, name: str, role: str = "user") -> Dict[str, Any]:
        """
        签发一个 API token（需 admin）。返回体含裸 key（仅此一次），
        key 为 e2b_<hex> 格式，可直接作 E2B_API_KEY。服务端只存其 hash。
        """
        resp = self._request("POST", "/api/v1/tokens", json={"name": name, "role": role})
        return resp.json()

    def list_tokens(self) -> List[Dict[str, Any]]:
        """列出已签发的 token（不含裸 key，只有 name/prefix/role/created_at）。"""
        resp = self._request("GET", "/api/v1/tokens")
        return resp.json().get("tokens", [])

    def delete_token(self, prefix: str) -> None:
        """按显示前缀吊销一个 token（立即生效）。"""
        self._request("DELETE", f"/api/v1/tokens/{prefix}")

    def create_sandbox_on_poder(
        self,
        poder_id: str,
        name: str,
        region: str = "local",
        provider_type: str = "local",
        instance_type: str = "",
        image: str = "",
    ) -> Dict[str, Any]:
        """
        在指定 Poder 上直接创建 Sandbox（跳过调度器），返回 sandbox 记录。
        """
        data: Dict[str, Any] = {
            "name": name,
            "region": region,
            "provider_type": provider_type,
        }
        if instance_type:
            data["instance_type"] = instance_type
        if image:
            data["image_id"] = image
        resp = self._request("POST", f"/api/v1/poders/{poder_id}/sandboxes", json=data)
        return resp.json()

    # ========== Session 操作 ==========

    def create_session(self, name: str, session_id: str = None) -> Dict[str, Any]:
        """
        创建 Session

        Args:
            name: Sandbox 名称
            session_id: Session ID (可选，自动生成)

        Returns:
            Session 信息
        """
        data = {}
        if session_id:
            data["session_id"] = session_id
        resp = self._request("POST", f"/api/v1/sandboxes/{name}/session", json=data)
        return resp.json()

    def list_sessions(self, name: str) -> List[Dict[str, Any]]:
        """列出所有 Session"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/session")
        data = resp.json()
        # Toolbox 返回数组，或 {sessions: [...]} 格式
        if isinstance(data, list):
            return data
        return data.get("sessions", [])

    def get_session(self, name: str, session_id: str) -> Dict[str, Any]:
        """获取 Session 信息"""
        resp = self._request("GET", f"/api/v1/sandboxes/{name}/session/{session_id}")
        return resp.json()

    def delete_session(self, name: str, session_id: str) -> None:
        """删除 Session"""
        self._request("DELETE", f"/api/v1/sandboxes/{name}/session/{session_id}")

    def execute_in_session(self, name: str, session_id: str, command: str) -> Dict[str, Any]:
        """
        在 Session 中执行命令 (保持状态)

        Args:
            name: Sandbox 名称
            session_id: Session ID
            command: shell 命令

        Returns:
            ExecuteResponse (cmd_id, output, exit_code)
        """
        data = {"command": command}
        resp = self._request("POST", f"/api/v1/sandboxes/{name}/session/{session_id}/exec", json=data)
        return resp.json()

    def close(self):
        """关闭客户端"""
        self.session.close()
