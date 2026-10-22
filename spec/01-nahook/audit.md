# 01-nahook audit：开发期核验

## 1. 要点与难点

| 类型 | 内容 | 依据 |
|---|---|---|
| 要点 | 在 `release` 或 `proxy429.Write` 之前绝不写 `w`。这是能回 429 的前提 | design §1.6 |
| 要点 | 不拦截的请求要逐字节透传，不缓冲；放行后的流必须和直连 NewAPI 逐字节一致（计费依赖它） | design §1.8 |
| 要点 | 遇到任何异常都放行，不能比官方行为更差。认出 NewAPI 的终态限流时改成代 429、不重试，这是用户拍板的行为变化（specaudit D5），不属于异常处理 | design §1.3、§1.6、§8 |
| 难点 | 冷却门的抖动必须真正分散：没有队列，等待时长和排第几个无关 | design §1.5、§4 |
| 难点 | Caddy 2.6.2 加上 `flush_interval -1` 时，客户端断开是否会传到 nahook | 00-总体架构 §6.1；`自动调度/06-community.md` §3 |
| 难点 | 代 429 要和 NewAPI（h3）返回的逐字节一致：状态码、`Content-Type`、响应体、不带换行、转义 HTML。只多一个 nahook 自己加的 `Retry-After` | design §1.4、§8 |
| 难点 | rc.45 的终态限流是心跳后面裸写的 JSON，不是 SSE 事件；只能在"之前只有心跳"的位置认它，不能放宽 | design §1.3、§8 |
| 审核指标 | 每个客户端请求打上游的次数 ≤ 1 + `RetryMax`；NewAPI 的终态限流不增加任何上游调用；热修改在下一次判定时就生效 | design §1.6、§1.7 |
| 审核指标 | 每个代 429 都带 `Retry-After`，值是 1–1800 的整数秒，不小于当时冷却门的剩余时间 | design §1.4 |
| 审核指标 | `nahookctl retry set/unset` 不会覆盖还没 reload 的手工改动 | design §1.7 |

## 2. 审计清单

T-C、T-F 都在测试机 LT 上做（00-总体架构 §2）：经 `「LT 站点域名」` 发请求，用测试令牌，上游是 mockrelay；"直连"指直接请求 `127.0.0.1:3000`。T-F1 还需要临时起一个 h3 对照实例（00 §6.3）：`127.0.0.1:3002`，打开护栏，指向同一个 mockrelay，测完就删。P 项在生产主机上做。

