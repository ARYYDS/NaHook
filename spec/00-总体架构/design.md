# 00-总体架构 design：测试机 LT 与生产主机上的 Caddy + nahook + NewAPI

## §0 范围与边界

### 范围

两台主机，部署结构相同：

| 主机 | 用途 | 部件 |
|---|---|---|
| 测试机 LT（`lt.xhub.chat`，43.162.107.217） | 2026-10-21 起和生产隔离，只做测试 | Caddy、nahook、NewAPI（官方镜像）、Redis、mockrelay；临时的 h3 对照实例（只给 T-F1 用） |
| 生产主机（是哪台待 K8 确认） | 线上服务 | Caddy、nahook（阶段 1 起）、NewAPI（先在 h3 上关护栏，再换官方镜像）、Redis |

本专题还包括：两台主机上的端到端链路、端口和资源预算、先测试后上线的顺序、跨模块不变量。

### 不在范围

| 内容 | 去处 |
|---|---|
| nahook 内部的算法和模块 | `01-nahook/design.md` |
| 自动调度（切渠道、探测、别名计费） | `渠道监控/自动调度/`（知识库） |
| NewAPI 源码 | 不改 |
| 外部 MySQL、Redis | 不碰；只在生产阶段 3 之前做备份（§1.2） |

### 前置约束

| 约束 | 出处 |
|---|---|
| LT：x86_64，Debian 13，systemd 257，Docker 29.8.2，没装 Go，内存 15GB。docker0 是 `172.17.0.1/16`；NewAPI 容器所在的网络 `luna-task_luna-task-network` 是 `172.18.0.0/16`，容器能直接连到 `172.17.0.1`（连没有程序监听的 18080 时立即返回 connection refused；宿主机防火墙链 `YJ-FIREWALL-INPUT` 为空） | 2026-10-20、10-21 实测 |
| LT 的 NewAPI：官方 `calciumion/new-api@sha256:6c63c7ee9cd5bf7880df3984e99dd3ef07992d7a97538cfb2719adf9d87bc3f9`（`v1.0.0-rc.45`，源码 `6cf0d90b`），没有护栏；编排文件 `/root/container/luna-task/compose.yaml`；外部 MySQL 是 TDSQL-C 5.7（`10.0.0.4:3306/luna`），和生产隔离；同机 Redis | 2026-10-21 实测；数据库隔离由用户确认 |
| LT 的 Caddy：2.6.2（Debian 包 `2.6.2-12+deb13u1`），`lt.xhub.chat` 站点配置了 `flush_interval -1` | 实测 |
| 生产主机：和 LT 在 2026-10-21 之前的状态一致：NewAPI 是 h3 `38e69e3` 并打开护栏，Caddy 配置相同，外部 MySQL，同机 Redis | 用户告知，**未核实**（`01-nahook/audit.md` §5 K8） |

## §1 核心流程

### 1.1 先在 LT 测，再上生产

```text
测试机 LT                                              生产主机
T0  Caddy 断开实验（临时站点，不碰主站点）
T9  搭测试环境：mockrelay、测试渠道、nahook、Caddy 路由
T10 跑完审计清单（T-C、T-F）
 │  G2 通过
 ▼
                                                      K8：确认是哪台主机，核对基线和 LT 改动前一致
                                                      阶段 1 → 阶段 2 → 阶段 3（§1.2）
```

- LT 上的 NewAPI 已在 2026-10-21 换成官方 rc.45，相当于在 LT 上提前做了一遍阶段 3 的正向操作。LT 上已经没有护栏，T9 之后 nahook 在 LT 上直接拦截。
- 任何新的变更（nahook 新版本、Caddy 路由改动、NewAPI 换版本）都先在 LT 上做完并通过 T10 的相关审计项，再上生产。

### 1.2 生产分阶段上线（每一步都能单独回退）

```text
阶段 1  部署 nahook + Caddy 路由（护栏仍然开着）
   │    nahook 只能看到 HTTP 429 → 全部透传，相当于空跑
   │    验证：转发正确、延迟不变；停掉 nahook 后 Caddy 能回落
   │    回退：去掉 Caddy 路由 → systemctl reload caddy
   ▼
阶段 2  h3 改设 RESPONSES_STREAM_STARTUP_GUARD_ENABLED=false → 重建容器
   │    nahook 开始拦截流内限流
   │    验证：journal 里出现 intercept / release / give_up；NewAPI 日志正常
   │    回退：改回 true → 重建容器
   ▼
阶段 3  镜像换成和 LT 相同 digest 的官方镜像，删掉护栏开关 → 重建容器
        先备份 MySQL（用户操作）
        验证：同阶段 2，外加面板功能冒烟
        回退：改回 h3 镜像（固定 digest）→ 重建容器
```

