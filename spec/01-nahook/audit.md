# 01-nahook audit：开发期核验

## 1. 要点与难点

| 类型 | 内容 | 依据 |
|---|---|---|
| 要点 | 在 `release` 或 `proxy429.Write` 之前绝不写 `w`。这是能回 429 的前提 | design §1.6 |
| 要点 | 不拦截的请求要逐字节透传，不缓冲；放行后的流必须和直连 NewAPI 逐字节一致（计费依赖它） | design §1.8 |
| 要点 | 遇到任何异常都放行，不能比官方行为更差 | design §1.3、§8 |
| 难点 | 冷却门的抖动必须真正分散：没有队列，等待时长和排第几个无关 | design §1.5、§4 |
| 难点 | Caddy 2.6.2 加上 `flush_interval -1` 时，客户端断开是否会传到 nahook | 00-总体架构 §6.1；`自动调度/06-community.md` §3 |
| 难点 | 代 429 要和 NewAPI（h3）返回的逐字节一致：状态码、`Content-Type`、响应体、不带换行、转义 HTML | design §1.4、§8 |
| 审核指标 | 每个客户端请求打上游的次数 ≤ 1 + `RetryMax`；热修改在下一次判定时就生效 | design §1.6、§1.7 |

## 2. 审计清单

测试环境就是 00-总体架构 §2 的"测试（甲）"。T-F1 还需要临时起一个 h3 测试实例做对照：`127.0.0.1:3002`，镜像 h3，打开护栏，指向同一个 mockrelay，测完就删。

| 编号 | 对应 design | 检查方法 | 通过判据 |
|---|---|---|---|
| T-C1 | 00 §6.2 | 测试站点**保留** `flush_interval -1`；mockrelay 设为 `hold:30`；`curl --max-time 2` 经 19443 发请求 | 实测记录 nahook 的 `r.Context()` 是否在 2s 左右被取消（看 nahook 日志，以及 `ss -tn` 里到 3001 的连接是否关闭） |
| T-C2 | 00 §6.2 | 同上，但去掉 `flush_interval -1` 后 reload | 约 2s 内被取消；nahook 不再重试；到 3001 的连接被关闭 |
| T-C3 | 00 §3.3 | mockrelay 设为 `ok`，并把输出拉长到约 30s；进入流式阶段后断开 | 测试 NewAPI 日志里这次流的结束原因是 `client_gone` |
| T-C4 | 00 §3.2 | `systemctl stop nahook-test` 后发请求 | 请求由 3001 正常完成；10s 内 Caddy 把 19081 标为不健康；启动 nahook-test 后流量自动回来 |
| T-F1 | §1.4 | 同一条 `rate_limit` 脚本：先经 nahook-test（上限设为 0），再经 h3 测试实例 3002（护栏打开，`RetryTimes=0`），分别抓客户端拿到的完整响应 | 状态码、`Content-Type`、响应体逐字节一致（`X-Oneapi-Request-Id` 的值除外） |
| T-F2 | §1.6 | 上限设为 2；脚本是 `rate_limit` × 10 | mockrelay 恰好收到 3 次请求；客户端拿到代 429 |
| T-F3 | §1.7 | 上限 3；脚本 `rate_limit` × 10；在第 1 次拦截后执行 `nahookctl --socket /run/nahook-test/ctl.sock retry set 1 --model gpt-6-astra` | 这个请求一共只打 2 次就拿到代 429；配置文件里是新值；日志里有一条 `config.retry_max_changed` |
| T-F4 | §1.5 | 16 个并发请求，脚本是前 16 次 `rate_limit`、之后 `ok`；mockrelay 记下每次重试到达的时间 | 重试的到达时间分布在 `[t0, until]` 里；排序后第 k 个的到达时间和 k 没有线性关系（相关系数接近 0，实测记录）；没有任何一个超出 until |
| T-F5 | §1.2、§1.8 | 非流式请求、不命中的模型、`stream=false`，分别经 nahook 和直连 3001，各 100 次 | 响应逐字节一致；延迟差值实测并记录 |
| T-F6 | §1.8 | 脚本 `ok`，经 nahook 和直连各一次 | 客户端拿到的字节一致；测试 NewAPI 的消费日志里 token 数和 quota 一致 |
| T-F7 | §1.6 | 脚本 `http429`；测试 NewAPI `RetryTimes=0` | nahook 不重试（mockrelay 只收到 1 次）；客户端拿到的是 NewAPI 原样的 429 |
| T-F8 | §1.7 | 把配置文件写坏后 `systemctl reload nahook-test` | 继续使用旧配置；日志里有 `config.reload_failed`；`retry get` 返回旧值 |
| T-F9 | §0 前置约束 | 被拦截的那几次尝试，看测试 NewAPI 的日志和计费 | 实测记录：每次是否 $0、各记了几行日志 |
| T-F10 | §6.1 | 对比经 nahook 和直连时，测试 NewAPI 日志里记录的客户端 IP | 一致（XFF、X-Real-IP 都没被改） |
| T-F11 | §6.4 | 查看 `/run/nahook-test/ctl.sock` 的权限；用非 root 用户执行 `nahookctl` | 权限 0600；非 root 用户被拒 |
| T-F12 | §1.3 | 脚本 `quota` | 不拦截，原样放行 |
| T-F13 | §1.3 | 脚本 `chat_rate_limit`，走 `/v1/chat/completions` | 被拦截，按上限重试 |
| T-F14 | §1.3 | 脚本 `rate_limit_failed_only` | 被拦截 |
| T-F15 | §1.3、2.5 | 测试 NewAPI 打开保活、间隔设到最小；脚本 `hold:5` 之后 `rate_limit` | 能正确拦截；放行的流里保留了 `: PING` |
| T-F16 | §1.2 | 请求体大于 64MiB | 透传，不拦截，NewAPI 正常处理（或按它自己的限制拒绝） |
| P-1 | 00 §1.1 阶段 1 | 生产环境护栏还开着时上线 nahook；观察 30 分钟 | nahook 只有透传、没有拦截；`/v1/responses` 的 p50 延迟和上线前比较（实测记录）；停掉 nahook 后请求不中断 |
| P-2 | 00 §1.1 阶段 2 | 关掉护栏，等下一次限流 | journal 里出现 intercept 并接着 release 或 give_up；客户端拿到的要么是成功的流，要么是 HTTP 429，不会是 SSE 错误 |
| P-3 | 00 §1.1 阶段 3 | 换成官方镜像 | 同 P-2；面板登录、渠道列表、日志页正常 |

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

