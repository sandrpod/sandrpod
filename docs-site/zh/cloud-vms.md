---
title: 用云主机当底座
description: 沙箱可以自己开一台机器 —— 需要什么配置，以及失败时长什么样。
---

# 用云主机当底座

`provider_type: local` 跑在你已经接入的节点上。其它值表示**为这个沙箱开一台云主机**：
创建虚拟机、装 Docker、在上面起 worker、再把沙箱放上去。

```bash
sandrpod-cli create agent-01 --provider aliyun --region cn-hongkong \
  --instance-type ecs.e-c1m2.large --ttl 1800
```

## 部署需要配什么

| 配置 | 阿里云 | AWS |
|---|---|---|
| 凭据 | `ALIYUN_ACCESS_KEY`、`ALIYUN_SECRET_KEY` | `AWS_ACCESS_KEY_ID`、`AWS_SECRET_ACCESS_KEY` |
| 默认地域 | `ALIYUN_REGION` | 按请求传 |
| 交换机 | `SANDRPOD_VM_SUBNET_ID_ALIYUN` | `SANDRPOD_VM_SUBNET_ID_AWS` |
| 安全组 | `SANDRPOD_VM_SECURITY_GROUP_ALIYUN` | `SANDRPOD_VM_SECURITY_GROUP_AWS` |
| 公网 IP | 默认开启；NAT 环境用 `SANDRPOD_VM_PUBLIC_IP_ALIYUN=false` | 同上 |

厂商只有注册成功后才会出现在 `/api/v1/capabilities` 里，控制台据此禁用其余选项 ——
给客户一个服务器根本开不出来的选项，等于让他填完表单再失败。

云主机需要**出网**：它要装 Docker、拉 worker 镜像、连回控制面。不需要任何入站端口，但它是
用 `SANDRPOD_PUBLIC_URL` 连回来的，所以那个地址必须是外人也能访问的地址。

## 创建时发生了什么

```
create → 调厂商接口开机器 → 等 running → 拿公网 IP
       → 云助手：装 Docker（10 分钟预算）
       → 云助手：预拉沙箱镜像
       → 起 worker 容器 → 它拨回控制面
       → 等注册（5 分钟）→ 创建沙箱
```

注册之前的任何一步失败，机器都会被还回去，错误会指明是哪一步。注册之后这台就是机队里的
一个 worker，**删除沙箱不会删掉它**，因为它可能还会被复用。

## 回收

一台机器在 `SANDRPOD_PODER_IDLE_TIMEOUT` 时间内没有承载任何沙箱，就会被终止并删除记录。
在按量付费的云上这不是可选项 —— 没有它，每次创建都会留下一台机器在计费，直到有人发现。

```bash
SANDRPOD_PODER_IDLE_TIMEOUT=20m
```

"空"是按**沙箱记录**判定的，不是按容器计数：记录没了的容器谁也访问不到，让它把机器钉住
就是在白花钱。

## 四个已经修掉的错误

每一个都会让一个"能用"的功能变成"开一台机器然后扔掉"。记在这里，是因为它们的症状指向的
地方都不对。

| 症状 | 真正的原因 |
|---|---|
| `provider aws not found` | 厂商注册写在 `Main()` 里，嵌入构建从不执行它 |
| 机器开出来了，却报 "docker: command not found" | 云助手默认 60 秒超时掐断了 Docker 安装，而超时被当成了成功 |
| worker 起来了，但从不回连 | 告诉机器去连的控制面地址是 `127.0.0.1` |
| 首次创建超时，容器却在之后出现 | 550MB 的沙箱镜像要在 2 分钟预算内拉完。现在在引导阶段预拉 |
| **机器比沙箱活得久** | 闲置回收同样只从 `Main()` 启动；而且孤儿容器被当成"忙" |

## 各厂商的详细文档

每家都有自己的部署文档：凭据、网络对象、以及各自的排错表。阿里云那份最完整，其余结构相同：

- [阿里云](https://github.com/sandrpod/sandrpod/blob/main/docs/ALIYUN_PROVISIONING.md)
- [AWS](https://github.com/sandrpod/sandrpod/blob/main/docs/AWS_PROVISIONING.md) ·
  [Azure](https://github.com/sandrpod/sandrpod/blob/main/docs/AZURE_PROVISIONING.md) ·
  [GCP](https://github.com/sandrpod/sandrpod/blob/main/docs/GCP_PROVISIONING.md) ·
  [腾讯云](https://github.com/sandrpod/sandrpod/blob/main/docs/TENCENT_PROVISIONING.md) ·
  [DigitalOcean](https://github.com/sandrpod/sandrpod/blob/main/docs/DIGITALOCEAN_PROVISIONING.md) ·
  [Hetzner](https://github.com/sandrpod/sandrpod/blob/main/docs/HETZNER_PROVISIONING.md) ·
  [Oracle](https://github.com/sandrpod/sandrpod/blob/main/docs/ORACLE_PROVISIONING.md)

## 怎么验

```bash
sandrpod-cli create canary --provider aliyun --region cn-hongkong \
  --instance-type ecs.e-c1m2.large --ttl 900
sandrpod-cli execute canary "uname -srm; curl -s -o /dev/null -w '%{http_code}' https://example.com"
sandrpod-cli delete canary
# 然后：过了闲置时限，那台机器必须消失
```

一次创建是唯一能同时验证厂商接口、引导、隧道和容器的测试。**最后一行是大家最容易忘的，
也是唯一花钱的那行。**
