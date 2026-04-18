# Copyright 2024 SandrPod
# Sandbox 对象

from typing import Dict, Any, Optional
import requests


class Sandbox:
    """SandPod 沙箱包装器"""

    def __init__(
        self,
        name: str,
        api_url: str,
        api_key: str,
        info: Optional[Dict[str, Any]] = None
    ):
        self.name = name
        self.api_url = api_url.rstrip("/")
        self.api_key = api_key
        self._info = info or {}

        self.session = requests.Session()
        self.session.headers.update({
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
        })

    def _request(self, method: str, path: str, **kwargs) -> requests.Response:
        url = f"{self.api_url}{path}"
        resp = self.session.request(method, url, **kwargs)
        resp.raise_for_status()
        return resp

    @property
    def id(self) -> str:
        return self._info.get("id", self.name)

    @property
    def state(self) -> str:
        return self._info.get("state", "UNKNOWN")

    @property
    def ip(self) -> str:
        return self._info.get("ip", "")

    @property
    def region(self) -> str:
        return self._info.get("region", "")

    def refresh(self) -> None:
        """刷新状态"""
        resp = self._request("GET", f"/api/v1/pods/{self.name}")
        self._info = resp.json()

    def start(self) -> None:
        """启动沙箱"""
        self._request("POST", f"/api/v1/pods/{self.name}/start")
        self.refresh()

    def stop(self) -> None:
        """停止沙箱"""
        self._request("POST", f"/api/v1/pods/{self.name}/stop")
        self.refresh()

    def delete(self) -> None:
        """删除沙箱"""
        self._request("DELETE", f"/api/v1/pods/{self.name}")

    def process(
        self,
        language: str,
        code: str,
        timeout: int = 30
    ) -> "ProcessResult":
        """
        执行代码

        Args:
            language: 语言 (python, node, bash)
            code: 代码
            timeout: 超时时间(秒)

        Returns:
            ProcessResult 对象
        """
        data = {
            "language": language,
            "code": code,
            "timeout": timeout,
        }
        resp = self._request("POST", f"/api/v1/pods/{self.name}/process", json=data)
        return ProcessResult(resp.json())

    def health(self) -> Dict[str, Any]:
        """健康检查"""
        resp = self._request("GET", f"/api/v1/pods/{self.name}/health")
        return resp.json()

    def __repr__(self) -> str:
        return f"<Sandbox {self.name} ({self.state})>"


class ProcessResult:
    """代码执行结果"""

    def __init__(self, data: Dict[str, Any]):
        self.exit_code: int = data.get("exit_code", 0)
        self.stdout: str = data.get("stdout", "")
        self.stderr: str = data.get("stderr", "")
        self.started_at: str = data.get("started_at", "")
        self.ended_at: str = data.get("ended_at", "")

    @property
    def success(self) -> bool:
        return self.exit_code == 0

    def __repr__(self) -> str:
        return f"<ProcessResult exit_code={self.exit_code}>"

    def __str__(self) -> str:
        return self.stdout


class SandboxProcess:
    """Sandbox 代码执行接口"""

    def __init__(self, sandbox: Sandbox):
        self._sandbox = sandbox

    def python(self, code: str, timeout: int = 30) -> ProcessResult:
        """执行 Python 代码"""
        return self._sandbox.process("python", code, timeout)

    def node(self, code: str, timeout: int = 30) -> ProcessResult:
        """执行 Node.js 代码"""
        return self._sandbox.process("node", code, timeout)

    def bash(self, code: str, timeout: int = 30) -> ProcessResult:
        """执行 Bash 代码"""
        return self._sandbox.process("bash", code, timeout)

    def code_run(self, code: str, language: str = "python", timeout: int = 30) -> ProcessResult:
        """通用代码执行"""
        return self._sandbox.process(language, code, timeout)
