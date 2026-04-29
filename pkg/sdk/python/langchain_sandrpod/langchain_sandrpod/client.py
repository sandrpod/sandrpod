"""SandrPod API Server 客户端 — 管理 sandbox 生命周期。"""

from __future__ import annotations

import os
from collections.abc import Generator
from contextlib import contextmanager
from typing import Any

import httpx


class SandrPodClient:
    """SandrPod API Server HTTP 客户端。

    负责 sandbox 的创建、查询、启停和删除，并作为 :class:`SandrPodSandbox`
    实例的工厂。

    优先级（从高到低）：
      1. 构造参数
      2. 环境变量 ``SANDRPOD_API_URL`` / ``SANDRPOD_API_TOKEN``
      3. 默认值 ``http://localhost:8080``

    Example::

        client = SandrPodClient(api_url="http://localhost:8080")

        # 一次性使用：用完自动删除
        with client.sandbox("my-sb") as sb:
            result = sb.execute("python3 -c 'print(42)'")
            print(result.output)   # 42

        # 长期管理
        sb = client.create_sandbox("long-lived")
        sb.execute("pip install numpy")
        client.delete_sandbox("long-lived")
    """

    DEFAULT_API_URL = "http://localhost:8080"

    def __init__(
        self,
        api_url: str | None = None,
        api_token: str | None = None,
        *,
        request_timeout: float = 30.0,
    ) -> None:
        self._base_url = (
            api_url or os.environ.get("SANDRPOD_API_URL") or self.DEFAULT_API_URL
        ).rstrip("/")
        self._api_token = api_token or os.environ.get("SANDRPOD_API_TOKEN")
        self._request_timeout = request_timeout
        self._http = self._build_http_client()

    # ------------------------------------------------------------------ #
    # Internal helpers                                                     #
    # ------------------------------------------------------------------ #

    def _build_http_client(self) -> httpx.Client:
        headers: dict[str, str] = {}
        if self._api_token:
            headers["Authorization"] = f"Bearer {self._api_token}"
        return httpx.Client(
            base_url=self._base_url,
            headers=headers,
            timeout=self._request_timeout,
        )

    def _raise(self, resp: httpx.Response) -> None:
        if not resp.is_success:
            raise RuntimeError(
                f"SandrPod API error {resp.status_code}: {resp.text[:400]}"
            )

    def _sandbox_from_name(self, name: str) -> "SandrPodSandbox":  # noqa: F821
        from langchain_sandrpod.sandbox import SandrPodSandbox  # avoid circular

        return SandrPodSandbox(
            sandbox_name=name,
            api_url=self._base_url,
            api_token=self._api_token,
            _http=self._http,
        )

    # ------------------------------------------------------------------ #
    # Lifecycle                                                            #
    # ------------------------------------------------------------------ #

    def create_sandbox(
        self,
        name: str,
        *,
        region: str = "local",
        provider_type: str = "local",
        instance_type: str = "",
        image_id: str = "",
    ) -> "SandrPodSandbox":  # noqa: F821
        """创建 sandbox 并等待其进入 RUNNING 状态后返回。

        API Server 的创建接口是同步的：函数返回时 sandbox 已就绪。

        Args:
            name:          Sandbox 名称（同一 API Server 内唯一）。
            region:        区域标识，默认 ``"local"``。
            provider_type: Poder 提供商类型，默认 ``"local"``。
            instance_type: 实例规格（可选）。
            image_id:      容器镜像 ID（空则使用 Poder 默认镜像）。

        Returns:
            指向新建 sandbox 的 :class:`SandrPodSandbox` 实例。
        """
        resp = self._http.post(
            "/api/v1/sandboxes",
            json={
                "name": name,
                "region": region,
                "provider_type": provider_type,
                "instance_type": instance_type,
                "image_id": image_id,
            },
        )
        self._raise(resp)
        return self._sandbox_from_name(name)

    def get_sandbox(self, name: str) -> "SandrPodSandbox":  # noqa: F821
        """获取已存在 sandbox 的操作句柄，不校验其状态。"""
        resp = self._http.get(f"/api/v1/sandboxes/{name}")
        self._raise(resp)
        return self._sandbox_from_name(name)

    def delete_sandbox(self, name: str) -> None:
        """删除 sandbox（停止并移除容器）。"""
        resp = self._http.delete(f"/api/v1/sandboxes/{name}")
        self._raise(resp)

    def start_sandbox(self, name: str) -> None:
        """启动已停止的 sandbox。"""
        resp = self._http.post(f"/api/v1/sandboxes/{name}/start")
        self._raise(resp)

    def stop_sandbox(self, name: str) -> None:
        """停止运行中的 sandbox（保留容器，可恢复）。"""
        resp = self._http.post(f"/api/v1/sandboxes/{name}/stop")
        self._raise(resp)

    def list_sandboxes(self) -> list[dict[str, Any]]:
        """返回所有 sandbox 的信息列表。"""
        resp = self._http.get("/api/v1/sandboxes")
        self._raise(resp)
        return resp.json().get("sandboxes") or []

    # ------------------------------------------------------------------ #
    # Context manager                                                      #
    # ------------------------------------------------------------------ #

    @contextmanager
    def sandbox(
        self,
        name: str,
        *,
        region: str = "local",
        provider_type: str = "local",
        instance_type: str = "",
        image_id: str = "",
        auto_delete: bool = True,
    ) -> Generator["SandrPodSandbox", None, None]:  # noqa: F821
        """上下文管理器：自动创建 sandbox，退出时可选删除。

        Example::

            with client.sandbox("agent-run-001") as sb:
                result = sb.execute("ls /workspace")
                print(result.output)
            # sandbox 已被自动删除
        """
        sb = self.create_sandbox(
            name,
            region=region,
            provider_type=provider_type,
            instance_type=instance_type,
            image_id=image_id,
        )
        try:
            yield sb
        finally:
            if auto_delete:
                try:
                    self.delete_sandbox(name)
                except Exception:
                    pass
