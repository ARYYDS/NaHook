# 00-总体架构 design：LT 上 Caddy + nahook + NewAPI 的整体蓝图

## §0 范围与边界

### 范围

本专题在 LT 宿主机（`lt.xhub.chat`）上涉及的全部部件：

- Caddy 入口；
- nahook 的生产实例和测试实例；
- 生产 NewAPI（先从 h3 关掉护栏，再换官方镜像）；
- 测试 NewAPI（官方镜像 + SQLite）；
- 测试用的假上游 mockrelay。

还包括：端到端链路、端口和资源预算、分阶段上线流程、跨模块不变量。

### 不在范围

| 内容 | 去处 |
|---|---|
| nahook 内部的算法和模块 | `01-nahook/design.md` |
| 自动调度（切渠道、探测、别名计费） | `渠道监控/自动调度/`（知识库） |
| NewAPI 源码 | 不改 |
| 外部 MySQL、Redis | 不碰；只在第 3 阶段前做备份（§1） |

### 前置约束

| 约束 | 出处 |
|---|---|
| LT：x86_64，Debian 13，systemd 257，Docker 29.8.2，docker0 网关 172.17.0.1，没装 Go，内存 15GB，磁盘剩余 162GB | 2026-10-20 实测 |
| 生产 NewAPI 现状：`ghcr.io/alterem/new-api-h3:latest`（`main-20261020-38e69e3`）；护栏打开；编排文件 `/root/container/luna-task/compose.yaml` | `渠道监控/hook-without-fork.md` §1；实测 |
| 生产 NewAPI 用外部 MySQL（`SQL_DSN`）和同机 Redis | 实测（只看了变量名和前缀） |
| Caddy 2.6.2，生产站点配置了 `flush_interval -1` | 实测 Caddyfile |

## §1 核心流程

### 1.1 分阶段上线（每一步都能单独回退）

```text
阶段 0  测试环境（甲）：全部审计项通过
   │    生产：不动
   ▼
阶段 1  部署 nahook 生产实例 + Caddy 生产路由（护栏仍然开着）
   │    nahook 只能看到 HTTP 429 → 全部透传，相当于空跑
   │    验证：转发正确、延迟不变；停掉 nahook 后 Caddy 能回落
   │    回退：去掉 Caddy 路由 → systemctl reload caddy
   ▼
阶段 2  h3 改设 RESPONSES_STREAM_STARTUP_GUARD_ENABLED=false → 重建容器
   │    nahook 开始拦截流内限流
   │    验证：journal 里出现 intercept / release / give_up；NewAPI 日志正常
   │    回退：改回 true → 重建容器
   ▼
阶段 3  镜像换成官方 calciumion/new-api（固定 digest）→ 重建容器
        先备份 MySQL：会执行 AutoMigrate（§8）
        验证：同阶段 2，外加面板功能冒烟
        回退：改回 h3 镜像这一行 → 重建容器
```

顺序不能颠倒：**护栏必须在 nahook 已经接管 Caddy 路由之后才能关。** 反过来的话，有一段时间客户端会直接收到 SSE 限流错误，没有任何拦截。

每个生产阶段在执行前都要用户明确同意。

### 1.2 端到端请求（阶段 2 和 3 的稳态）

```text
客户端 ─HTTPS─► Caddy ─@stream_relay─► nahook :19080 ─HTTP─► NewAPI :3000 ─HTTPS─► 中转站
                  │ 其他路径 ──────────────────────────► NewAPI :3000
                  │ nahook 连不上 ─────────────────────► NewAPI :3000（lb_policy first）
```

## §2 架构

### 部署图

