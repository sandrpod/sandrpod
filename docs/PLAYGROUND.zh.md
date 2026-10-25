# Playground：手工验证每一层

一份可实际跑的走查：permission gate、OS 沙箱、network namespace、egress 策略、凭据掩码，
每一步都给出应当看到的输出。

它是**手工**的，这是刻意的。自动化测试（`go test ./pkg/egress`、`./pkg/sandbox` 以及
下面需要 `--privileged` 的 Docker 测试）已经证明了机制本身；这一页回答另一个问题：
**拼装起来的产品，在我的机器上，是否真的做到了文档说的那些事。** 本文所有命令都跑过，
"应当看到"里的内容是真实输出，不是设想。

两条轨道：

- **A —— 容器沙箱（poder）**：舰队行为，真实 Docker 网络里的 egress；
- **B —— 员工机（agent）**：完整五层（gate、内核、netns、策略、掩码）跑在特权 Linux
  容器里——在没有笔记本的情况下，这已经是最接近笔记本的东西。

> 全部本地运行、自行清理。不会碰到你真实的 `~/.sandrpod`：B 轨道里的 agent 用的是容器
> 里的 `/root`。

## 0. 前置

```bash
docker --version          # A、B 都要
go version                # 用来构建你要运行的二进制
```

先构建一次：

```bash
mkdir -p /tmp/pg
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/pg/pg-agent-linux ./cmd/agent
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/pg/pg-echo-linux  ./hack/playground/echo
# amd64 机器把 GOARCH 换成 amd64
```

`hack/playground/echo` 是一个百来行的 TLS 服务，会把收到的 `Authorization` 打印出来。
它存在的理由是：验证掩码不该依赖某个第三方回显服务。

## A. 容器沙箱：网络那面墙

### A.1 起控制面

```bash
go run ./cmd/server -port 18080 -db sqlite:/tmp/pg/sandrpod.db
```

应当看到 `API server listening on :18080`。

### A.2 起一个 worker

```bash
go run ./cmd/poder -api-url=http://localhost:18080 -region=local
```

应当看到它注册成功并开始轮询 job。

### A.3 建一个带策略的沙箱

```bash
curl -s -X POST http://127.0.0.1:18080/api/v1/sandboxes \
  -H 'Content-Type: application/json' -d '{
    "name": "walled", "provider": "local",
    "network": {
      "allowed_domains": ["example.com"],
      "denied_domains": ["*.blocked.test"],
      "denied_domain_reasons": {"*.blocked.test": "blocked on purpose by the playground"}
    }
  }'
```

应当看到 job 返回，随后 `walled` 到达 `RUNNING`。它背后，poder 创建了：

- 一个 `Internal: true` 的 docker 网络 `sandrpod-egress-walled`；
- 一个挂在它上面的 `egress-proxy` sidecar；
- 沙箱**只**接在这个内部网络上。

### A.4 验证三个答案

```bash
curl -s -X POST "http://127.0.0.1:18080/api/v1/sandboxes/execute?sandbox=walled" \
  -H 'Content-Type: application/json' \
  -d '{"language":"bash","code":"curl -s -o /dev/null -w \"allow=%{http_code}\\n\" https://example.com/; curl -s -o /dev/null -w \"deny=%{http_code}\\n\" https://blocked.test/; curl -s --noproxy \"*\" -o /dev/null -w \"direct=%{http_code}\\n\" https://example.com/","timeout":30}'
```

应当依次看到：

```
allow=200
deny=000          # 代理对 CONNECT 回了 403；curl 报告隧道失败
direct=000        # 完全没路由：容器在内部网络上
```

deny 用 `-v` 更好读，能看到代理的答复：

```
< HTTP/1.1 403 Forbidden
< X-Deny-Reason: denied_domain
```

body 里是 `denied_domain_reasons` 里操作者写的那句话。

### A.5 清理

```bash
curl -s -X DELETE http://127.0.0.1:18080/api/v1/sandboxes/walled
docker network ls | grep sandrpod            # 什么都没有
docker ps -a | grep sandrpod-egress          # 什么都没有
```

## B. 员工机：完整五层，装在容器里

用真正的 `sandrpod-agent` 跑在特权 Linux 容器里（带 bubblewrap），于是五层同时生效：
permission gate、内核沙箱、network namespace、egress 策略、凭据掩码。

### B.1 上游

```bash
/tmp/pg/pg-echo-linux -gen-ca /tmp/pg/certs          # 若还没有证书
docker network create pg-net
docker run -d --name pg-echo --network pg-net \
  -v /tmp/pg:/pg -w /pg alpine:3.20 \
  sh -c '/pg/pg-echo-linux -addr :8443 -cert /pg/cert.pem -key /pg/key.pem'
docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' pg-echo
# 记下地址，例如 172.22.0.2 —— 下一步要写进 /etc/hosts
```