| 编号 | 对应 design | 检查方法 | 通过判据 |
|---|---|---|---|
| T-C1 | 00 §6.1 | LT 的 nahook 路由临时**写上** `flush_interval -1`；mockrelay 设为 `hold:30`；`curl --http2 --max-time 2` 经 `「LT 站点域名」` 发请求（T0 只测了 HTTP/1.1，这里补上 HTTP/2） | 实测记录 nahook 的 `r.Context()` 是否在 2s 左右被取消（看 nahook 日志里的 `client_gone` 计数，以及 `ss -tn` 里 nahook 到 `127.0.0.1:3000` 的连接是否关闭） |
| T-C2 | 00 §6.1 | 同上，但去掉 `flush_interval -1` 后 reload（这是正式配置） | 约 2s 内被取消；nahook 不再重试；到 3000 的连接被关闭。T-C1 预期相反：按 T0 的结论，2s 时不会取消 |
| T-C3 | 00 §3.3 | mockrelay 设为 `slow:30`；进入流式阶段后断开 | LT 的 NewAPI 日志里这次流的结束原因是 `client_gone` |
| T-C4 | 00 §3.2 | `systemctl stop nahook` 后立即连发 3 个请求，12s 后再发一次；然后启动 nahook | 停掉后的第一个请求立即收到 502（不能等满 5s），之后的请求由 3000 完成（不能是 503）；10s 内 Caddy 把 19080 标为不健康；启动 nahook 后流量自动回来 |
| T-F1 | §1.4 | 同一条 `rate_limit` 脚本：先经 LT 的 nahook（上限设为 0），再直连 h3 对照实例 3002（护栏打开，`RetryTimes=0`），分别抓客户端拿到的完整响应 | 状态码、`Content-Type`、响应体逐字节一致（`X-Oneapi-Request-Id` 的值，以及 T16 起 nahook 加的 `Retry-After` 除外） |
| T-F2 | §1.4、§1.6 | 上限设为 2；脚本是 `rate_limit` × 10；响应结束后立刻 `nahookctl stats` | mockrelay 恰好收到 3 次请求；客户端拿到代 429；T16 起带 `Retry-After`，是 ≥ 1 的整数，并且不小于 `stats` 里这个 key 的 `until_ms` 减去收到响应时刻（向上取整到秒） |
| T-F3 | §1.7 | 上限 3；脚本 `rate_limit` × 10；在第 1 次拦截后执行 `nahookctl retry set 1 --model gpt-6-astra` | 这个请求一共只打 2 次就拿到代 429；配置文件里是新值；日志里有一条 `config.retry_max_changed` |
| T-F4 | §1.5 | 16 个并发请求，脚本是前 16 次 `rate_limit`、之后 `ok`；mockrelay 记下每次重试到达的时间 | 重试的到达时间分布在 `[t0, until]` 里；排序后第 k 个的到达时间和 k 没有线性关系（相关系数接近 0，实测记录）；没有任何一个超出 until |
| T-F5 | §1.2、§1.8 | 非流式请求、不命中的模型、`stream=false`，分别经 nahook 和直连 3000，各 100 次 | 响应逐字节一致；延迟差值实测并记录 |
| T-F6 | §1.8 | 脚本 `ok`，经 nahook 和直连各一次 | 客户端拿到的字节一致；LT NewAPI 的消费日志里 token 数和 quota 一致 |
| T-F7 | §1.6 | 脚本 `http429`；LT NewAPI 的 `RetryTimes` 临时设为 0，测完改回 | nahook 不重试（mockrelay 只收到 1 次）；客户端拿到的是 NewAPI 原样的 429 |
| T-F8 | §1.7 | 把配置文件写坏后 `systemctl reload nahook` | 继续使用旧配置；日志里有 `config.reload_failed`；`retry get` 返回旧值 |
| T-F9 | §0 前置约束 | 被拦截的那几次尝试，看 LT NewAPI 的日志和计费 | 实测记录：每次是否 $0、各记了几行日志 |
| T-F10 | §6.1 | 对比经 nahook 和直连时，LT NewAPI 日志里记录的客户端 IP | 一致（XFF、X-Real-IP 都没被改） |
| T-F11 | §6.4 | 查看 `/run/nahook/ctl.sock` 的权限；用非 root 用户执行 `nahookctl` | 权限 0600；非 root 用户被拒 |
| T-F12 | §1.3 | 脚本 `quota` | 不拦截，原样放行 |
| T-F13 | §1.3 | 脚本 `chat_rate_limit`，走 `/v1/chat/completions` | 被拦截，按上限重试 |
| T-F14 | §1.3 | 脚本 `rate_limit_failed_only` | 被拦截 |
| T-F15 | §1.3、2.5 | LT NewAPI 临时打开保活、间隔设到最小（测完改回）；脚本 `hold_rate_limit:5` 和 `stall_rate_limit:5` 各跑一次（PING 的两个来源：等上游响应头时，以及流式阶段） | 能正确拦截；放行的流里保留了 `: PING` |
| T-F16 | §1.2 | 请求体大于 64MiB | 透传，不拦截，NewAPI 正常处理（或按它自己的限制拒绝） |
| T-F17 | §1.3、§1.6 | NewAPI 临时打开心跳、间隔调到最小，`RetryTimes=0`（测完改回）；脚本 `hold_http429:3`。在 LT 的 rc.45 上跑；关掉护栏的 h3 只在要切换镜像时才跑（h3 的切换方法同 K4，需要用户同意；开着护栏时心跳在启动阶段不发出，客户端直接拿到 HTTP 429，测不到这条路径） | T16 起：mockrelay 只收到 1 次请求（nahook 不重试）；客户端拿到 HTTP 429 + `Retry-After`，响应体是 NewAPI 写进流里的那个错误对象（`{"error":{…}}`），不再是 200；`rate_limited` +1、`final_429` +1，`released`、`passthrough` 不变；日志里有一条 `final_429`。T16 之前的预期（rc.45 读到 EOF 放行、h3 按第 2 条透传）见 specaudit §7.16、§7.17 |
| T-F18 | §1.7、§5 | 手工在 `/etc/nahook/config.json` 的 `match.models` 里加一个模型，不 reload：先 `nahookctl config show`，再 `nahookctl retry set 3`；然后 `nahookctl reload`，再 `retry set 3`、`config show`；最后把文件改回原样再 reload | 第一次 `config show` 的 `file` 是 `changed`；第一次 `retry set` 回 409、退出码 1，文件的 sha256 不变，`retry get` 和改之前一样；reload 之后 `retry set` 成功，新加的模型还在文件里，`config show` 的 `file` 是 `in_sync`。把文件写坏（不是合法 JSON）时，`retry set` 同样回 409，`config show` 的 `file` 是 `unreadable` |
| P-1 | 00 §1.2 阶段 1 | 在生产主机上，护栏还开着时上线 nahook；观察 30 分钟 | nahook 只有透传、没有拦截；`/v1/responses` 的 p50 延迟和上线前比较（实测记录）；停掉 nahook 后请求不中断 |
| P-2 | 00 §1.2 阶段 2 | 在生产主机上关掉护栏，等下一次限流 | journal 里出现 intercept 并接着 release 或 give_up；客户端拿到的要么是成功的流，要么是 HTTP 429，不会是 SSE 错误 |
| P-3 | 00 §1.2 阶段 3 | 在生产主机上换成和 LT 相同 digest 的官方镜像 | 同 P-2；面板登录、渠道列表、日志页正常 |