顺序不能颠倒：**护栏必须在 nahook 已经接管 Caddy 路由之后才能关。** 反过来的话，有一段时间客户端会直接收到 SSE 限流错误，没有任何拦截。

每个生产阶段在执行前都要用户明确同意。

### 1.3 端到端请求（两台主机的稳态）

```text
客户端 ─HTTPS─► Caddy ─@stream_relay─► nahook :19080 ─HTTP─► NewAPI :3000 ─HTTPS─► 上游渠道
                  │ 其他路径 ──────────────────────────► NewAPI :3000
                  │ nahook 连不上 ─────────────────────► NewAPI :3000（lb_policy first）
```

LT 上测试令牌对应的上游渠道是 mockrelay（§6.2）。

## §2 架构

### 部署图

```text
┌──────────────────────────── 测试机 LT（lt.xhub.chat）──────────────────────────────┐
│ Caddy（systemd）:443                                                                │
│   ├─ @stream_relay → 127.0.0.1:19080（nahook）, localhost:3000（回落）                │
│   └─ 其他 → localhost:3000                                                          │
│ nahook.service      127.0.0.1:19080   /run/nahook/ctl.sock   /etc/nahook/config.json  │
│ luna-task-master    0.0.0.0:3000（官方 rc.45）→ MySQL 10.0.0.4/luna；Redis（容器网络内）│
│   └─ 测试渠道（独立分组）Base URL http://172.17.0.1:18080 ──┐                          │
│ mockrelay.service   172.17.0.1:18080（只在 docker0 上监听）◄─┘                         │
│ 临时：h3 对照实例   127.0.0.1:3002（只给 T-F1，测完就删）                               │
└────────────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────── 生产主机（K8 待确认）──────────────────────────────────┐
│ Caddy :443                                                                          │
│   ├─ @stream_relay → 127.0.0.1:19080（nahook，阶段 1 起）, localhost:3000（回落）      │
│   └─ 其他 → localhost:3000                                                          │
│ nahook.service      127.0.0.1:19080   /run/nahook/ctl.sock   /etc/nahook/config.json  │
│ NewAPI 容器 :3000   h3（护栏开）→ 阶段 2 关护栏 → 阶段 3 官方镜像；外部 MySQL；Redis    │
└────────────────────────────────────────────────────────────────────────────────────┘
```

### 进程表

| 主机 | 进程 | 管理方式 | 监听 | 依赖 |
|---|---|---|---|---|
| 两台 | caddy | systemd（已有） | :80、:443 | nahook、NewAPI |
| 两台 | nahook | `nahook.service` | 127.0.0.1:19080；`/run/nahook/ctl.sock` | 本机 NewAPI |
| 两台 | NewAPI | docker compose（已有） | 0.0.0.0:3000 | MySQL、Redis |
| LT | mockrelay | `mockrelay.service` | 172.17.0.1:18080 | 无 |
| LT | h3 对照实例（临时） | `docker run`，测完删除 | 127.0.0.1:3002 | SQLite、mockrelay |

### 分层与依赖方向

```text
入口层（Caddy） → 拦截层（nahook） → 网关层（NewAPI） → 上游（中转站 / mockrelay）
```

依赖只能从左往右；nahook 不反向调用 Caddy；NewAPI 不知道 nahook 的存在。控制面（`nahookctl`）只连本机 nahook 的 unix socket。两台主机之间没有任何连接。

## §3 管线与时序

### 3.1 稳态

见 `01-nahook/design.md` §3.1–3.3。

### 3.2 nahook 进程挂了

```text
Caddy ─dial 127.0.0.1:19080 被拒─► 立即换下一个上游（连接失败时 POST 也一定会重试：Caddy reverse_proxy 文档 lb_retry_match 一节）
      ─► NewAPI :3000 ─► 官方原样行为
health_interval 10s 内把 nahook 标记为不健康 → 之后的请求直接走 NewAPI；nahook 恢复后自动回来
```

### 3.3 LT 上的测试链路

```text
curl（测试令牌）─HTTPS─► lt.xhub.chat（Caddy）─► nahook :19080 ─► NewAPI :3000
   ─► 测试渠道 ─► mockrelay 172.17.0.1:18080（用 /__mode 设定的脚本：rate_limit、ok、hold…）
```