### B.2 准备 agent 的世界

```bash
mkdir -p /tmp/pg/work /tmp/pg/audit

cat > /tmp/pg/egress.json <<'J'
{
  "allowed_domains": ["echo.local:8443", "example.com"],
  "denied_domains": ["*.blocked.test"],
  "denied_domain_reasons": {"*.blocked.test": "blocked on purpose by the playground"}
}
J

cat > /tmp/pg/agent-run.sh <<'S'
#!/bin/sh
set -e
apk add -q bubblewrap ca-certificates bash curl >/dev/null
cp /pg/ca.pem /usr/local/share/ca-certificates/playground.crt
update-ca-certificates >/dev/null 2>&1          # 代理必须信任我们的上游
echo "<ECHO_IP> echo.local" >> /etc/hosts
mkdir -p /root/.ssh /pg/work/.git/hooks
printf 'PRIVATE-KEY-SECRET\n' > /root/.ssh/id_rsa
printf '#!/bin/sh\necho ok\n' > /pg/work/.git/hooks/pre-commit
printf 'TOP-SECRET\n'         > /pg/work/.git/config
cd /pg/work
exec /pg/pg-agent-linux \
  -api-url=http://host.docker.internal:18080 -name=playground \
  -permission-mode=strict -permission-file=/pg/permissions.json \
  -work-dir=/pg/work \
  -os-sandbox=bwrap \
  -egress-mode=enforce -egress-policy=/pg/egress.json \
  -credential-mask=OPENAI_API_KEY \
  -credential-terminate-ports=443,8443 \
  -audit-dir=/pg/audit
S
sed -i '' "s/<ECHO_IP>/$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' pg-echo)/" /tmp/pg/agent-run.sh
# Linux 上 sed 用 `sed -i`（不带空参数）
```

用 `-permission-mode=strict` 是因为容器里没人能回答同意对话框；这时 gate 会拒绝
`work_dir` 之外的一切。

### B.3 启动

```bash
docker run -d --name pg-agent --privileged --network pg-net \
  --add-host=host.docker.internal:host-gateway \
  -e OPENAI_API_KEY=sk-real-playground-key \
  -v /tmp/pg:/pg alpine:3.20 sh /pg/agent-run.sh
sleep 15 && docker logs pg-agent | tail -20
```

应当看到四层依次报到：

```
egress: credential masking on for [OPENAI_API_KEY]; TLS is terminated for allowed hosts except []
egress: enforcing from /pg/egress.json (unix=/tmp/sandrpod-egress-…/proxy.sock relay=127.0.0.1:3128)
os sandbox: network namespace removed — the relay at 127.0.0.1:3128 is the only route out
os sandbox: bwrap active — work_dir=/pg/work writable, 15 hard-locked path(s) masked, 2 persistence path(s) read-only
os sandbox:   masked ~/.ssh
…
Connected — sandbox "playground" is online
```

那 `2 persistence path(s)` 是 `.git/hooks` 和 `.git/config`，来自 mandatory deny 的遍历。

### B.4 跑检查

先写个辅助脚本，让每条检查只有一行：

```bash
cat > /tmp/pg/exec.sh <<'S'
#!/bin/sh
curl -s -X POST "http://127.0.0.1:18080/api/v1/sandboxes/execute?sandbox=playground" \
  -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"language":"bash","code":sys.argv[1],"timeout":30}))' "$1")" \
| python3 -c 'import json,sys; d=json.load(sys.stdin); print("exit:",d["exit_code"]); print(d["stdout"]); print(d["stderr"],end="")'
S
chmod +x /tmp/pg/exec.sh
```

#### B.4.1 网络：允许、拒绝、以及"没有网络"

```bash
/tmp/pg/exec.sh 'curl -s -o /dev/null -w "allow=%{http_code}\n" https://example.com/;
                 curl -s -o /dev/null -w "deny=%{http_code}\n"  https://blocked.test/;
                 curl -s --noproxy "*" https://example.com/'
```

实测输出：

```
allow=200
deny=000
curl: (6) Could not resolve host: example.com
```

最后一行就是 network namespace：`--noproxy` 之后既没有 DNS 也没有路由，所以失败发生在
解析阶段，而不是"连接被拒"。

#### B.4.2 内核兜底：hardlock 与持久化路径

```bash
/tmp/pg/exec.sh 'cat ~/.ssh/id_rsa; echo "---"; echo pwned >> .git/hooks/pre-commit; echo "---"; wc -c < .git/config'
```

实测输出：

```
cat: /root/.ssh/id_rsa: No such file or directory
---
bash: .git/hooks/pre-commit: Read-only file system
---
11
```

逐条读：

- `~/.ssh` 被换成一个**空 tmpfs**，所以那个文件不只是读不到，而是根本不在；
- `.git/hooks` 只读，写失败；
- `.git/config` 仍可读（11 字节）——mandatory deny 防的是**写**持久化，不是不让读仓库配置。