## 3. CI 门禁

本地构建（nahook 仓库）时，下面四项都要通过：

在 Win11 的 `O:\NaHook` 下用 pwsh 7 执行，环境变量见 design §7。每条都必须通过：

```powershell
$env:PATH = 'O:\WinCache\tools\go\bin;O:\WinCache\tools\msys64\ucrt64\bin;' + $env:PATH; $env:GOTOOLCHAIN = 'local'; $env:GOTMPDIR = 'O:\WinCache\tmp'
```

```powershell
if (gofmt -l .) { throw 'gofmt' }
```

```powershell
go vet ./...
```

```powershell
$env:CGO_ENABLED = '1'; go test -race -count=1 ./...
```

```powershell
$env:CGO_ENABLED = '0'; $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; go build -trimpath -o "O:\WinCache\nahook\dist\$(git rev-parse --short=8 HEAD)\" ./cmd/...; Remove-Item Env:GOOS, Env:GOARCH
```

上面这条产出二进制，只在 T8 的 `scripts/build.ps1` 里执行。日常门禁执行下面这条：用同样的环境变量交叉编译仓库里的全部包（包括 `cmd/` 和没被任何 `cmd` 引用的库包），不产出二进制：

```powershell
$env:CGO_ENABLED = '0'; $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; go build -trimpath ./...; Remove-Item Env:GOOS, Env:GOARCH
```

不要把日常门禁换成 `./cmd/...`：那样只会编译被 `cmd` 引用到的包，没被引用的库包就漏掉了 linux 交叉编译检查（`cmd/mockrelay` 不引用任何 `internal` 包）。

另外两条要求：

- 不引入任何第三方模块：`go.mod` 里不能出现 `require`。
- 构建只针对已提交的代码：`git status --porcelain` 必须为空。

## 4. 易错与幻觉预警