```text
┌──────────────────────────────── LT 宿主机 ──────────────────────────────────────────┐
│ 生产                                                                                 │
│   Caddy（systemd）lt.xhub.chat :443                                                  │
│     ├─ @stream_relay → 127.0.0.1:19080（nahook）, localhost:3000（回落）               │
│     └─ 其他 → localhost:3000                                                          │
│   nahook.service       127.0.0.1:19080   /run/nahook/ctl.sock   /etc/nahook/config.json│
│   luna-task-master     0.0.0.0:3000（容器）→ 外部 MySQL；redis_luna-task（容器网络内） │
│                                                                                      │
│ 测试（甲）                                                                            │
│   Caddy 测试站点  http://127.0.0.1:19443（bind 127.0.0.1）                              │
│     ├─ @stream_relay → 127.0.0.1:19081（nahook-test）, 127.0.0.1:3001（回落）            │
│     └─ 其他 → 127.0.0.1:3001                                                          │
│   nahook-test.service  127.0.0.1:19081   /run/nahook-test/ctl.sock  /etc/nahook/test.json│
│   nahook-test-newapi   127.0.0.1:3001 → 容器 :3000；SQLite /root/container/nahook-test/data│
│        └─ 测试渠道 Base URL http://host.docker.internal:18080                            │
│   mockrelay.service    172.17.0.1:18080（只在 docker0 上监听）                          │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

### 进程表

| 进程 | 管理方式 | 监听 | 依赖 |
|---|---|---|---|
| caddy | systemd（已有） | :80、:443、127.0.0.1:19443（测试） | nahook、NewAPI |
| nahook | `nahook.service` | 127.0.0.1:19080；`/run/nahook/ctl.sock` | 生产 NewAPI |
| nahook-test | `nahook-test.service` | 127.0.0.1:19081；`/run/nahook-test/ctl.sock` | 测试 NewAPI |
| luna-task-master | docker compose（已有） | 0.0.0.0:3000 | MySQL、Redis |
| nahook-test-newapi | `docker run`，restart=unless-stopped | 127.0.0.1:3001 | SQLite、mockrelay |
| mockrelay | `mockrelay.service` | 172.17.0.1:18080 | 无 |

### 分层与依赖方向

```text
入口层（Caddy） → 拦截层（nahook） → 网关层（NewAPI） → 上游（中转站 / mockrelay）
```

依赖只能从左往右；nahook 不反向调用 Caddy；NewAPI 不知道 nahook 的存在。控制面（`nahookctl`）只连自己那个实例的 unix socket。

## §3 管线与时序

### 3.1 生产稳态

见 `01-nahook/design.md` §3.1–3.3。

### 3.2 nahook 进程挂了

```text
Caddy ─dial 127.0.0.1:19080 被拒─► 立即换下一个上游（连接失败时 POST 也一定会重试：Caddy reverse_proxy 文档 lb_retry_match 一节）
      ─► NewAPI :3000 ─► 官方原样行为
health_interval 10s 内把 nahook 标记为不健康 → 之后的请求直接走 NewAPI；nahook 恢复后自动回来
```

### 3.3 测试链路

```text
curl ─► 127.0.0.1:19443（Caddy 测试站点）─► nahook-test :19081 ─► 测试 NewAPI :3001
     ─► mockrelay 172.17.0.1:18080（用 /__mode 设定的脚本：rate_limit、ok、hold…）
