# Copyright 2024 SandrPod
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
        timeout: int = 30
    ):
        self.api_url = api_url.rstrip("/")
        self.timeout = timeout
        self.session = requests.Session()
        self.session.headers.update({
            "Content-Type": "application/json",
        })

    def _request(self, method: str, path: str, timeout: int = None, **kwargs) -> requests.Response:
        """发送 HTTP 请求"""
        url = f"{self.api_url}{path}"
        if timeout is None:
            timeout = self.timeout
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
        instance_type: str = "medium",
    ) -> Dict[str, Any]:
        """
        创建 Sandbox

        Args:
            name: 名称
            region: 区域
            provider_type: Provider 类型 (aws, aliyun, local, docker)
            instance_type: 实例类型

        Returns:
            Sandbox 信息
        """
        data = {
            "name": name,
            "region": region,
            "provider_type": provider_type,
            "instance_type": instance_type,
        }
        # 调用 /api/v1/sandboxes，让 Scheduler 处理编排逻辑
        resp = self._request("POST", "/api/v1/sandboxes", json=data)
        return resp.json()

    def delete_sandbox(self, name: str) -> None:
        """删除 Sandbox"""
        # 需要先获取 sandbox 所在的 Poder
        sandbox = self.get_sandbox(name)
        poder_id = sandbox.get("poder_id")
        if not poder_id:
            raise RuntimeError("Poder ID not found for sandbox")
        self._request("DELETE", f"/api/v1/poders/{poder_id}/sandboxes/{name}")

    def start_sandbox(self, name: str) -> Dict[str, Any]:
        """启动 Sandbox"""
        sandbox = self.get_sandbox(name)
        poder_id = sandbox.get("poder_id")
        if not poder_id:
            raise RuntimeError("Poder ID not found for sandbox")
        resp = self._request("POST", f"/api/v1/poders/{poder_id}/sandboxes/{name}/start")
        return resp.json()

    def stop_sandbox(self, name: str) -> Dict[str, Any]:
        """停止 Sandbox"""
        sandbox = self.get_sandbox(name)
        poder_id = sandbox.get("poder_id")
        if not poder_id:
            raise RuntimeError("Poder ID not found for sandbox")
        resp = self._request("POST", f"/api/v1/poders/{poder_id}/sandboxes/{name}/stop")
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
        resp = requests.post(
            f"{self.api_url}/api/v1/sandboxes/{name}/toolbox/files/upload?path={encoded_path}",
            files=files,
            timeout=self.timeout
        )
        self._handle_error(resp)
        return resp.json()

    def upload_files(self, name: str, files: List[Tuple[str, bytes]], path: str = "/") -> Dict[str, Any]:
        """上传文件列表到指定目录"""
        file_dict = {}
        for fname, fcontent in files:
            file_dict[fname] = (fname, io.BytesIO(fcontent), "application/octet-stream")
        encoded_path = urllib.parse.quote(path, safe="")
        resp = requests.post(
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

    def delete_file(self, name: str, path: str) -> Dict[str, Any]:
        """删除文件/目录"""
        poder_url = self._get_poder_url(name)
        resp = requests.delete(f"{poder_url}/files/delete", params={"path": path}, timeout=self.timeout)
        resp.raise_for_status()
        return resp.json()

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
