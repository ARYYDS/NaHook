# NaHook

NaHook（下称 nahook）是一个独立的常驻进程，串在 Caddy 和 NewAPI 之间，只解决一件事：

**流式 `/v1/responses`、`/v1/chat/completions` 在启动阶段被上游限流时，NewAPI 会把错误当作 200 SSE 直接透传给客户端。** nahook 在客户端收到任何字节之前识别出这种限流，按冷却门加全抖动等待一段时间后，把原请求原样重新发给 NewAPI（同站重试）；重试用完，返回一个和 NewAPI 格式逐字节一致的 HTTP 429。重试上限用 `nahookctl` 一行命令热修改，下一次判定就生效。

```text
没有 nahook：  客户端 ─► Caddy ─► NewAPI ─► 上游限流 ─► 200 SSE 里夹着一条限流错误（客户端当成功处理）
有 nahook：    客户端 ─► Caddy ─► nahook ─► NewAPI ─► 上游限流
                                    └─ 识别 → 冷却 → 重试 ─► 成功：客户端拿到完整的流，看不到那次限流
                                                       └─► 用完上限：客户端拿到 HTTP 429
```

- 只用 Go 标准库，一个二进制，没有外部依赖。
- 不改 NewAPI 的代码、面板和渠道；nahook 对 NewAPI 来说只是一个普通的 HTTP 客户端。
- 任何拿不准的情况一律原样放行，行为不会比没有 nahook 时更差。
- 工程规格以 `spec/` 为准，本文是面向安装和使用的说明，技术细节和 SPEC 一致。

## 目录