这一条的适用范围：仓库里还没有 `cmd/` 时（T1–T5），改成执行下面这条，用同样的环境变量交叉编译全部库包，不产出二进制：

```powershell
$env:CGO_ENABLED = '0'; $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; go build -trimpath ./...; Remove-Item Env:GOOS, Env:GOARCH
```

从 T6（开始有 `cmd/`）起，必须执行 `./cmd/...` 那一条。

另外两条要求：

- 不引入任何第三方模块：`go.mod` 里不能出现 `require`。
- 构建只针对已提交的代码：`git status --porcelain` 必须为空。

## 4. 易错与幻觉预警

| 开发 agent 容易想当然的做法 | 正确做法（依据） |
|---|---|
| 直接用 `httputil.ReverseProxy` | 自己写 `forward`（design §6.1、§8） |
| "顺手"把 HTTP 429 也重试了 | 非 200 一律透传（design §1.6） |
| 把 h3 在缓冲超限、坏 JSON、EOF 时回 502 的逻辑照搬过来 | 一律放行（interface 2.1） |
| 给冷却门加一把全局锁或者一个队列 | 只用原子变量和 CAS，每个等待者自己的计时器（design §1.5、§4） |
| 用 `time.Sleep` | 计时器 + `ctx.Done()`（design §8） |
| 自己发明一个平滑系数（EWMA 的 α） | base 取最近一次被拦截尝试的耗时，没有任何未注明出处的常数（design §1.5） |
| 以为 NewAPI 的 `RetryTimes` 会限制 nahook 的重试 | 两者无关；每个请求都从 0 开始计数（`NEWAPI/controller/relay.go:172`） |
| 引入 YAML 库 | 配置是 JSON，只用标准库（design §7） |
| 把 `insufficient_quota` 也算作限流 | 不算（design §1.3） |
| 在代 429 里加上 `Retry-After` 头或改写 message | 和 NewAPI 保持一致，不加也不改（design §1.4） |
| mockrelay 监听 `127.0.0.1` | 监听 `172.17.0.1`（00 §8） |
| 判断不出是否要拦截时就回 502 | 放行（design §1.3） |

## 5. 开工前核查

| 编号 | 要查的点 | 现状 | 影响 |
|---|---|---|---|
| K1 | Caddy 2.6.2 加上 `flush_interval -1` 时，客户端断开会不会传到后端 | 文档写的是"不取消"；#4952 引入了这个行为，2024 年 11 月回退了（`自动调度/06-community.md` §3）。2.6.2 很可能受影响 | 决定生产路由（00 §6.1）写不写 `flush_interval -1`。如果不写也传不到，就要另外评估 nahook 是否需要一个单请求的总时限 |
| K2 | 真实中转站 api21 在流里报限流时帧长什么样：错误在 `error` 事件里还是 `response.failed` 里，前面有没有 `response.created` | 只知道 message 原文和 `type=tokens`、`code=rate_limit_exceeded`（`429-vs-quota.md`），没见过原始帧 | 影响 `upstreamError` 能不能取到错误。message 兜底规则能覆盖大多数形态。可以在阶段 2 开始后，让 nahook 把前几次拦截的原始帧截断后记日志来确认（这些帧里没有密钥） |
| K3 | 阶段 3 要固定的官方镜像 digest | 还没定 | 测试 NewAPI 和生产要用同一个 digest |
| K4 | 从官方镜像回退到 h3 时，数据库兼不兼容 | 官方相比 h3 基线改了 `model/usedata.go`，有新增列（`git diff 32e587406..206e0fa5c -- model/`） | 在测试环境验证：先用官方镜像建好 SQLite 库，再换 h3 镜像启动，做一轮冒烟 |
| K5 | 生产 3000 端口能不能从公网访问 | 容器监听 `0.0.0.0:3000`，安全组情况不明 | 如果能访问，直连的客户端会绕过 nahook。需要用户在云控制台确认，或者从外网测一次 |
| K6 | 测试 NewAPI 的"自用模式"能不能免去模型定价 | 还没验证 | 不行的话，测试实例就要给 `gpt-6-astra` 配上价格 |
