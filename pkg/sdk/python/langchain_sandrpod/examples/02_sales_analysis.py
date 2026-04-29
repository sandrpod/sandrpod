"""Example 2: Sales Data Analysis

让 Agent 在沙箱里：
  1. 创建一份示例销售 CSV
  2. 用 Python 计算各产品的总销售额、均值、最畅销品
  3. 将分析报告写入 sales_report.txt

依赖：
    pip install langchain-sandrpod deepagents langchain-openai

用法：
    export SANDRPOD_API_URL=http://localhost:8080   # 可选，默认 localhost:8080
    export SANDRPOD_API_TOKEN=<your-token>          # 可选，服务端启用认证时需要
    export OPENAI_API_KEY=<your-key>
    python examples/02_sales_analysis.py
"""

import os

from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

from langchain_sandrpod import SandrPodClient

# ── 配置 ────────────────────────────────────────────────────────────────────
API_URL = os.environ.get("SANDRPOD_API_URL", "http://localhost:8080")
API_TOKEN = os.environ.get("SANDRPOD_API_TOKEN")
SANDBOX_NAME = os.environ.get("SANDRPOD_SANDBOX", "my-sandbox")

model = ChatOpenAI(
    model=os.environ.get("MODEL_NAME", "gpt-4o-mini"),
    base_url=os.environ.get("OPENAI_BASE_URL"),
    api_key=os.environ.get("OPENAI_API_KEY"),
    temperature=0,
)

# ── 运行 ────────────────────────────────────────────────────────────────────
client = SandrPodClient(api_url=API_URL, api_token=API_TOKEN)
sb = client.get_sandbox(SANDBOX_NAME)

agent = create_deep_agent(model=model, backend=sb)

result = agent.invoke({
    "messages": [{
        "role": "user",
        "content": (
            "请在沙箱的 /workspace 目录下完成以下任务：\n"
            "1. 创建一个 sales.csv，包含以下数据：\n"
            "   product,quantity,price\n"
            "   Apple,50,1.2\n"
            "   Banana,30,0.5\n"
            "   Cherry,20,3.0\n"
            "   Apple,40,1.2\n"
            "   Banana,60,0.5\n"
            "   Cherry,10,3.0\n"
            "2. 用 Python 脚本读取 sales.csv，计算：\n"
            "   - 各产品总销售额\n"
            "   - 平均单价\n"
            "   - 最畅销产品（按销量）\n"
            "3. 将分析结果写入 sales_report.txt\n"
            "4. 打印 sales_report.txt 的内容确认"
        ),
    }]
})

print("\n=== Agent 最终回复 ===")
print(result["messages"][-1].content)

# 通过 SDK 直接读取报告验证
report = sb.read("/workspace/sales_report.txt")
print("\n=== sales_report.txt（SDK 直读）===")
print(report)