```

## §4 并发、共享资源与预算

| 资源 | 预算 / 现状 | 出处 |
|---|---|---|
| 端口 | 19080、19081、3001、18080、19443 都空闲 | 2026-10-20 `ss -ltn` |
| 临时端口 | 3002：h3 对照实例，只给 T-F1 用，测完就删 | `01-nahook/audit.md` §2 |
| nahook 缓存的请求体 | 每个被拦截的请求最多 64MiB，最坏情况是 并发拦截数 × 64MiB | `01-nahook/design.md` §5；实际请求体大小和并发数**待实测**（LT 当天的请求约 19K token，远小于上限） |
| 内存 | 可用 14GB | 实测 |
| 发往 NewAPI 的空闲连接 | 最多 2000 | 和 LT Caddyfile 的 `keepalive_idle_conns 2000` 一致 |
| 日志量 | 每次拦截约 3 行（intercept、wait、release 或 give_up） | `01-nahook/design.md` §5 |
| 上游尝试次数 | 每个客户端请求 ≤ 1 + `retry.max` = 4 | `retry.max` = 3（用户拍板） |

## §5 数据模型与存储

| 内容 | 位置 |
|---|---|
| 生产 Caddyfile | `/etc/caddy/Caddyfile`（改之前先备份成 `Caddyfile.bak.<时间戳>`） |
| 生产编排 | `/root/container/luna-task/compose.yaml`（阶段 2 改环境变量，阶段 3 改镜像；改之前先备份成 `compose.yaml.bak.<时间戳>`，沿用现有命名） |
| nahook 生产配置 / 测试配置 | `/etc/nahook/config.json`、`/etc/nahook/test.json` |
| systemd 单元 | `/etc/systemd/system/{nahook,nahook-test,mockrelay}.service` |
| 测试 NewAPI 数据 | `/root/container/nahook-test/data`（SQLite） |
| 二进制 | `/usr/local/bin/{nahook,nahookctl,mockrelay}` |

## §6 模块细节

### 6.1 Caddy 生产路由（阶段 1 加入；在 `lt.xhub.chat` 站点内）

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

- `flush_interval` 写不写，按 T-C1/T-C2 的结果定（`01-nahook/audit.md` §5 K1）。
- 除 `lb_policy` 外，其余参数沿用现有生产配置。
- nahook 会把 `/api/status` 透传给 NewAPI，所以健康检查对两个上游都有效。

### 6.2 Caddy 测试站点

```caddy
http://127.0.0.1:19443 {
	bind 127.0.0.1
	@stream_relay {
		method POST
		path /v1/responses /v1/chat/completions
	}
	handle @stream_relay {
		reverse_proxy 127.0.0.1:19081 127.0.0.1:3001 {
			lb_policy       first
			lb_try_duration 5s
			flush_interval  -1        # T-C1 用；跑 T-C2 时删掉这一行再 reload
		}
	}
	handle {
		reverse_proxy 127.0.0.1:3001
	}
}
```

### 6.3 测试 NewAPI

```bash
docker run -d --name nahook-test-newapi --restart unless-stopped -p 127.0.0.1:3001:3000 --add-host host.docker.internal:host-gateway -v /root/container/nahook-test/data:/data calciumion/new-api@sha256:<固定 digest>
```

- 一定要用 `-p 127.0.0.1:3001:3000`：NewAPI 监听的是 `":"+port`，也就是所有网卡（`NEWAPI/main.go:209-214`）。
- 初始化由用户在 `http://127.0.0.1:3001` 完成：建 root 账号、打开自用模式（免去配置模型价格）、添加 OpenAI 类型渠道（Base URL `http://host.docker.internal:18080`，模型 `gpt-6-astra`，密钥随便填）、建一个测试令牌。

### 6.4 systemd 单元（nahook）

```ini
[Unit]
Description=nahook
After=network-online.target
[Service]
ExecStart=/usr/local/bin/nahook -config /etc/nahook/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RuntimeDirectory=nahook
RuntimeDirectoryMode=0700
[Install]
WantedBy=multi-user.target
```

`nahook-test.service` 只是把 `-config` 换成 `/etc/nahook/test.json`、`RuntimeDirectory` 换成 `nahook-test`。

## §7 技术栈与依赖

| 部件 | 版本 | 出处 |
|---|---|---|
| Caddy | 2.6.2（不升级） | 实测 |
| nahook | Go，只用标准库；源码在 `ARYYDS/NaHook`，在 Win11 的 `O:\NaHook` 上构建 | `01-nahook/design.md` §7 |
| 生产 NewAPI | 阶段 2：h3 `38e69e300`；阶段 3：官方镜像，固定 digest，对应源码 `206e0fa5c` 或者上线当天的版本 | `NEWAPI` 克隆 |
| 测试 NewAPI | 官方镜像，和阶段 3 用同一个 digest | — |

## §8 易错点

| 错误写法 | 后果 | 正确写法与依据 |
|---|---|---|
| 先关护栏，再部署 nahook | 中间这段时间客户端直接收到 SSE 限流错误 | 按 §1.1 的顺序 |
| 测试 NewAPI 用 `-p 3001:3000` | 监听 0.0.0.0，暴露到公网 | `-p 127.0.0.1:3001:3000`（§6.3） |
| mockrelay 监听 127.0.0.1 | 容器里访问不到宿主机的回环地址 | 监听 docker0 网关 172.17.0.1，配合 `host-gateway` |
| 阶段 3 不备份就换镜像 | 官方相比 h3 基线（`32e587406..206e0fa5c`，12 个提交）改了 `model/usedata.go`，新增了列（例如 `cache_read_tokens`），启动时会 AutoMigrate；回退时库里会多出这些列 | 先备份 MySQL；回退兼容性在 `01-nahook/audit.md` §5 K4 核查 |
| 改完 Caddyfile 用 restart | 现有连接会被断开 | `systemctl reload caddy`（优雅重载） |
| 3000 端口对公网开放 | 客户端直连就绕过了 nahook | 在安全组或防火墙上关掉 3000 的公网入口（`01-nahook/audit.md` §5 K5） |
| 测试流量打到生产 NewAPI | 污染生产日志和计费 | 测试只走 19443 → 19081 → 3001 这条链路 |