测试令牌只能路由到测试渠道（§6.2），LT 上的其他渠道不受测试影响。

## §4 并发、共享资源与预算

| 资源 | 预算 / 现状 | 出处 |
|---|---|---|
| LT 端口 | 19080、18080 空闲 | 2026-10-21 `ss -ltn` |
| LT 临时端口 | 3002：h3 对照实例，只给 T-F1 用，测完就删 | `01-nahook/audit.md` §2 |
| 生产主机端口 | 19080 是否空闲 | 待 K8 |
| nahook 缓存的请求体 | 每个被拦截的请求最多 64MiB，最坏情况是 并发拦截数 × 64MiB | `01-nahook/design.md` §5；实际请求体大小和并发数**待实测**（LT 当天的请求约 19K token，远小于上限） |
| 内存 | LT 可用 14GB；生产主机待 K8 | 实测 |
| 发往 NewAPI 的空闲连接 | 最多 2000 | 和 LT Caddyfile 的 `keepalive_idle_conns 2000` 一致 |
| 日志量 | 每次拦截约 3 行（intercept、wait、release 或 give_up） | `01-nahook/design.md` §5 |
| 上游尝试次数 | 每个客户端请求 ≤ 1 + `retry.max` = 4 | `retry.max` = 3（用户拍板） |

## §5 数据模型与存储

| 内容 | LT | 生产主机 |
|---|---|---|
| Caddyfile | `/etc/caddy/Caddyfile`；改之前先备份成 `Caddyfile.bak.<时间戳>` | 同左（K8 核对） |
| NewAPI 编排 | `/root/container/luna-task/compose.yaml`；2026-10-21 已换成官方镜像，换之前的备份是 `compose.yaml.bak.20261021131926` | 同路径（K8 核对）；阶段 2 改环境变量，阶段 3 改镜像，改之前先备份成 `compose.yaml.bak.<时间戳>` |
| nahook 配置 | `/etc/nahook/config.json` | 同左 |
| systemd 单元 | `/etc/systemd/system/{nahook,mockrelay}.service` | `/etc/systemd/system/nahook.service` |
| 二进制 | `/usr/local/bin/{nahook,nahookctl,mockrelay}` | `/usr/local/bin/{nahook,nahookctl}` |
| 测试渠道、测试令牌 | 存在 LT 的 MySQL `luna` 里，由用户在面板里创建（§6.2） | 无 |
| h3 对照实例的数据 | `/root/container/nahook-h3-ref/data`（SQLite，测完由用户删除） | 无 |

## §6 模块细节

### 6.1 Caddy 的 nahook 路由（两台主机相同；写在主站点内）

```caddy
	@stream_relay {
		method POST
		path /v1/responses /v1/chat/completions
	}
	handle @stream_relay {
		reverse_proxy 127.0.0.1:19080 localhost:3000 {
			lb_policy       first
			lb_try_duration 5s
			health_uri      /api/status
			health_interval 10s
			health_timeout  5s
			transport http {
				keepalive               10m
				keepalive_idle_conns    2000
				response_header_timeout 30m
				dial_timeout            5s
			}
			header_up X-Real-IP {remote_host}
		}
	}
	handle {
		reverse_proxy localhost:3000 { …原配置原样… }
	}
```

- `flush_interval` 写不写，先按 T0 的结论定（`00-总体架构/tasks.md` G1），再由 T-C1、T-C2 在 LT 的完整链路上确认（`01-nahook/audit.md` §5 K1）。
- 除 `lb_policy` 外，其余参数沿用各自主站点现有的配置。
- nahook 会把 `/api/status` 透传给 NewAPI，所以健康检查对两个上游都有效。
- 在 LT 上由 T9 加入；在生产主机上由阶段 1（T11）加入。

### 6.2 LT 的测试渠道和测试令牌（用户在面板里创建）

| 项 | 要求 |
|---|---|
| 测试渠道 | OpenAI 类型；Base URL `http://172.17.0.1:18080`；模型 `gpt-6-astra`；密钥随意；关闭自动禁用 |
| 隔离 | 测试渠道和测试令牌放在一个只给测试用的分组里，测试令牌只能路由到测试渠道。T9 验证：用测试令牌请求后，mockrelay 的 `/__log` 有记录，其他渠道没有新的调用 |
| 重试 | `RetryTimes` 保持 LT 现值；T-F7、T-F17 要求设为 0 时，测完改回原值 |
| 计费 | 测试令牌所在分组对 `gpt-6-astra` 的定价（`01-nahook/audit.md` §5 K6） |

