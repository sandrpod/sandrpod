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

    @property
    def arch(self) -> str:
        """CPU 架构（继承自 Poder 主机，e.g. amd64, arm64）"""
        return self._info.get("arch", "")

    @property
    def os(self) -> str:
        """操作系统类型（e.g. linux）"""
        return self._info.get("os", "")

    @property
    def os_version(self) -> str:
        """操作系统版本（e.g. Ubuntu 22.04.3 LTS）"""
        return self._info.get("os_version", "")

    def env_info(self) -> "EnvironmentInfo":
        """
        获取容器内真实运行环境信息（通过 Toolbox /info 端点）。

        比 arch/os/os_version 属性更精确：由容器自身上报，
        额外包含 kernel_version、shell、work_dir。

        Returns:
            EnvironmentInfo 对象
        """
        resp = self._request("GET", f"/api/v1/sandboxes/{self.name}/toolbox/info")
        return EnvironmentInfo(resp.json())

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


class EnvironmentInfo:
    """容器运行环境信息（来自 Toolbox /info 端点）"""

    def __init__(self, data: Dict[str, Any]):
        self.arch: str = data.get("arch", "")
        self.os: str = data.get("os", "")
        self.os_version: str = data.get("os_version", "")
        self.kernel_version: str = data.get("kernel_version", "")
        self.shell: str = data.get("shell", "")
        self.work_dir: str = data.get("work_dir", "")

    def __repr__(self) -> str:
        return (
            f"<EnvironmentInfo arch={self.arch!r} os={self.os!r} "
            f"os_version={self.os_version!r} kernel={self.kernel_version!r}>"
        )

    def to_dict(self) -> Dict[str, Any]:
        return {
            "arch": self.arch,
            "os": self.os,
            "os_version": self.os_version,
            "kernel_version": self.kernel_version,
            "shell": self.shell,
            "work_dir": self.work_dir,
        }


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
