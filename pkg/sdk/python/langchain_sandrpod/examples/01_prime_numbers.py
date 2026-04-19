"""Example 1: Prime Numbers

让 Agent 在沙箱里写一段 Python 代码，找出 1-100 内的所有质数并运行验证。

依赖：
    pip install langchain-sandrpod deepagents langchain-openai

用法：
    export SANDRPOD_API_URL=http://localhost:8080   # 可选，默认 localhost:8080
    export OPENAI_API_KEY=<your-key>
    python examples/01_prime_numbers.py
"""

import os

from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

from langchain_sandrpod import SandrPodClient

# ── 配置 ────────────────────────────────────────────────────────────────────
API_URL = os.environ.get("SANDRPOD_API_URL", "http://localhost:8080")
SANDBOX_NAME = os.environ.get("SANDRPOD_SANDBOX", "my-sandbox")

model = ChatOpenAI(
    model=os.environ.get("MODEL_NAME", "gpt-4o-mini"),
    base_url=os.environ.get("OPENAI_BASE_URL"),  # None = 官方 OpenAI
    api_key=os.environ.get("OPENAI_API_KEY"),
    temperature=0,
)

# ── 运行 ────────────────────────────────────────────────────────────────────
client = SandrPodClient(api_url=API_URL)
sb = client.get_sandbox(SANDBOX_NAME)

agent = create_deep_agent(model=model, backend=sb)

result = agent.invoke({
    "messages": [{
        "role": "user",
        "content": (
            "在沙箱里写一个 Python 脚本 primes.py，"
            "找出 1 到 100 以内的所有质数并打印出来，然后运行它。"
        ),
    }]
})

print("\n=== Agent 最终回复 ===")
print(result["messages"][-1].content)

# 读取脚本内容验证
script = sb.read("/workspace/primes.py")
print("\n=== primes.py 内容 ===")
print(script)