### 6.3 h3 对照实例（只给 T-F1，测完就删）

```bash
docker run -d --name nahook-h3-ref -p 127.0.0.1:3002:3000 -e RESPONSES_STREAM_STARTUP_GUARD_ENABLED=true -v /root/container/nahook-h3-ref/data:/data ghcr.io/alterem/new-api-h3@sha256:6249f47587fc2b6357eec5daead3afce4eae9b72410abc87a34af9077a0f4318
```

- 一定要用 `-p 127.0.0.1:3002:3000`：NewAPI 监听的是 `":"+port`，也就是所有网卡（`NEWAPI/main.go:209-214`）。
- 这个容器在默认网桥上，`172.17.0.1` 就是它的网关，渠道的 Base URL 同样写 `http://172.17.0.1:18080`。
- 初始化由用户在 `http://127.0.0.1:3002` 完成：建 root 账号、加测试渠道、建测试令牌、`RetryTimes` 设为 0。
- 测完由用户执行 `docker rm -f nahook-h3-ref`，并删除数据目录。

### 6.4 systemd 单元

```ini
[Unit]
Description=nahook
After=network-online.target
[Service]
ExecStart=/usr/local/bin/nahook -config /etc/nahook/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
UMask=0077
RuntimeDirectory=nahook
RuntimeDirectoryMode=0700
[Install]
WantedBy=multi-user.target
```

```ini
[Unit]
Description=mockrelay (NaHook test upstream, LT only)
After=docker.service
Requires=docker.service
[Service]
ExecStart=/usr/local/bin/mockrelay -listen 172.17.0.1:18080
Restart=always
[Install]
WantedBy=multi-user.target
```

`mockrelay` 要等 Docker 把 docker0 建好以后才能绑定 `172.17.0.1`，所以依赖 `docker.service`。

## §7 技术栈与依赖

| 部件 | 版本 | 出处 |
|---|---|---|
| Caddy | 2.6.2（不升级） | 实测 |
| nahook | Go，只用标准库；源码在 `ARYYDS/NaHook`，在 Win11 的 `O:\NaHook` 上构建 | `01-nahook/design.md` §7 |
| LT 的 NewAPI | 官方 `calciumion/new-api@sha256:6c63c7ee9cd5bf7880df3984e99dd3ef07992d7a97538cfb2719adf9d87bc3f9`（`v1.0.0-rc.45`） | 2026-10-21 部署 |
| 生产 NewAPI | 阶段 2：h3 `38e69e3`（`ghcr.io/alterem/new-api-h3@sha256:6249f47587fc2b6357eec5daead3afce4eae9b72410abc87a34af9077a0f4318`）；阶段 3：和 LT 相同的官方 digest。要换成别的官方版本，先在 LT 上重做 K4 核查并通过 T10 的相关审计项 | `01-nahook/audit.md` §5 K3、K4 |
| h3 对照实例 | 和生产阶段 2 相同的 h3 digest | — |

## §8 易错点

| 错误写法 | 后果 | 正确写法与依据 |
|---|---|---|
| 生产上先关护栏，再部署 nahook | 中间这段时间客户端直接收到 SSE 限流错误 | 按 §1.2 的顺序 |
| 新变更直接上生产，没有先在 LT 上测 | 生产成了试验场 | 按 §1.1：先 LT，T10 通过后再上生产 |
| 测试令牌能路由到真实渠道 | 测试请求花掉真实额度，还会污染这些渠道的统计 | 测试渠道和测试令牌放进单独分组，T9 验证隔离（§6.2） |
| mockrelay 监听 127.0.0.1 | 容器里访问不到宿主机的回环地址 | 监听 docker0 网关 172.17.0.1（§0 已实测可达） |
| h3 对照实例用 `-p 3002:3000` | 监听 0.0.0.0，暴露到公网 | `-p 127.0.0.1:3002:3000`（§6.3） |
| 阶段 3 不备份就换镜像，或者换成没在 LT 上验证过的版本 | 回退时可能碰到表结构不兼容 | 先备份 MySQL；用和 LT 相同的 digest。rc.45 比 h3 的官方基线旧 4 个提交，表结构是 h3 的子集，LT 换镜像时启动迁移没有改表（`specaudit.md` §7.11） |
| 改完 Caddyfile 用 restart | 现有连接会被断开 | `systemctl reload caddy`（优雅重载） |
| 生产的 3000 端口对公网开放 | 客户端直连就绕过了 nahook | 在安全组或防火墙上关掉 3000 的公网入口（`01-nahook/audit.md` §5 K5） |