#### B.4.3 permission gate：结构化的拒绝

gate 是文件 API 走的那条路（相对于 shell 直接系统调用）：

```bash
curl -s "http://127.0.0.1:18080/api/v1/sandboxes/playground/toolbox/files?path=/root/.ssh"
```

实测（HTTP 403）：

```
access denied: /root/.ssh is hard-locked by the operator's policy and cannot be
read or written; do not retry, and ask the user to unlock it in the tray if this
is genuinely needed
```

这句是 `ModelReason`——给 agent 看的。人在机器上看到的是本地化的 `Reason`（对话框与审计里）。

#### B.4.4 凭据掩码：真值到达上游

```bash
/tmp/pg/exec.sh 'echo "sentinel: $OPENAI_API_KEY";
                 curl -s https://echo.local:8443/v1/chat -H "Authorization: Bearer $OPENAI_API_KEY"'
```

实测输出：

```
sentinel: fake_value_3281d413-bb82-44b1-a4b0-99e5f3b956be
{"authorization":"Bearer sk-real-playground-key","host":"echo.local:8443","path":"/v1/chat"}
```

两行、两个事实：沙箱里始终只有 sentinel；到达上游的请求带的是真钥匙。上游自己的日志也这么说：

```bash
docker logs pg-echo | tail -1
# upstream: GET /v1/chat Authorization="Bearer sk-real-playground-key"
```

沙箱的任何输出里都不含 `sk-real-playground-key`。

#### B.4.5 明文拒绝

```bash
/tmp/pg/exec.sh 'curl -s -D - -o /dev/null http://example.com/ -H "Authorization: Bearer $OPENAI_API_KEY" | head -3'
```

实测：

```
HTTP/1.1 403 Forbidden
X-Deny-Reason: masked_credential_cleartext
```

#### B.4.6 审计

```bash
python3 - <<'PY'
import json
for line in open('/tmp/pg/audit/active.log'):
    d = json.loads(line)
    if d.get("source") == "egress":
        print(d.get("host"), d.get("decision"), "terminated=", d.get("terminated"), d.get("credentials"))
PY
```

实测：

```
blocked.test deny terminated= None None
echo.local allow terminated= None None
echo.local allow terminated= True ['OPENAI_API_KEY']
```

第一行 `echo.local` 是隧道建立；第二行记录了替换：连接被终止，`OPENAI_API_KEY` 被放进了
它里面。只记名字，永不记值。

> 凭据使用即使在"允许事件不记日志"时也会记录：**哪个 secret 去了哪个 host**，正是这个
> 特性存在的理由。

### B.5 清理

```bash
docker rm -f pg-agent pg-echo
docker network rm pg-net
rm -rf /tmp/pg
```

## C. 这次走查证明了什么

| 层 | 检查 | 证据 |
|---|---|---|
| permission gate | B.4.3 | 403 + 给模型看的原因 |
| mandatory deny | B.4.2 | `.git/hooks` 报 `Read-only file system` |
| hardlock（内核） | B.4.2 | `~/.ssh` 是空 tmpfs |
| network namespace | B.4.1 | 不走代理就 `Could not resolve host` |
| egress 策略 | B.4.1 / A.4 | 200 / 403 + `X-Deny-Reason` |
| 容器网络墙 | A.4 | sidecar 之外完全没有路由 |
| 凭据掩码 | B.4.4 | 沙箱里是 sentinel，上游是真值 |
| 明文拒绝 | B.4.5 | `masked_credential_cleartext` |
| 审计 | B.4.6 | `terminated=True`、只有凭据名 |

## D. 对不上时

- **`fork/exec bwrap: no such file or directory`** —— 沙箱二进制没有被解析成绝对路径
  （已在 `pkg/sandbox.Resolve` 修复）；重新构建 agent。
- **`curl: (7) Failed to connect to 127.0.0.1:3128`** —— relay 没起来。它由沙箱 wrapper
  启动并等待就绪文件；看命令 stderr 里的 `relay:` 行。
- **上游看到的是 sentinel** —— 那个端口没有被终止。加进
  `-credential-terminate-ports`，或者该主机在 `-credential-terminate-exclude` 里。
- **沙箱里报 `x509: certificate signed by unknown authority`** —— 信任 bundle 的问题。
  检查 `SSL_CERT_FILE` 是否指向 `~/.sandrpod/egress-ca/trust-bundle.pem`，文件是否存在。
- **全都放行、一条拒绝都没有** —— 策略文件没加载。启动时找 `egress: enforcing from …`；
  没有它，egress 是关的。

自动化等价物：`go test ./pkg/egress -run 'Masking|ResignAWS'`、
`go test ./pkg/permission -run 'Mandatory'`，以及 `pkg/sandbox` 里需要 `--privileged`
的 Docker 测试（`Bwrap|Netns`）。
