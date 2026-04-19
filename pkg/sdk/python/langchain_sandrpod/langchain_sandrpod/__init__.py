"""langchain-sandrpod — SandrPod sandbox integration for Deep Agents.

公开 API::

    from langchain_sandrpod import SandrPodClient, SandrPodSandbox

    # 通过 Client 管理生命周期
    client = SandrPodClient(api_url="http://localhost:18080")
    with client.sandbox("my-sb") as sb:
        result = sb.execute("python3 -c 'print(42)'")
        print(result.output)

    # 直接构造（sandbox 已存在）
    sb = SandrPodSandbox(sandbox_name="existing-sb")
    content = sb.read("/workspace/main.py")
"""

from langchain_sandrpod.client import SandrPodClient
from langchain_sandrpod.sandbox import SandrPodSandbox

__all__ = ["SandrPodClient", "SandrPodSandbox"]
__version__ = "0.1.0"