| 开发 agent 容易想当然的做法 | 正确做法（依据） |
|---|---|
| 直接用 `httputil.ReverseProxy` | 自己写 `forward`（design §6.1、§8） |
| "顺手"把 HTTP 429 也重试了 | 非 200 一律透传（design §1.6） |
| 以为 NewAPI 的终态错误消息末尾一定带 request id | 只有 NewAPI 自己生成的错误才带；转发上游的 OpenAI 错误不带，要靠"之前只有心跳、判定帧不带 type"来认（design §1.6 第 2 条；h3 `relaykit/types/error.go:184`） |
| 把 h3 在缓冲超限、坏 JSON、EOF 时回 502 的逻辑照搬过来 | 一律放行（interface 2.1） |
| 给冷却门加一把全局锁或者一个队列 | 只用原子变量和 CAS，每个等待者自己的计时器（design §1.5、§4） |
| 用 `time.Sleep` | 计时器 + `ctx.Done()`（design §8） |
| 自己发明一个平滑系数（EWMA 的 α） | base 取最近一次被拦截尝试的耗时，没有任何未注明出处的常数（design §1.5） |
| 以为 NewAPI 的 `RetryTimes` 会限制 nahook 的重试 | 两者无关；每个请求都从 0 开始计数（`NEWAPI/controller/relay.go:172`） |
| 引入 YAML 库 | 配置是 JSON，只用标准库（design §7） |
| 把 `insufficient_quota` 也算作限流 | 不算（design §1.3） |
| 在代 429 里改写 message | 原样保留（design §1.4、§8） |
| `Retry-After` 用 `base`、向下取整，或者在 NewAPI 透传出来的非 200 上也补 | 只加在代 429 上，用 `Gate.Remaining()`，向上取整，1–1800（design §1.4） |
| 认出 NewAPI 的终态限流以后照旧放行，或者顺手再重试一轮 | 不重试，代 429（design §1.6；specaudit Q23、D5） |
| 为了认 rc.45 的裸 JSON，把"非字段行"一律当成 data | 只在之前只有心跳、事件里只有这一行 `{…}`、`event:` 没有值、没有 `data:` 时才这样认（design §1.3） |
| 用文件的字节或哈希判断配置是否被改过 | 解析以后按内容比较，nil 和空集合视为相同（design §1.7 `sameConfig`）；否则只改了缩进也会被拒绝，或者 `Persist` 刚写出的文件就被判成"改过" |
| mockrelay 监听 `127.0.0.1` | 监听 `172.17.0.1`（00 §8） |
| 判断不出是否要拦截时就回 502 | 放行（design §1.3） |

## 5. 开工前核查

