# langchain-sandrpod Examples

三个由浅入深的示例，展示如何用 `langchain-sandrpod` + deepagents 让 LLM Agent 在 SandrPod 沙箱里完成真实编程任务。

## 前置条件

1. **运行 SandrPod API Server**

   ```bash
   go run ./cmd/server -port 8080
   ```

2. **连接至少一个沙箱**（Poder 或 sandrpod-agent）

   ```bash
   # 本地 Docker 方式
   go run ./cmd/poder -api-url=http://localhost:8080 -region=local

   # 或本机直连
   go run ./cmd/agent -api-url=http://localhost:8080 -name=my-sandbox
   ```

3. **安装依赖**

   ```bash
   pip install langchain-sandrpod deepagents langchain-openai
   ```

## 配置环境变量

```bash
export SANDRPOD_API_URL=http://localhost:8080   # 默认值，可省略
export SANDRPOD_SANDBOX=my-sandbox              # 沙箱名称
export OPENAI_API_KEY=<your-key>

# 使用 OpenAI 兼容接口（如 DeepSeek）
export OPENAI_BASE_URL=https://api.deepseek.com/v1
export MODEL_NAME=deepseek-chat
```

## 示例列表

### 01 · 质数计算 `01_prime_numbers.py`

Agent 在沙箱里写 `primes.py`，找出 1–100 内的所有质数并运行验证。
演示最基础的"写代码 → 执行 → 读结果"流程。

```bash
python examples/01_prime_numbers.py
```

---

### 02 · 销售数据分析 `02_sales_analysis.py`

Agent 在沙箱里：

1. 创建 `sales.csv`（6 行示例数据）
2. 用 Python 计算各产品总销售额、均值、最畅销品
3. 将报告写入 `sales_report.txt`

主机端通过 `sb.read()` 直接读回报告内容验证。

```bash
python examples/02_sales_analysis.py
```

---

### 03 · 代码审查与修复 `03_code_review_and_fix.py`

完整演示 SDK 文件 I/O + Agent 协作：

1. 主机用 `sb.upload_files()` 上传含 2 个 Bug 的 `math_utils.py`
2. Agent 找出 Bug（`find_max` 初始值错误、`divide` 缺除零保护）并修复
3. Agent 运行测试验证修复
4. 主机用 `sb.download_files()` 取回修复后的文件

```bash
python examples/03_code_review_and_fix.py
```

## 关键 API 速查

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent

client = SandrPodClient(api_url="http://localhost:8080")

# 获取已有沙箱
sb = client.get_sandbox("my-sandbox")

# 或用上下文管理器自动创建/删除
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})

# 直接操作文件（不经过 Agent）
sb.upload_files([("/workspace/data.csv", csv_bytes)])
sb.download_files(["/workspace/output.txt"])
sb.read("/workspace/output.txt")          # → str
sb.execute("ls /workspace")               # → ExecuteResponse
```
