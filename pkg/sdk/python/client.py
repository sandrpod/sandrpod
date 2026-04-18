# Copyright 2024 SandrPod
# Python SDK Client

import requests
from typing import Optional, Dict, Any, List

from .sandbox import Sandbox


class SandrPodClient:
    """SandrPod API 客户端"""

    def __init__(
        self,
        api_key: str,
        api_url: str = "http://localhost:8080",
        timeout: int = 30
    ):
        """
        初始化 SandrPod 客户端

        Args:
            api_key: API 密钥
            api_url: API 服务器地址
            timeout: 请求超时时间(秒)
        """
        self.api_key = api_key
        self.api_url = api_url.rstrip("/")
        self.timeout = timeout
        self.session = requests.Session()
        self.session.headers.update({
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
        })

    def _request(self, method: str, path: str, **kwargs) -> requests.Response:
        """发送 HTTP 请求"""
        url = f"{self.api_url}{path}"
        resp = self.session.request(method, url, timeout=self.timeout, **kwargs)
        resp.raise_for_status()
        return resp

    def health(self) -> Dict[str, Any]:
        """检查服务健康状态"""
        resp = self._request("GET", "/health")
        return resp.json()

    # ========== SandPod 操作 ==========

    def list_pods(self) -> List[Dict[str, Any]]:
        """列出所有 SandPod"""
        resp = self._request("GET", "/api/v1/pods")
        return resp.json().get("pods", [])

    def get_pod(self, name: str) -> Dict[str, Any]:
        """获取 SandPod 信息"""
        resp = self._request("GET", f"/api/v1/pods/{name}")
        return resp.json()

    def create_pod(
        self,
        name: str,
        region: str = "cn-hangzhou",
        instance_type: str = "ecs.g6.large",
        image_id: Optional[str] = None,
        labels: Optional[Dict[str, str]] = None,
    ) -> Sandbox:
        """
        创建 SandPod

        Args:
            name: 名称
            region: 区域
            instance_type: 实例类型
            image_id: 镜像 ID
            labels: 标签

        Returns:
            Sandbox 对象
        """
        data = {
            "name": name,
            "region": region,
            "instance_type": instance_type,
            "image_id": image_id or "",
            "labels": labels or {},
        }
        resp = self._request("POST", "/api/v1/pods", json=data)
        info = resp.json()

        return Sandbox(
            name=name,
            api_url=self.api_url,
            api_key=self.api_key,
            info=info,
        )

    def delete_pod(self, name: str) -> None:
        """删除 SandPod"""
        self._request("DELETE", f"/api/v1/pods/{name}")

    def stop_pod(self, name: str) -> None:
        """停止 SandPod"""
        self._request("POST", f"/api/v1/pods/{name}/stop")

    def start_pod(self, name: str) -> None:
        """启动 SandPod"""
        self._request("POST", f"/api/v1/pods/{name}/start")

    # ========== 代码执行 ==========

    def process(
        self,
        name: str,
        language: str,
        code: str,
        timeout: int = 30
    ) -> Dict[str, Any]:
        """
        在指定 SandPod 中执行代码

        Args:
            name: SandPod 名称
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
        resp = self._request("POST", f"/api/v1/pods/{name}/process", json=data)
        return resp.json()

    def process_async(
        self,
        name: str,
        language: str,
        code: str,
        timeout: int = 30
    ) -> str:
        """
        异步执行代码，返回任务 ID

        Args:
            name: SandPod 名称
            language: 语言
            code: 代码
            timeout: 超时时间

        Returns:
            任务 ID
        """
        data = {
            "language": language,
            "code": code,
            "timeout": timeout,
        }
        resp = self._request("POST", f"/api/v1/pods/{name}/process-async", json=data)
        return resp.json().get("task_id")

    def get_task_status(self, task_id: str) -> Dict[str, Any]:
        """获取任务状态"""
        resp = self._request("GET", f"/api/v1/tasks/{task_id}")
        return resp.json()

    # ========== Provider 操作 ==========

    def list_providers(self) -> List[str]:
        """列出所有 Provider"""
        resp = self._request("GET", "/api/v1/providers")
        return resp.json().get("providers", [])

    def list_regions(self, provider: str = "aliyun") -> List[str]:
        """列出 Provider 可用区域"""
        resp = self._request("GET", f"/api/v1/providers/{provider}/regions")
        return resp.json().get("regions", [])

    def list_instance_types(self, provider: str = "aliyun", region: str = "cn-hangzhou") -> List[Dict[str, Any]]:
        """列出实例类型"""
        resp = self._request("GET", f"/api/v1/providers/{provider}/regions/{region}/instance-types")
        return resp.json().get("instance_types", [])

    # ========== Local Agent (toC) ==========

    def connect_local(
        self,
        name: str,
        token: str = "",
        work_dir: str = "",
        agent_bin: str = "",
        reconnect: int = 5,
        blocking: bool = True,
    ) -> Optional["subprocess.Popen"]:
        """
        将当前机器注册为本地沙箱代理（toC 场景）。

        启动 sandrpod-agent 进程，通过 WebSocket tunnel 把本机接入 API Server，
        无需 Docker 或 Poder 层。

        Args:
            name:       沙箱名称（全局唯一）
            token:      API 鉴权 token（可选）
            work_dir:   代码执行工作目录（默认当前目录）
            agent_bin:  sandrpod-agent 二进制路径（不设则从 PATH 查找）
            reconnect:  断线重连间隔（秒）
            blocking:   True = 阻塞直到进程退出；False = 返回 Popen 对象

        Returns:
            blocking=False 时返回 subprocess.Popen，否则返回 None

        Raises:
            FileNotFoundError: sandrpod-agent 未找到
        """
        import shutil
        import subprocess as _sp

        bin_path = agent_bin or (
            __import__("os").environ.get("SANDRPOD_AGENT_BIN")
            or shutil.which("sandrpod-agent")
        )
        if not bin_path:
            raise FileNotFoundError(
                "sandrpod-agent not found. "
                "Build it with: go build -o sandrpod-agent ./cmd/agent"
            )

        cmd = [
            bin_path,
            f"-api-url={self.api_url}",
            f"-name={name}",
            f"-reconnect={reconnect}s",
        ]
        if token or self.api_key:
            cmd.append(f"-token={token or self.api_key}")
        if work_dir:
            cmd.append(f"-work-dir={work_dir}")

        proc = _sp.Popen(cmd)
        if blocking:
            proc.wait()
            return None
        return proc

    def close(self):
        """关闭客户端"""
        self.session.close()

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.close()
