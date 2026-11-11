# 快照与分叉

> 快照把一个沙箱定格——它的 `/workspace` 数据和镜像层——分叉则从它启动一个新沙箱。

## 做一个

```bash
curl -s -X POST "$API/api/v1/sandboxes/box/snapshot?image=sandrpod-snapshot/box:v1" \
  -H "Authorization: Bearer $TOKEN"
# → {"image":"registry.example.com/sandrpod-snapshots/box:v1","sandbox":"box"}
```

控制台里：行的 ⋯ 菜单 → **快照成镜像**；用 **快照** 按钮查看列表；在创建抽屉里用
**从快照创建** 分叉。

## 快照里有什么

- **镜像层**（`docker commit` 捕获的东西：装过的包、`/workspace` 之外的文件）
- **`/workspace` 卷**，`docker commit` *不* 捕获它——引擎在创建分叉时复制过来

## 存在哪里

worker 上配置了 `SANDRPOD_REGISTRY` 时，快照会被打上
`<registry>/sandrpod-snapshots/<沙箱>:<tag>` 并**推送上去**，任何 worker 都能从它分叉。
没有 registry 时它只留在做快照的那台机器上，分叉也只能在那里进行——那控制台会把节点锁定。

平台自身的部署在 Caddy 后面跑一个小的 `registry:2`；接线方式见[部署](deploy.md)。

## 命名

默认 tag 是 `latest`，所以同一沙箱在同一 worker 上做两次快照会互相覆盖。
需要保留历史时传一个不同的 tag（时间戳、标签）。

## 删除

```bash
curl -s -X DELETE "$API/api/v1/snapshots?image=sandrpod-snapshot/box:v1" \
  -H "Authorization: Bearer $TOKEN"
# → {"removed":1}
```

会删掉该 tag 在所有 worker 上的副本。worker 会拒绝删除不是自己提交的镜像，docker 会拒绝
删除正在使用的镜像。
