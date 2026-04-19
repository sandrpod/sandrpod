"""Example 3: Code Review & Bug Fix

演示通过 SDK upload_files / download_files 与 Agent 协作：
  1. 主机用 upload_files 把含 Bug 的 Python 文件上传到沙箱
  2. Agent 审查代码，找出 Bug 并修复
  3. Agent 运行测试确认修复正确
  4. 主机用 download_files 取回修复后的文件

含有的两个 Bug：
  - find_max 初始值写成 0，对全负数列表返回错误结果
  - divide 没有除零保护，会抛出 ZeroDivisionError

依赖：
    pip install langchain-sandrpod deepagents langchain-openai

用法：
    export SANDRPOD_API_URL=http://localhost:8080   # 可选，默认 localhost:8080
    export OPENAI_API_KEY=<your-key>
    python examples/03_code_review_and_fix.py
"""

import os

from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

from langchain_sandrpod import SandrPodClient

# ── 含 Bug 的代码 ────────────────────────────────────────────────────────────
BUGGY_CODE = '''\
def find_max(numbers):
    """返回列表中的最大值。"""
    max_val = 0          # Bug 1: 初始值应为 float('-inf')，否则全负数列表返回错误
    for n in numbers:
        if n > max_val:
            max_val = n
    return max_val


def divide(a, b):
    """返回 a / b。"""
    return a / b         # Bug 2: 缺少除零检查，b=0 时抛出 ZeroDivisionError


def add(a, b):
    return a + b


def subtract(a, b):
    return a - b
'''

# ── 配置 ────────────────────────────────────────────────────────────────────
API_URL = os.environ.get("SANDRPOD_API_URL", "http://localhost:8080")
SANDBOX_NAME = os.environ.get("SANDRPOD_SANDBOX", "my-sandbox")

model = ChatOpenAI(
    model=os.environ.get("MODEL_NAME", "gpt-4o-mini"),
    base_url=os.environ.get("OPENAI_BASE_URL"),
    api_key=os.environ.get("OPENAI_API_KEY"),
    temperature=0,
)

# ── 上传含 Bug 的文件 ─────────────────────────────────────────────────────────
client = SandrPodClient(api_url=API_URL)
sb = client.get_sandbox(SANDBOX_NAME)

print("=== 上传含 Bug 的代码 ===")
results = sb.upload_files([("/workspace/math_utils.py", BUGGY_CODE.encode())])
for r in results:
    status = "✓" if r.error is None else f"✗ {r.error}"
    print(f"  {r.path}: {status}")

# ── 让 Agent 审查并修复 ───────────────────────────────────────────────────────
agent = create_deep_agent(model=model, backend=sb)

result = agent.invoke({
    "messages": [{
        "role": "user",
        "content": (
            "请审查沙箱里 /workspace/math_utils.py 的代码，"
            "找出所有 Bug 并修复。修复后请写一段测试代码运行，"
            "确认 find_max([-3, -1, -5]) 返回 -1，"
            "以及 divide(10, 0) 不抛出异常（返回合理的错误提示或 None）。"
        ),
    }]
})

print("\n=== Agent 最终回复 ===")
print(result["messages"][-1].content)

# ── 取回修复后的文件 ──────────────────────────────────────────────────────────
print("\n=== 下载修复后的文件 ===")
downloads = sb.download_files(["/workspace/math_utils.py"])
for dl in downloads:
    if dl.error:
        print(f"  下载失败: {dl.error}")
    else:
        print(f"  {dl.path} ({len(dl.content)} bytes)")
        print("\n--- 修复后的代码 ---")
        print(dl.content.decode())