- [1. 安装](#1-安装)
- [2. 配置](#2-配置)
- [3. 重试次数：怎么查、怎么改](#3-重试次数怎么查怎么改)
- [4. 运维](#4-运维)
- [5. 技术逻辑](#5-技术逻辑)
- [6. 与 NewAPI 的接口](#6-与-newapi-的接口)
- [7. 与 Caddy 的接口](#7-与-caddy-的接口)
- [8. 测试与验证](#8-测试与验证)
- [9. 构建与发布](#9-构建与发布)
- [10. 仓库结构](#10-仓库结构)

---

## 1. 安装

### 1.1 前提

| 项 | 要求 |
|---|---|
| 系统 | Linux amd64（在 Debian 13 上验证过），root 权限，systemd |
| NewAPI | 和 nahook 在同一台主机上，监听 `127.0.0.1:3000`（Docker 映射即可）。官方镜像直接可用（验证过 rc.44、rc.45）；h3 必须把 `RESPONSES_STREAM_STARTUP_GUARD_ENABLED` 设为 `false`（护栏开着时 NewAPI 自己回 HTTP 429，nahook 只透传，不起作用） |
| Caddy | 验证过 2.11.4（推荐）和 2.6.2。2.6.2 上站点层不能写 `encode`，见 [7.4](#74-caddy-版本差异) |
| 端口 | `127.0.0.1:19080` 空闲 |
| 客户端 | 必须经过 Caddy。直连 NewAPI 的 3000 端口会绕过 nahook，3000 不要对公网开放 |

### 1.2 下载并校验 Release

Release 的 tag 是 `build-<8位提交号>`，每个 Release 有五个附件：`nahook`、`nahookctl`、`mockrelay`（只用于测试）、`BUILDINFO.txt`、`SHA256SUMS`。下面以 `build-bd6fad86` 为例：

```bash
TAG=build-bd6fad86
mkdir -p /root/nahook-dist/$TAG && cd /root/nahook-dist/$TAG
for f in nahook nahookctl mockrelay BUILDINFO.txt SHA256SUMS; do
  wget -q -O "$f" "https://github.com/ARYYDS/NaHook/releases/download/$TAG/$f"
done
sha256sum -c SHA256SUMS          # 四行都必须是 OK
cat BUILDINFO.txt                # commit、go 版本、构建命令
```

Release 附件不带可执行位，安装时用 `install -m 0755`。旧版本的目录留着，回退时直接用。

### 1.3 安装二进制、配置文件和 systemd 单元

```bash
cd /root/nahook-dist/$TAG
install -m 0755 nahook nahookctl /usr/local/bin/

# 配置和单元取自同一个 tag 的 deploy/ 目录
RAW=https://raw.githubusercontent.com/ARYYDS/NaHook/$TAG/deploy
wget -q -O config.example.json "$RAW/config.example.json"
wget -q -O nahook.service      "$RAW/nahook.service"

install -d -m 0755 /etc/nahook
install -m 0600 config.example.json /etc/nahook/config.json    # 然后按第 2 节修改 match.models
install -m 0644 nahook.service /etc/systemd/system/nahook.service

systemctl daemon-reload
systemctl enable --now nahook
```

`deploy/nahook.service`：

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

`RuntimeDirectory` 会建好 `/run/nahook/`（0700），控制口 socket 是 `/run/nahook/ctl.sock`（0600，只有 root 能用）。`UMask=0077` 让 nahook 改写配置文件时也保持 0600。

### 1.4 检查 nahook 本身

```bash
systemctl is-active nahook                                        # active
ss -ltnp | grep 19080                                             # nahook 监听 127.0.0.1:19080
nahookctl retry get                                               # {"scope":"*","max":3,"source":"global"}
nahookctl stats                                                   # 8 个计数器，gates 为空
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:19080/api/status   # 200（经 nahook 透传到 NewAPI）
```

### 1.5 接入 Caddy

在站点块里加一个只匹配两个流式路径的路由，把 nahook 放在第一个上游、NewAPI 放在第二个；站点原来的 `reverse_proxy` 原样放进兜底的 `handle {}`。以站点 `example.com` 为例：

```caddy
example.com {
	@stream_relay {
		method POST
		path /v1/responses /v1/chat/completions
	}
	handle @stream_relay {
		reverse_proxy 127.0.0.1:19080 localhost:3000 {
			lb_policy       first
			health_uri      /api/status
			health_interval 10s
			health_timeout  5s
			fail_duration   10s
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
		reverse_proxy localhost:3000 {
			# 站点原来的配置原样放在这里，但不要写 flush_interval
		}
	}

	encode zstd gzip          # Caddy ≥ 2.9 可以保留；2.6.2 上删掉
	request_body {
		max_size 512MB
	}
}
```

三条规则，每条都是实测出来的（理由见 [第 7 节](#7-与-caddy-的接口)）：

1. **不写 `flush_interval`**（两个 `handle` 都不写）。SSE 会被 Caddy 自动立即 flush；在 2.6.2 上写了 `-1` 会让客户端断开传不到后端。
2. **写 `fail_duration 10s`**。nahook 连不上时，第一次失败就把它标为不可用，后面的请求直接走 NewAPI。
3. **nahook 路由里不写 `lb_try_duration`**。Caddy 重试 POST 时请求体已经被关闭，重试必然失败，还会把 NewAPI 也标成不可用。

改 Caddyfile 时先备份，校验通过再优雅重载：

```bash
cp -p /etc/caddy/Caddyfile /etc/caddy/Caddyfile.bak.$(date +%Y%m%d%H%M%S)
# 编辑 /etc/caddy/Caddyfile
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile && systemctl reload caddy
```

### 1.6 上线后确认在工作

```bash
journalctl -u nahook -f -o cat     # 出现 intercept → wait → release / give_up 说明正在拦截
nahookctl stats                    # intercepted、rate_limited、released、gave_up 随流量增长
```

### 1.7 回退和卸载

| 目的 | 做法 | 生效 |
|---|---|---|
| 立刻停止拦截，流量全部回到 NewAPI | 删掉 Caddyfile 里的 `@stream_relay` 和 `handle @stream_relay` 两块，`caddy validate` 后 `systemctl reload caddy` | 立即，不断开现有连接 |
| 临时停掉 nahook | `systemctl stop nahook`；Caddy 会自动回落到 NewAPI（停掉那一刻正在发的那个请求收到 502） | 立即 |
| 回到旧版本 | `install -m 0755 /root/nahook-dist/<旧tag>/{nahook,nahookctl} /usr/local/bin/ && systemctl restart nahook` | 立即 |
| 完全卸载 | 先去掉 Caddy 路由并 reload，再 `systemctl disable --now nahook`，删除 `/usr/local/bin/nahook*`、`/etc/nahook/`、`/etc/systemd/system/nahook.service`，`systemctl daemon-reload` | — |

---

## 2. 配置

配置文件 `/etc/nahook/config.json`（样例在 `deploy/config.example.json`）：

```json
{
  "listen": "127.0.0.1:19080",
  "control_socket": "/run/nahook/ctl.sock",
  "newapi": "http://127.0.0.1:3000",
  "body_limit": 67108864,
  "match": { "paths": ["/v1/responses", "/v1/chat/completions"], "models": ["gpt-6-astra"] },
  "retry": { "max": 3, "per_model": {} },
  "gate":  { "base_floor_ms": 50, "streak_cap": 3 }
}
```

| 字段 | 含义 | 规则 | 生效方式 |
|---|---|---|---|
| `listen` | 数据面监听地址，Caddy 连这里 | 非空 | 重启 |
| `control_socket` | 控制口 unix socket 路径 | 非空 | 重启 |
| `newapi` | NewAPI 的地址 | 绝对的 http/https URL | 重启 |
| `body_limit` | 拦截路径上缓存请求体的上限（字节）。超过的请求直接透传，不拦截 | > 0，默认 64MiB | reload |
| `match.paths` | 只有这些路径的 `POST` 才可能被拦截 | — | reload |
| `match.models` | 只有这些模型的流式请求才被拦截。**留空数组 `[]` 表示所有模型**；样例里的 `gpt-6-astra` 是测试用的，上线前改成你的模型列表或 `[]` | — | reload |
| `retry.max` | 全局重试上限：被拦截后最多再试几次 | ≥ 0；0 表示不重试，直接返回代 429 | reload 或 `nahookctl` 立即生效 |
| `retry.per_model` | 按模型覆盖重试上限，例如 `{"gpt-6-astra": 1}` | 值 ≥ 0；键不能为空，也不能是 `*` | 同上 |
| `gate.base_floor_ms` | 冷却窗口的下限（毫秒） | 0–1800000 | reload |
| `gate.streak_cap` | 连续被拦截的封顶次数，窗口最大是 2^(cap-1) 倍基准 | 1–22 | reload |

- 只有同时满足 `POST`、路径在 `match.paths` 里、请求体是 JSON 且 `"stream": true`、模型在 `match.models` 里（或列表为空）、请求体不超过 `body_limit` 的请求才会被拦截；其他请求逐字节透传给 NewAPI。
- 读取配置时不认识的键会直接报错（防止键名写错被静默当成 0）。
- 重新加载：`systemctl reload nahook`（发 SIGHUP）或 `nahookctl reload`。新配置校验失败时 nahook 继续用旧配置，日志里记 `config.reload_failed`，进程不重启。
- `listen`、`control_socket`、`newapi` 改了必须 `systemctl restart nahook`；用 reload 改它们会被拒绝（返回 409），快照不变。
- 部署之后只建议通过 `nahookctl` 改重试上限；nahook 会把修改写回这个文件（临时文件 + fsync + rename），重启后依然生效。

---

## 3. 重试次数：怎么查、怎么改

### 3.1 语义

- **重试上限 N** = 一个客户端请求被识别为限流以后，最多再重试 N 次。每个请求打到 NewAPI 的次数 ≤ 1 + N。
- 默认全局 N = 3。可以给某个模型单独设一个值，模型级的值优先于全局值。
- **N = 0** 表示不重试：第一次识别到限流就返回代 429（相当于只把 SSE 里的限流错误转换成 HTTP 429）。
- 改动在**下一次判定时立即生效**，包括正在等待和重试中的请求；同时写回配置文件，重启后保留。
- nahook 的重试和 NewAPI 面板里的 `RetryTimes` 互不相关：`RetryTimes` 只管 HTTP 层的错误（NewAPI 自己重试），nahook 只管 SSE 里的限流。两者不会叠加（见 [6.4](#64-两层重试怎么分工)）。

### 3.2 用 `nahookctl`（推荐）

`nahookctl` 只能由 root 运行（控制口 socket 是 0600）。

```bash
# 查全局值
nahookctl retry get
{"scope":"*","max":3,"source":"global"}

# 改全局值
nahookctl retry set 5
{"scope":"*","old":3,"new":5}

# 给某个模型单独设
nahookctl retry set 1 --model gpt-6-astra
{"scope":"gpt-6-astra","old":3,"new":1}

# 查某个模型实际生效的值（source 说明来自模型级还是全局）
nahookctl retry get --model gpt-6-astra
{"scope":"gpt-6-astra","max":1,"source":"per_model"}

# 删掉模型级的值，回到全局值
nahookctl retry unset --model gpt-6-astra
{"scope":"gpt-6-astra","removed":true}

# 计数器和冷却门状态
nahookctl stats
{"counters":{"bad_gateway":0,"client_gone":0,"gave_up":0,"intercepted":12,"passthrough":40,"rate_limited":5,"released":11,"retries":4},
 "gates":[{"key":"gpt-6-astra|org-xxxx","until_ms":1792582081838,"streak":0,"base_ms":50}]}

# 重新加载配置文件
nahookctl reload
```

- 全局参数 `--socket PATH`（默认 `/run/nahook/ctl.sock`）。
- 退出码：0 成功；2 参数错误；1 控制口返回错误或连不上（错误信息打到 stderr，例如 `nahookctl: control returned 500: {"error":"..."}`）。
- 每次修改都会在日志里记一条 `config.retry_max_changed`，带 `scope`、`old`、`new` 和执行者的 `uid`。

### 3.3 直接调用控制口（HTTP over unix socket）

`nahookctl` 只是把子命令转成下面这些 HTTP 请求，脚本里也可以直接用 `curl`：

```bash
S=/run/nahook/ctl.sock
curl -s --unix-socket $S http://nahook/v1/retry                                   # 查全局
curl -s --unix-socket $S 'http://nahook/v1/retry?model=gpt-6-astra'               # 查某个模型
curl -s --unix-socket $S -X PUT -d '{"scope":"*","n":5}' http://nahook/v1/retry   # 改全局
curl -s --unix-socket $S -X PUT -d '{"scope":"gpt-6-astra","n":1}' http://nahook/v1/retry
curl -s --unix-socket $S -X DELETE 'http://nahook/v1/retry?model=gpt-6-astra'     # 删模型级
curl -s --unix-socket $S http://nahook/v1/stats
curl -s --unix-socket $S -X POST http://nahook/v1/reload
```

| 方法与路径 | 请求体 | 响应 |
|---|---|---|
| `GET /v1/retry?model=M` | — | `{"scope":"M"或"*","max":3,"source":"per_model"或"global"}` |
| `PUT /v1/retry` | `{"scope":"*"或模型名,"n":3}`，`n` 必填 | `{"scope":…,"old":…,"new":…}` |
| `DELETE /v1/retry?model=M` | — | `{"scope":"M","removed":true}` |
| `GET /v1/stats` | — | `{"counters":{…},"gates":[{"key","until_ms","streak","base_ms"}]}` |
| `POST /v1/reload` | — | `{"ok":true}` |

错误响应的响应体都是 `{"error":"<原因>"}`：400 参数或 JSON 不合法；404 删除的模型本来就没有模型级配置；409 reload 改了只能重启生效的字段；500 写盘失败（这时文件和内存都不变）。如果已经生效、只是目录 fsync 失败，返回 200 并带 `"warning"`。

### 3.4 代码里的对应函数（`internal/config`）

| 函数 | 作用 |
|---|---|
| `func (s *Store) RetryMax(model string) int` | 数据面每次判定时调用：模型级的值优先，否则用全局值。只读一个原子指针，不拿锁 |
| `func (s *Store) SetRetryMax(scope string, n int) (old int, err error)` | `scope` 是 `"*"` 或模型名。持写锁 → 深拷贝当前快照 → 修改 → 校验 → 写盘 → 原子替换快照 |
| `func (s *Store) UnsetRetryMax(model string) error` | 删除模型级配置，回到全局值；没有该配置时返回 `ErrRetryNotSet` |
| `func (s *Store) Reload() error` | 重新读文件 → 校验 → 拒绝改 `listen`/`control_socket`/`newapi`（`ErrRestartRequired`）→ 原子替换 |

---

## 4. 运维

### 4.1 日志

日志是每行一个 JSON 事件，写到 stdout，由 journald 收集：`journalctl -u nahook -o cat`。

| 事件 | 字段 | 含义 |
|---|---|---|
| `intercept` | `request_id`、`key`、`attempt`、`elapsed_ms`、`max` | 第 `attempt` 次尝试被识别为限流 |
| `wait` | `key`、`wait_ms` | 冷却等待结束，马上重试 |
| `release` | `request_id`、`attempt` | 放行（`attempt` > 0 表示重试后成功） |
| `give_up` | `request_id`、`key`、`attempts` | 用完上限，返回了代 429 |
| `config.retry_max_changed` | `scope`、`old`、`new`、`uid` | 重试上限被修改 |
| `config.reloaded` / `config.reload_failed` | `uid` / `error` | 配置重新加载成功 / 失败 |
| `startup_failed` | `component`、`error` | 启动失败 |

`request_id` 就是 NewAPI 的 `X-Oneapi-Request-Id`，可以直接拿去 NewAPI 的日志里查对应的那次请求。

### 4.2 计数器

| 计数器 | 含义 |
|---|---|
| `intercepted` | 进入拦截流程的请求数（命中路径、模型且是流式） |
| `rate_limited` | 被识别为限流的尝试数 |
| `retries` | 实际发起的重试数 |
| `released` | 拦截流程里放行的请求数 |
| `gave_up` | 返回代 429 的请求数 |
| `passthrough` | 透传的请求数（含不拦截的请求、NewAPI 的非 200 响应、NewAPI 的终态错误，**也含 Caddy 每 10s 一次的健康检查**） |
| `bad_gateway` | 连不上 NewAPI、回了 502 的次数 |
| `client_gone` | 客户端在转发、读启动帧或冷却等待时断开的次数 |

恒成立：`rate_limited = retries + gave_up`。计数器在进程重启后清零。

### 4.3 常见问题

| 现象 | 说明 |
|---|---|
| 流式请求开头的 `response.created` 比以前晚到 | 正常。nahook 要读到第一个非启动事件才能判断要不要拦截，created 会和第一个 delta 一起发出；第一个 token 的到达时间不变 |
| 启动阶段客户端收不到 NewAPI 的 `: PING` 心跳 | 设计上的取舍：nahook 在判定之前不能写任何字节（写了就没法再回 429）。心跳会攒到放行时一起发出 |
| NewAPI 日志里一个请求对应了好几行 | 每次重试在 NewAPI 看来都是一个新请求。被拦截的那几次各记一行、quota 为 0（"Upstream returned no usage"），只有成功的那次扣费 |
| 想彻底关掉拦截 | 删 Caddy 路由并 reload（[1.7](#17-回退和卸载)）。`retry set 0` 不会关掉拦截，只是不再重试、直接回 429 |

---

## 5. 技术逻辑

### 5.1 整体架构

```text
client (Codex / SDK / 浏览器)
   │ HTTPS · HTTP/2 · HTTP/3
   ▼
Caddy ── 站点
   │   encode zstd gzip ; request_body max 512MB
   │
   ├─ @stream_relay = POST /v1/responses | POST /v1/chat/completions
   │     reverse_proxy  127.0.0.1:19080 (nahook, 第 1 上游)  localhost:3000 (NewAPI, 第 2 上游)
   │       lb_policy first · health /api/status 10s · fail_duration 10s
   │       不写 lb_try_duration · 不写 flush_interval · response_header_timeout 30m
   │         │ HTTP/1.1
   │         ▼
   │     nahook  127.0.0.1:19080   (systemd, 单进程, 每个请求一个 goroutine, 没有后台 worker)
   │       classify ─┬─ 不拦截 → forward → passthrough（逐块 flush）
   │                 └─ 拦截   → serveIntercept：读启动帧 → 放行 / 冷却+重试 / 代 429
   │         │ HTTP/1.1 · Accept-Encoding: identity · 请求体原样
   │         ▼
   └─ handle（其它路径）── reverse_proxy localhost:3000 ──┐
                                                          ▼
                                   NewAPI 127.0.0.1:3000（官方镜像）
                                     RetryTimes（只管 HTTP 层错误）· 心跳 ": PING"
                                                    │
                                                    ▼
                                      上游中转站（OpenAI 兼容 SSE）

控制面：root$ nahookctl ──HTTP over unix──► /run/nahook/ctl.sock (0600) ──► control
                                                                  ├─► config.Store（原子快照）──► /etc/nahook/config.json (0600)
                                                                  └─► gate.Registry / obs（只读统计）
观测：slog JSON ─► stdout ─► journald；8 个原子计数器 ─► GET /v1/stats
```

### 5.2 模块与依赖

```text
cmd/nahook ──┬─► internal/proxy ──┬─► internal/startup    启动帧读取与判定
             │                    ├─► internal/gate       冷却门（无锁、无队列）
             │                    ├─► internal/proxy429   代 429
             │                    ├─► internal/config     配置快照（数据面只读）
             │                    └─► internal/obs        日志 + 计数器
             └─► internal/control ──► config / gate / obs
cmd/nahookctl ──unix socket──► internal/control
cmd/mockrelay（测试用的假上游，独立进程）
```

没有循环依赖；不 import 任何 NewAPI 代码，NewAPI 只作为对照实现。

### 5.3 请求分类 `proxy.classify`

```text
ServeHTTP(w, r)
 └─ classify(r, cfg)
     ├─ 不是 POST，或路径不在 match.paths ───────────► forward(r.Body 流式) ─► passthrough
     ├─ 读请求体出错 ─┬─ ctx 已取消 ─► client_gone++，不写响应
     │               └─ 其它       ─► bad_gateway++，回 502
     ├─ 请求体 > body_limit ───────────────────────► forward(已读部分 + 剩余部分) ─► passthrough
     ├─ 坏 JSON，或 "stream" 不是 true ────────────► forward(buf) ─► passthrough
     ├─ model 不在 match.models（列表非空时）───────► forward(buf) ─► passthrough
     └─ 命中 ─────────────────────────────────────► serveIntercept(buf, model)
```

### 5.4 拦截主循环 `proxy.(*Handler).serveIntercept`

```text
intercepted++ ; n = 0
loop:
  t0 = now
  resp, err = forward(ctx, body 原样字节)
     err ─┬─ ctx 已取消 ─► client_gone++ ; return（不写）
          └─ 其它       ─► bad_gateway++ ; 502 ; return
  rid = resp.Header["X-Oneapi-Request-Id"]
  status != 200 或 Content-Type 不是 text/event-stream
        ─► passthrough++ ; passthrough(resp) ; return        ← 包括 NewAPI 自己的 HTTP 429（它已按 RetryTimes 重试过）
  res = startup.Read(resp.Body)
  res = 限流，且 newAPIFinalError(res, rid)
        ─► passthrough++ ; release(prefix, rest) ; return     ← NewAPI 的终态错误，不再叠加一层重试（5.6）
  res = 放行:
        ctx 已取消 ─► client_gone++ ; return                  （不清零冷却门）
        gates.ResetModel(model) ; released++ ; log release ; release(prefix, rest) ; return
  ── 以下是限流 ──
  关闭响应体 ; rate_limited++
  key = model + "|" + org(message)          例：gpt-6-astra|org-xxxx（message 里 "in organization org-…" 的部分）
  gate(key).OnRateLimit(elapsed = now - t0, floor = base_floor_ms, cap = streak_cap)
  max = store.RetryMax(model)                ← 每次都读当前快照，热修改立即生效
  log intercept{request_id, key, attempt:n, elapsed_ms, max}
  n >= max ─► gave_up++ ; log give_up ; proxy429.Write(w, resp.Header, res.Err) ; return
  n++ ; retries++
  gate(key).Wait(ctx) ─ ctx 取消 ─► client_gone++ ; return
  log wait{key, wait_ms} ; goto loop
```

铁律：在 `release` 或 `proxy429.Write` 之前，绝不对客户端写任何东西（包括 Flush）——这是还能改回 429 的前提。

### 5.5 启动帧读取与判定 `startup.Read` / `inspect` / `IsRateLimit`

判定逻辑移植自 NewAPI h3 分支的 `relay/channel/openai/responses_startup_error.go`，区别是遇到任何异常都放行，而不是回 502。

```text
Read(body):                                      上限：MaxBytes = 256KiB，MaxEvents = 128
  逐行读（单行不超过剩余预算，超限 ─► 原样放行）
    "event:" ─► 记下事件名            commentOnly = false
    "data:"  ─► 收集数据行            commentOnly = false
    ":" 开头 ─► 注释（": PING"）       hasComment = true
    其它非空行 ─► commentOnly = false
    空行或 EOF ─► 一个事件结束：events++（> 128 ─► 放行）
        d, err, typed = inspect(事件名, 数据)
        continue  ─► 纯注释事件 heartbeats++ ；有数据的启动事件 dataSeen = true ；只有空行 ─► 两样都不算
        release   ─► VerdictRelease(Prefix = 已读的全部字节, Rest)
        ratelimit ─► VerdictRateLimit(Prefix, Rest, Err,
                       HeartbeatOnly = heartbeats > 0 && !dataSeen,
                       Untyped       = !typed)
  EOF 或读错 ─► VerdictRelease(Prefix = 已读的全部字节)
  不变量：Prefix + Rest == NewAPI 发来的原始字节（放行后逐字节还原）

inspect(name, data):
  data == ""               ─► continue（纯注释事件）
  JSON 解析失败（如 [DONE]） ─► release
  typ = data.type，没有就用事件名 ; typed = (typ != "")
  取错误：error 字段 → response.error → type=="error" 时用顶层的 code/message
          事件是 response.failed / response.error，或 status=="failed" 时兜底
  有错误 ─► IsRateLimit ? ratelimit : release
  typ ∈ {response.created, response.in_progress, response.queued} ─► continue
  其它（第一个 delta、Chat 的首个 chunk）─► release

IsRateLimit(e):
  code == "429"
  或 code/type 含 rate_limit_exceeded | rate_limit_error | ratelimitreached | too_many_requests | toomanyrequests
  或 message 含 "Rate limit reached"
  （刻意不包括 insufficient_quota / quota_exceeded：额度耗尽，同站重试没有意义）
```

### 5.6 NewAPI 终态错误的识别 `newAPIFinalError(res, rid)`

NewAPI 打开心跳时，会在上游还没回的时候先提交 200 并发 `: PING`；之后它按 `RetryTimes` 重试完，把最终错误写进同一个流。这个错误如果再被 nahook 当成上游限流重试，两层重试就叠加了。两条规则，满足任意一条就原样透传：

```text
规则 1：错误消息以 " (request id: <本次响应的 X-Oneapi-Request-Id>)" 结尾
        └ NewAPI 自己生成的错误（如 model_not_found）带这个后缀
规则 2：HeartbeatOnly && Untyped（判定帧之前只有心跳，判定帧不带 type）
        └ NewAPI 转发的上游错误：": PING"… → data: {"error":{…}} → data: [DONE]
          上游的 OpenAI 错误原样输出，不带后缀，规则 1 认不出来（h3、官方 #7857 起）
误判只会往放行的方向：上游很慢、心跳已经发出、上游第一帧又恰好是不带 type 的限流
                      ─► 客户端看到的和没有 nahook 时一样
```

官方 rc.44、rc.45 没有 `writeRelayStreamError`，终态错误是不带 `data:` 的裸 JSON，nahook 读到 EOF 直接放行，同样不会叠加。

### 5.7 冷却门 `gate.Gate`

```text
每个 key 一个 Gate{ until, streak, base }，全部是原子变量 + CAS；没有锁，没有队列

OnRateLimit(elapsed, floor, cap):
   base  = clamp(elapsed, floor, 30m)         这次被拦截的尝试耗时多久，窗口基准就是多久
   s     = min(streak + 1, cap)
   until = max(until, now + base << (s-1))    窗口依次是 1×、2×、4× base（cap = 3）；until 只往后推
Wait(ctx):
   remain = until - now ; remain <= 0 ─► 立即返回
   timer(rand.N(remain))  |  ctx.Done()       全抖动；每个等待者自己的计时器，和排第几个无关
ResetModel(model)：这个模型下任何一次放行 ─► 所有 key 的 streak 清零（until 不回退）
```

为什么用尝试耗时而不用上游给的 "Please try again in 28ms"：实测这个提示在 nahook 拿到时早已过期（被拦截的尝试本身就要一秒多）。为什么用全抖动、不排队：同一时刻被限流的请求如果按顺序排队重试，会在同一时刻再次撞上限流；全抖动把重试分散到整个窗口里。

### 5.8 代 429 `proxy429.Write`

```text
状态码：429
Content-Type：application/json; charset=utf-8
从最后一次尝试的响应头里只抄白名单：X-Oneapi-Request-Id、X-New-Api-Version、Vary、Access-Control-*
响应体（json.Marshal，转义 HTML，末尾不加换行；字段顺序和 NewAPI 的 OpenAIError 一致）：
   {"error":{"message":"<上游原文>","type":"<上游 type>","param":"<上游 param 或空串>","code":<上游原始 JSON 值>}}
```

实测和 NewAPI h3 护栏返回的 429、官方 NewAPI 自己返回的 429 逐字节相同。

### 5.9 放行与透传

```text
release(w, resp, prefix, rest)：
   复制响应头（去掉逐跳头）→ WriteHeader(原状态码) → Write(prefix) 一次写出 + Flush
   → copyFlush(rest)：每读到一块（最大 32KiB）就写出并 Flush
passthrough(resp) = release(resp, prefix=nil, rest=resp.Body)
```

### 5.10 控制面

```text
nahookctl ─► unix socket ─► control（HTTP）─► config.Store
   Store.p   atomic.Pointer[Config]   数据面只读，从不拿锁
   Store.wmu sync.Mutex               只有控制面的写入用
   SetRetryMax：wmu.Lock → 深拷贝 → 修改 → Validate → Persist（临时文件 + fsync + rename + 目录 fsync）→ p.Store
   Reload（SIGHUP / POST /v1/reload）：Load → Validate → 检查只能重启生效的字段 → p.Store
控制口：启动时删掉残留 socket → listen → chmod 0600；uid 用 SO_PEERCRED 取
```

### 5.11 并发与状态

| 对象 | 机制 | 读写方 |
|---|---|---|
| `config.Store.p` | `atomic.Pointer[Config]` | 数据面读；控制面写 |
| `gate.Gate.until/streak/base` | 原子变量 + CAS | 数据面 |
| `gate.Registry` | `sync.Map`，key = 模型 × org，数量很小 | 数据面 |
| 计数器 | `atomic.Int64` | 数据面写；控制面读 |

```text
请求：S0 分类 ─拦截─► S1 转发 ─200 SSE─► S2 读启动帧 ─限流─► S3 拦截
                         │非 200 → 透传        │放行 → release       │ n ≥ max → 代 429
                         │连不上 → 502          │ctx 取消 → 结束       │ 否则 Wait → S1
冷却门（每个 key）：streak 0 ─拦截─► 1 ─► 2 ─► 3（封顶）；同一模型任意一次放行 → 0
                   until 只往后推，取最大值
```

不设单请求总时限：客户端断开由 Caddy 传过来，`r.Context()` 随之取消，转发和等待都会立即返回。发往 NewAPI 的响应头不设超时，由 Caddy 的 `response_header_timeout 30m` 兜底。

---

## 6. 与 NewAPI 的接口

### 6.1 nahook 怎么调用 NewAPI

```text
nahook ─► NewAPI（config.newapi，默认 http://127.0.0.1:3000）
  方法/路径/查询串：和客户端请求相同
  Host：客户端的 Host（Caddy 原样转发）
  请求头：原样复制，去掉逐跳头；Authorization、X-Forwarded-For、X-Real-IP 一律不改
          Accept-Encoding 强制为 identity（启动帧必须是明文才能判定）
          客户端没带 User-Agent 时保持为空（不补 Go-http-client）
  请求体：缓存的原始字节（≤ body_limit；带 ContentLength 和 GetBody）；超过上限的走流式透传
  客户端：不跟随重定向（3xx 原样交给客户端）；Transport 不压缩；空闲连接 2000，空闲超时 10m
  每次重试 = 一个全新的 NewAPI 请求（新的 X-Oneapi-Request-Id，新的一行日志和计费）
  其它路径（如 /api/status 健康检查）：透传
```

### 6.2 NewAPI 返回什么，nahook 怎么处理

| NewAPI 的输出 | nahook 的判定 | 动作 |
|---|---|---|
| 200 SSE：`response.created` → `error` 或 `response.failed`（限流） | 限流 | 冷却后重试；用完上限返回代 429 |
| 200 SSE：首个 delta，或 Chat 的首个 chunk | 放行 | 原样转发 |
| 200 SSE：额度错误（`insufficient_quota`） | 放行 | 原样转发 |
| HTTP 4xx/5xx/429 的 JSON（NewAPI 按 `RetryTimes` 重试过之后） | 非 200 | 透传 |
| 200：`: PING`… 然后是裸 JSON 错误，再到 EOF（官方 rc.44、rc.45） | EOF 放行 | 原样转发 |
| 200：`: PING`… → `data: {"error":…}` → `data: [DONE]`（h3、`#7857` 起） | 限流 + 规则 2 | 透传 |
| 错误消息末尾带 `(request id: <本次 rid>)` | 规则 1 | 透传 |
| 响应头 `X-Oneapi-Request-Id` | — | 写进日志；用于规则 1；代 429 抄这个头 |

### 6.3 nahook 依赖的 NewAPI 行为

1. 流式启动阶段的上游错误被当成功透传：200 SSE（`relay/channel/openai/relay_responses.go` 逐帧转发后返回 nil）。这是 nahook 存在的原因。
2. `RetryTimes` 只对 HTTP 层错误起作用，而且每个请求从 0 开始计数（`controller/relay.go`）。
3. 心跳（`ping_interval`）：上游迟迟不回时，NewAPI 先提交 200 并发 `: PING`，之后的终态错误写在流里（5.6）。
4. 被拦截的尝试在 NewAPI 里各记一行消费日志：quota 0，内容是 "Upstream returned no usage, so nothing was charged"。
5. `STREAMING_TIMEOUT`（默认 300s）到期会关流，nahook 读到 EOF 就放行，自己不另设计时器。
6. `c.ClientIP()` 认 `X-Forwarded-For` / `X-Real-IP`；nahook 原样转发，NewAPI 记录的仍是真实客户端 IP。

### 6.4 两层重试怎么分工

```text
SSE 里的限流（200）            ─► nahook 重试（retry.max），NewAPI 不管
HTTP 层的错误（429/5xx 等）     ─► NewAPI 重试（RetryTimes），nahook 透传
心跳后 NewAPI 写进流里的终态错误 ─► nahook 识别后透传（5.6），不再重试
```

### 6.5 典型时序

```text
(a) 偶发限流：第一次被限流，第二次成功
Caddy     nahook                         NewAPI                 上游
  │─POST─►│ classify = 拦截               │                       │
  │       │─forward #0───────────────────►│──────────────────────►│
  │       │◄─200 SSE created + error(429)─│◄──────────────────────│
  │       │ 判为限流；OnRateLimit；n=0 < 3；Wait（全抖动）
  │       │─forward #1（请求体原样）────────►│──────────────────────►│
  │       │◄─200 SSE created + delta…─────│◄──────────────────────│
  │◄─200 + 前缀 + 后续流──│ release；ResetModel
  客户端看不到那次限流

(b) 一直限流：用完上限
  … 第 1+3 次尝试仍被限流：n=3 ≥ max=3
  │◄─429 {"error":{…}}──│ proxy429.Write（白名单头取自最后一次尝试）

(c) 心跳打开 + 上游回 HTTP 429（h3 关护栏，RetryTimes=3）
  │─POST─►│─forward──────────────────────►│─#1─► 429
  │       │◄─200 ": PING" …（攒住不发）─────│─#2─► 429
  │       │                               │─#3─► 429
  │       │                               │─#4─► 429  NewAPI 自己的重试用完
  │       │◄─data:{"error":…} + data:[DONE]─│
  │       │ 判为限流，但规则 2 成立 ─► 透传，不再重试
  │◄─200 + PING + 错误帧──│ 上游一共被调用 4 次，全部来自 NewAPI 自己
```

---

## 7. 与 Caddy 的接口

### 7.1 接口约定

| 项 | 约定 | 依据 |
|---|---|---|
| 分流 | 只有 `POST /v1/responses` 和 `POST /v1/chat/completions` 进 nahook，其它路径不经过 | — |
| 协议 | Caddy 到 nahook 用 HTTP/1.1 明文，nahook 只监听 127.0.0.1 | — |
| 流式刷新 | 不写 `flush_interval`；`text/event-stream` 由 Caddy 自动立即 flush | 实测逐帧到达 |
| 健康检查 | `health_uri /api/status`，nahook 把它透传给 NewAPI，所以两个上游都能检查 | — |
| 客户端断开 | Caddy 取消发往 nahook 的请求，nahook 的 `r.Context()` 被取消，等待和重试随即停止 | 实测 0.5s 内 |
| nahook 挂掉 | 发现连不上的那个请求收到 502；`fail_duration` 把 nahook 标为不可用，之后的请求直接走 NewAPI；健康检查 10s 内也会标为不健康 | 实测首个 502 用时 13ms，之后全部正常 |
| 不能重试 | Caddy 重试 POST 时请求体已被 Go 的 Transport 关闭，重试必然失败，还会把 NewAPI 也记一次失败，所以不写 `lb_try_duration` | caddyserver/caddy#5454；实测 |
| 超时 | `response_header_timeout 30m` 是外层上限；nahook 到 NewAPI 之间不另设 | — |
| 压缩 | nahook 对 NewAPI 始终要明文；客户端那一侧的压缩由 Caddy 的 `encode` 负责 | 7.4 |
| 来源 IP | `header_up X-Real-IP {remote_host}`，nahook 原样转发 | 实测 NewAPI 记录的 IP 不变 |
| 请求体大小 | Caddy 的 `request_body max_size` 要大于 nahook 的 `body_limit`；超过 `body_limit` 的请求由 nahook 透传 | — |

### 7.2 故障路径

```text
nahook 进程挂掉 / 重启中：
   client ─► Caddy ─dial 19080 被拒─► 502（只有这一个请求）
                   └ 被动健康检查：19080 不可用 10s
   client ─► Caddy ─first 跳过 19080─► NewAPI（和没有 nahook 时的行为一样）
   nahook 恢复 ─► 主动健康检查通过、被动计数过期 ─► 流量回到 nahook（实测 4–10s）

客户端中途断开：
   client ╳ ─► Caddy 取消请求 ─► nahook r.Context() 取消
           ├─ 等 NewAPI 响应头时 ─► forward 返回，关闭到 NewAPI 的连接，client_gone++
           ├─ 冷却等待中        ─► Wait 返回，不再重试，client_gone++
           └─ 放行之后的流式阶段 ─► 写失败 ─► NewAPI 记录 end_reason=client_gone

NewAPI 挂掉：
   nahook ─dial 3000 失败─► 502（bad_gateway++）；默认路由同样拿不到响应
```

### 7.3 为什么 nahook 路由不能写 `flush_interval -1`

在 Caddy 2.6.2 上，写了 `flush_interval -1` 的 `reverse_proxy` 会把发往后端的请求上下文换成"忽略客户端断开"的版本。nahook 在等待和重试期间不写任何字节，Caddy 也就没有机会通过写失败发现客户端已经走了：实测客户端 2 秒就断开，nahook 到第 30 秒还在把响应放行给一个早已离开的客户端（照常扣费，遇到限流还会继续重试）。不写的话，断开 1ms 内就传到后端。Caddy v2.8.0 删除了这段逻辑，但写它没有任何好处，所以一律不写。

### 7.4 Caddy 版本差异

```text
                       Caddy 2.6.2                         Caddy ≥ 2.9（实测 2.11.4）
flush_interval -1      吞掉客户端断开                      已删除（v2.8.0）；我们也不写
encode 压缩 SSE         首次写入超过 512 字节后开始压缩，     每个事件都会立即冲刷，逐帧到达
                       Flush 不冲刷压缩器，事件会被攒住      ─► 可以保留 encode
                       ─► 站点层不要写 encode
POST 重试              请求体已关闭，重试必然失败             同左；不依赖重试（不写 lb_try_duration）
```

nahook 放行时把启动阶段读到的字节（created 加第一个 delta）一次写出，所以经过 nahook 时 Caddy 的首次写入比直连时大，在 2.6.2 上更容易触发压缩。

---

## 8. 测试与验证

### 8.1 单元测试

```bash
go test -race -count=1 ./...     # 需要 CGO（-race）；只用标准库，没有外部依赖
gofmt -l . && go vet ./...
```

### 8.2 假上游 `mockrelay`（只在测试环境用）

`mockrelay` 模拟一个 OpenAI 兼容的上游，按脚本决定每次请求怎么回。测试环境里在 NewAPI 建一个 OpenAI 类型的测试渠道，Base URL 指向 mockrelay（Docker 里的 NewAPI 用 `http://172.17.0.1:18080`），配一个只路由到这个渠道的测试令牌。单元见 `deploy/mockrelay.service`。

```bash
# 设定脚本：第 1 次请求回限流，之后都正常（最后一项一直重复）
curl -s -X POST http://172.17.0.1:18080/__mode -d '{"script":["rate_limit","ok"]}'
# 查看每次请求到达的时间和用了哪一项（设定脚本时清空）
curl -s http://172.17.0.1:18080/__log
```

| 脚本项 | 行为 |
|---|---|
| `ok` | 正常的流（Responses：created → 3 个 delta → completed；Chat：3 个 chunk → stop → `[DONE]`） |
| `slow:N` | 和 `ok` 一样，但 N 个 delta 每秒一个 |
| `hold:N` | N 秒内不回响应头，然后按 `ok` 回 |
| `rate_limit` | created → error（限流）→ response.failed（只用于 Responses） |
| `rate_limit_failed_only` | 只有 response.failed（限流） |
| `hold_rate_limit:N` | N 秒内不回响应头，然后按 `rate_limit` 回 |
| `stall_rate_limit:N` | 先回 200 和响应头，N 秒内不发帧，然后按 `rate_limit` 回 |
| `chat_rate_limit` | Chat 格式的限流错误帧 + `[DONE]` |
| `quota` | 额度耗尽错误（不应被拦截） |
| `http429` | HTTP 429 |
| `hold_http429:N` | N 秒后回 HTTP 429 |

### 8.3 已在测试环境验证的项目

| 类别 | 结果 |
|---|---|
| 拦截与重试 | 偶发限流重试后成功，客户端看不到限流；一直限流时 1+N 次尝试后返回代 429；`retry set` 在下一次判定生效 |
| 冷却门 | 16 个并发请求同时被限流，重试时刻分散在整个窗口里，没有排队（等待时长和到达先后的相关系数约 0.1–0.3） |
| 透传 | 不拦截的请求 900 次，响应和直连 NewAPI 逐字节相同；nahook 增加的延迟小于 0.06ms |
| 格式与计费 | 代 429 和 NewAPI 自己的 429 逐字节相同；放行的流扣费和直连一致；被拦截的尝试不扣费 |
| 断开与故障 | 客户端断开 0.5s 内传到 nahook；nahook 挂掉只影响一个请求；写坏配置后 reload 继续用旧配置；非 root 用户无法使用控制口 |
| 心跳 | 心跳之后的上游限流照常拦截；心跳之后 NewAPI 的终态错误不叠加重试 |
| 版本 | NewAPI 官方 rc.45、h3（开/关护栏）；Caddy 2.6.2 和 2.11.4（带 `encode`） |

完整的测试记录在 `spec/specaudit.md` §7。

---

## 9. 构建与发布

- 工具链：Go 1.27.2（`go.mod` 声明 `go 1.26`），只用标准库，`go.mod` 没有 require。
- 构建（在 Windows 上交叉编译 linux/amd64）：`scripts/build.ps1`，产物在 `O:\WinCache\nahook\dist\<8位提交号>\`（三个二进制、`BUILDINFO.txt`、`SHA256SUMS`）。只在干净、已推送的提交上构建，已有的 dist 目录不会覆盖。
- 发布：`scripts/release.ps1`，创建 prerelease `build-<8位提交号>`，上传五个附件；上传前核对 `BUILDINFO.txt` 的提交号和 `SHA256SUMS`。一个提交只对应一个 Release，不覆盖、不删除。
- 可复现：同一工具链下，对同一提交执行下面的命令，得到的二进制和 Release 附件逐字节相同：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath ./cmd/...
```

二进制不进仓库。

---

## 10. 仓库结构

| 目录 | 内容 |
|---|---|
| `cmd/nahook` | 进程入口：加载配置，启动数据面和控制口，处理信号 |
| `cmd/nahookctl` | 控制口命令行工具 |
| `cmd/mockrelay` | 测试用的假上游 |
| `internal/config` | 配置的读取、校验、落盘和原子快照（`Store`） |
| `internal/startup` | 启动帧读取与判定 |
| `internal/gate` | 冷却门 |
| `internal/proxy429` | 代 429 |
| `internal/proxy` | 数据面：分类、转发、拦截主循环、放行与透传 |
| `internal/control` | 控制口服务端 |
| `internal/obs` | 日志和计数器 |
| `deploy/` | systemd 单元、配置样例 |
| `scripts/` | 构建和发布脚本 |
| `spec/` | 工程 SPEC（设计、接口、审计、排期），以这里为准 |
| `docs/` | 开发任务书、测试方案、知识库快照（`docs/reference/` 只读） |
| `ops/` | 测试环境的实验脚本 |
| `private/` | 服务器清单等含主机信息的文件。被 `.gitignore` 排除，不进仓库；只在开发机和 Win11 上，通过 SSH 直接传输 |

- 仓库只保留 `main` 一个长期分支。开发时从最新的 `main` 新建 `dev`，审计用 `audit/Tn`；合并进 `main` 以后，这些分支都删掉。Release 的 `build-*` 标签保留。
- 仓库是公开的：文件和提交信息里不写真实的主机地址、域名和数据库地址，用「LT 主机 IP」「生产站点域名」这类占位符，对照表在 `private/servers.md`。