| 编号 | 要查的点 | 现状 | 影响 |
|---|---|---|---|
| K1 | Caddy 2.6.2 加上 `flush_interval -1` 时，客户端断开会不会传到后端 | **已查清**（2026-10-21 T0）：写了 `-1` 不传递，等待响应头阶段客户端走了 18s 后端连接仍开着；不写时 1ms 内传递。原因是 caddy v2.6.2 `reverseproxy.go:802-803` 的 `ignoreClientGoneContext`，和 HTTP 版本无关。流式阶段两种配置都会传递（写了 `-1` 时靠写失败发现，约 5s；不写约 2s） | nahook 路由不写 `flush_interval`（00 §6.1）；nahook 不加单请求总时限（design §4）；T-C1、T-C2 用 HTTP/2 在完整链路上复测 |
| K2 | 真实中转站 api21 在流里报限流时帧长什么样：错误在 `error` 事件里还是 `response.failed` 里，前面有没有 `response.created` | 只知道 message 原文和 `type=tokens`、`code=rate_limit_exceeded`（`429-vs-quota.md`），没见过原始帧 | 影响 `upstreamError` 能不能取到错误。message 兜底规则能覆盖大多数形态。可以在阶段 2 开始后，让 nahook 把前几次拦截的原始帧截断后记日志来确认（这些帧里没有密钥） |
| K3 | 阶段 3 要固定的官方镜像 digest | **已定**：`calciumion/new-api@sha256:6c63c7ee9cd5bf7880df3984e99dd3ef07992d7a97538cfb2719adf9d87bc3f9`（`v1.0.0-rc.45`，源码 `6cf0d90b`），2026-10-21 起在 LT 上运行 | 生产阶段 3 用同一个 digest。要换别的版本，先在 LT 上重做 K4 并通过 T10 的相关审计项 |
| K4 | h3 和官方 rc.45 之间来回切换时，数据库兼不兼容 | 正向已验证：h3 `38e69e3` = 官方 `32e587406` + 护栏提交，护栏没改表；rc.45 比 `32e587406` 旧 4 个提交，表结构上只差 `audit_logs` 的一个复合索引（`058c823`），启动迁移只建不删。2026-10-21 在 LT 上换镜像，启动迁移没有报错，渠道和设置都正常读到。回退方向（rc.45 → h3）从代码看兼容，还没在 LT 上实际切换过 | 生产阶段 3 的回退方案依赖它。T10 期间在 LT 上演练一次回退再切回来（各需用户同意） |
| K5 | 生产主机的 3000 端口能不能从公网访问 | **已查清**（2026-10-22）：容器仍监听 `0.0.0.0:3000`，但从 LT 经公网请求 `http://「生产主机 IP」:3000/api/status` 6s 超时，安全组挡住了 | 目前直连的客户端绕不过 nahook。保护只靠安全组；以后可以把端口绑到 `127.0.0.1`，不属于 nahook 上线的前置 |
| K6 | LT 上测试令牌所在分组对 `gpt-6-astra` 的定价 | **已查清**（2026-10-21，T9、T10）：测试令牌的 `/v1/models` 只返回 `gpt-6-astra`，请求没有因定价被拒；T-F6 经 nahook 和直连的消费日志都是 quota 125、prompt 10、completion 3；T-F9 被拦截的尝试 quota 0 | — |
| K7 | 生产主机的 NewAPI 有没有打开心跳（`general_setting.ping_interval_enabled`），间隔是多少 | **LT 已查清**（2026-10-21，用户）：心跳一直开着，间隔 1s。生产和 LT 一样（用户告知），K8 核查时确认 | 心跳打开时，上游的 HTTP 错误只要比 1s 慢，NewAPI 就会先提交 200，终态错误以 200 SSE 的形式出现：rc.45 是裸 JSON；h3 和 `#7857` 起的官方版本是 `data:` 帧，要靠 design §1.6 第 2 条识别（specaudit Q20）。T16 起这两种写法的终态限流都改成代 429（design §1.3、§1.6；specaudit Q23）。启动阶段的心跳被 nahook 攒到放行时才发出（Q19） |
| K8 | 生产主机是哪台、怎么访问；它的基线是否和 LT 在 2026-10-21 之前一致 | **已查清，基线不一致**（2026-10-22 只读核查）：主机 `「生产主机名」`（「生产主机 IP」，Debian 13，4 核 8G）。NewAPI 是官方 `calciumion/new-api@sha256:67bf271602177ff31bbb4b34f20d5f33003d3108514bd23412cff66498c05a84`（`v1.0.0-rc.44`，源码 `633b4c5b`），容器 `api19`，没有护栏开关；编排 `/root/container/api19/compose.yaml`；数据库 `「生产数据库」`，和 LT 隔离；Caddy 官方构建 v2.11.4，站点 `「生产站点域名」`，`lb_policy least_conn`、`lb_try_duration 5s`、`response_header_timeout 30m`、`encode zstd gzip`，没有 `flush_interval -1`；19080 空闲，主机上没有 nahook；另有 `relayscope-agent-api19`（读 NewAPI 日志目录）和 `beszel-agent`（主机网络，上报外部 hub）。 | 原计划的阶段 2、3 不存在；LT 的 Caddy（2.6.2）和生产（2.11.4）不一致。上线路径待用户拍板（00 design §1.2 待拍板，specaudit D3、D4） |
| K9 | 站点层的 `encode zstd gzip` 会不会憋住 SSE（包括 nahook 路由） | **已查清**（2026-10-22，D4）：Caddy 2.6.2 会（`modules/caddyhttp/encode/encode.go:88` 默认压缩 `text/*`；首次 Write 超过 512 字节才开始压缩，`:219-229`；`Flush()` 不冲刷压缩器，`:198-207`、`:380`；v2.9.0 起修复）。Caddy 2.11.4 不会：临时起一个只监听 127.0.0.1 的 SSE 服务（首帧 2KB、之后每秒一帧）和一个开 `encode` 的 Caddy 2.11.4，客户端带 zstd 或 gzip 时响应都被压缩，各帧仍在 0/1/2/3/4s 准时到达，和直连相同。nahook 放行时把启动帧一次写出（`internal/proxy/stream.go:19`）；mockrelay 的首次写入约 320 字节，在 LT 的完整链路上不会触发压缩 | 生产（2.11.4）的 `encode` 保留；LT 按生产加回 |
