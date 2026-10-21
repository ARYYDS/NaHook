# 01-nahook interface：接口、调用拓扑，以及对应的现有函数

nahook 是一个独立的新程序，不 import NewAPI 的任何代码。每个接口都对到一个**现有的参照实现**：要么移植 H3 的函数，要么照着 NewAPI 官方的行为写。开发时以参照实现为准，不要另起一套写法。

路径前缀：

- `H3/` 指 `C:\code\NewAPI\H3`，`38e69e300`。
- `NEWAPI/` 指 `C:\code\NewAPI\NEWAPI`，`206e0fa5c`。

## 1. 调用拓扑

```text
cmd/nahook.main
  ├─ config.Load ─► config.(*Config).Validate ─► config.NewStore
  ├─ obs.NewLogger / obs.NewStats
  ├─ gate.NewRegistry
  ├─ proxy.NewHandler(store, gates, stats, log) ─► http.Server{Addr: cfg.Listen}.Serve
  ├─ control.Serve(ctx, cfg.ControlSocket, store, gates, stats)
  └─ signal: SIGHUP ─► store.Reload；SIGTERM ─► Server.Shutdown

proxy.(*Handler).ServeHTTP
  ├─ classify ──不拦截──► forward ─► passthrough ─► copyFlush
  └─ 拦截 ─► serveIntercept
        ├─ forward
        ├─ startup.Read ─► inspect ─► upstreamError ─► IsRateLimit
        ├─ 放行：gate.(*Registry).ResetModel ─► release ─► copyFlush
        ├─ 限流：gate.Key ─► (*Registry).Get ─► (*Gate).OnRateLimit
        │        ├─ config.(*Store).RetryMax
        │        ├─ n ≥ max ─► proxy429.Write
        │        └─ 否则 ─► (*Gate).Wait ─► 回到 forward
        └─ obs.(*Stats).Inc

control.Serve ─► 处理 /v1/retry、/v1/stats、/v1/reload
  ├─ config.(*Store).RetryMax / SetRetryMax / UnsetRetryMax / Reload
  ├─ gate.(*Registry).Snapshot
  └─ obs.(*Stats).Snapshot

cmd/nahookctl.main ─► 通过 unix socket 发 HTTP 请求到 control
```

## 2. 接口与对应的现有函数

### 2.1 `internal/startup`

| nahook 接口 | 签名 | 对照的现有函数 | 和对照函数的差异 |
|---|---|---|---|
| `Read` | `func Read(body io.Reader) Result` | `H3/relay/channel/openai/responses_startup_error.go:94` `func readResponsesStartupStream(body io.Reader) responsesStartupReadResult` | 缓冲超限、事件数超限、EOF、读错误时返回 `VerdictRelease` 并带上已读的全部字节，包括没收尾的最后一个事件（h3 是返回 502 错误）；单行读取不超过剩余字节预算（`readLine`），h3 的 `ReadString` 没有这个上限 |
| `readLine` | `func readLine(r *bufio.Reader, limit int) (line []byte, overflow bool, err error)` | `bufio.Reader.ReadSlice` | 读到超过 `limit` 时立即返回 `overflow=true`，已读的字节都在 `line` 里 |
| `inspect` | `func inspect(eventName, data string) (decision, UpstreamError)` | 同文件 `:171` `func inspectResponsesStartupEvent(eventName, data string) (bool, *types.NewAPIError)` | JSON 解析失败时放行（h3 返回 502）；判定结果有三种：继续 / 放行 / 限流 |
| `upstreamError` | `func upstreamError(eventType string, f *frame) (UpstreamError, bool)` | 同文件 `:205` `func responsesStartupFrameError(eventType string, frame *responsesStartupFrame) *types.NewAPIError`，以及 `NEWAPI/relaykit/dto/openai_response.go:605` `func GetOpenAIError(errorField any) *types.OpenAIError` | 逐行等同 h3 `:205-245`：取错误的顺序是 `error` → `response.error` → `type=="error"` 时用顶层的 code/message（Type、Param 留空）；事件是 `response.failed`、`response.error`，或 `response.status=="failed"` 时，没取到错误就用顶层字段兜底，message 为空时填 `Responses startup event reported failure`（`:220-232`）。只返回字段，不做状态码映射 |
| `errorValue` | `func errorValue(raw json.RawMessage) UpstreamError` | `NEWAPI/relaykit/dto/openai_response.go:605` `GetOpenAIError` | 对象按 `map[string]any` 解，type、message、param 只在值是字符串时才取；字符串 → `type:"error"`；其他 → `type:"unknown_error"` |
| `canonical` / `codeText` | `func canonical(raw json.RawMessage) json.RawMessage`；`func codeText(raw json.RawMessage) string` | h3 的 `OpenAIError.Code` 类型是 `any`：输出时由 encoding/json 编码，判定时用 `fmt.Sprint` | `canonical` 先解成 any 再编回去（`429.0` → `429`）；`codeText` 遇到字符串取原值，其他值用 `fmt.Sprint` |
| `isStartupEvent` | `func isStartupEvent(t string) bool` | 同文件 `:196` `func isResponsesStartupEvent(eventType string) bool` | 完全相同 |
| `IsRateLimit` | `func IsRateLimit(e UpstreamError) bool` | 同文件 `:247` `func classifyResponsesStartupError(upstreamErr *types.OpenAIError) (int, bool)` 的第一个 case（`:259`） | 去掉 `insufficient_quota`；补充"message 里含 `Rate limit reached`"这一条（`渠道监控/429-vs-quota.md`） |
| `containsAny` | `func containsAny(text string, values ...string) bool` | 同文件 `:276` | 完全相同 |
| 常量 `MaxBytes`、`MaxEvents` | `256 << 10`、`128` | 同文件 `:24`、`:25` | 完全相同 |
| `Result.Prefix` 和 `Result.Rest` 的拼接 | `io.MultiReader(bytes.NewReader(prefix), rest)` | 同文件 `:78-81`（`replayBody`） | nahook 是把前缀直接写给客户端，再接着复制 `Rest` |

### 2.2 `internal/proxy429`

| nahook 接口 | 签名 | 对照的现有函数 | 说明 |
|---|---|---|---|
| `Write` | `func Write(w http.ResponseWriter, upstream http.Header, e startup.UpstreamError)` | `NEWAPI/controller/relay.go:93` 起的 defer 块（`:105` 设置 `Content-Type`，`:116` 执行 `c.JSON`）；`NEWAPI/relaykit/types/error.go:184` `func (e *NewAPIError) ToOpenAIError(upstreamURLs ...string) OpenAIError` | 对上游的 OpenAI 错误，ToOpenAIError 直接返回原对象，message 不带 request id（`:178` 的 `SetMessage` 只改 `Err`） |
| 响应体结构 | `struct{ Error openAIError "json:\"error\"" }` | `NEWAPI/relaykit/types/error.go:13-19` `type OpenAIError struct{ Message, Type, Param string; Code any; Metadata json.RawMessage }` | 字段顺序一致；`metadata` 本来就带 omitempty，nahook 不输出它 |
| 响应头 | 白名单：`X-Oneapi-Request-Id`、`X-New-Api-Version`、`Vary`、`Access-Control-*` | `NEWAPI/middleware/request-id.go:16`；`NEWAPI/middleware/cors.go:20`（`main.go:198` 全局挂载）；`NEWAPI/router/relay-router.go:16`（CORS） | 从 NewAPI 最后一次尝试的响应头里复制；其他头不复制（design §1.4） |

### 2.3 `internal/gate`

| nahook 接口 | 签名 | 对照 | 说明 |
|---|---|---|---|
| `(*Gate).OnRateLimit` | `func (g *Gate) OnRateLimit(elapsed, floor time.Duration, streakCap int32)` | `渠道监控/4-4-tpm-retry.md` §7（等待要能被取消、下限约 50ms）；NewAPI 里没有对应函数 | 冷却截止时间用 CAS 取最大值；streak 用 CAS 加 1 且不超过 `streakCap`；base 取 `min(max(elapsed, floor), maxBase)`，窗口用这个本地值算（design §1.5） |
| `(*Gate).OnReleased` | `func (g *Gate) OnReleased()` | — | — |
| `(*Gate).Wait` | `func (g *Gate) Wait(ctx context.Context) error` | 写法上参照 `NEWAPI/relay/helper/common.go:90` `StartStreamKeepAlive` 里"计时器 + ctx"的 select 结构 | 全抖动 `rand.N(remain)` |
| `(*Registry).Get` / `ResetModel` / `Snapshot` | `Get(key string) *Gate`；`ResetModel(model string)`；`Snapshot() []GateStat` | — | `sync.Map` |
| `Key` | `func Key(model, message string) string` | org 字段的格式来自 `渠道监控/429-vs-quota.md` 的报错原文 | 正则 `in organization (org-[A-Za-z0-9]+)` |

### 2.4 `internal/config`

| nahook 接口 | 签名 | 对照的现有函数 | 说明 |
|---|---|---|---|
| `Store` | `type Store struct{ p atomic.Pointer[Config]; wmu sync.Mutex; path string }` | `NEWAPI/model/request_policy.go:36` `var requestPolicySnapshot atomic.Pointer[RequestPolicySnapshot]` | 同样是"原子指针快照"的做法：读的一方不加锁 |
| `(*Store).Current` | `func (s *Store) Current() *Config` | `NEWAPI/model/request_policy.go:78` `func CurrentRequestPolicy() *RequestPolicySnapshot` | — |
| `(*Store).Reload` | `func (s *Store) Reload() error` | `NEWAPI/model/request_policy.go:205` `func refreshRequestPolicySnapshot() error`（先构建、校验成功才 Store） | 失败时保留旧快照；`listen`、`control_socket`、`newapi` 有变化时返回 `ErrRestartRequired`（design §1.7） |
| `(*Store).RetryMax` | `func (s *Store) RetryMax(model string) int` | — | design §1.7 |
| `(*Store).SetRetryMax` | `func (s *Store) SetRetryMax(scope string, n int) (old int, err error)` | — | 顺序：校验 → 写盘 → 切换快照；`old` 是修改前这个 scope 的生效值；返回 `ErrDirSync` 时修改已经生效 |
| `(*Store).UnsetRetryMax` | `func (s *Store) UnsetRetryMax(model string) error` | — | — |
| `Load` / `Validate` / `Persist` | `Load(path) (*Config, error)`；`(*Config) Validate() error`；`Persist(path string, c *Config) error` | — | `Load` 拒绝不认识的键和多余的内容；`Validate` 的规则见 design §6.4.1；`Persist`：同目录临时文件 → fsync 文件 → rename → fsync 目录（这一步失败返回 `ErrDirSync`） |
| 哨兵错误 | `ErrInvalidConfig`、`ErrInvalidRetry`、`ErrRetryNotSet`、`ErrRestartRequired`、`ErrDirSync` | — | 和控制口状态码的对应关系见 design §6.4 |

### 2.5 `internal/proxy`

| nahook 接口 | 签名 | 对照 | 说明 |
|---|---|---|---|
| `NewHandler` | `func NewHandler(s *config.Store, g *gate.Registry, st *obs.Stats, log *slog.Logger) *Handler` | — | Transport 参数见 design §6.1 |
| `(*Handler).ServeHTTP` | `func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request)` | — | — |
| `classify` | `func (h *Handler) classify(r *http.Request, cfg *config.Config) ([]byte, reqMeta, bool, io.Reader, error)` | — | design §1.2；只有读请求体出错时返回 error |
| `forward` | `func (h *Handler) forward(ctx context.Context, r *http.Request, body io.Reader) (*http.Response, error)` | 对照 NewAPI 的接收端：`NEWAPI/relay/responses_handler.go:18` `ResponsesHelper`，它只认 `:66` 的状态码判断 | 不改 XFF；`Host = r.Host`；设置 `Accept-Encoding: identity`；URL、请求体长度和 `GetBody` 的规则见 design §6.1 |
| `serveIntercept` | `func (h *Handler) serveIntercept(w http.ResponseWriter, r *http.Request, body []byte, meta reqMeta)` | 对照的是这个循环要弥补的行为：`NEWAPI/relay/channel/openai/relay_responses.go:70` `OaiResponsesStreamHandler`（`:92` 逐帧转发，`:98` 返回 nil）；`NEWAPI/controller/relay.go:72` `Relay`（`:172`/`:177` 每个请求重新计数） | design §1.6 |
| `release` / `passthrough` / `copyFlush` | `release(w, resp, prefix []byte, rest io.Reader)`；`passthrough(w, resp)`；`copyFlush(w, src io.Reader)` | flush 写法参照 `NEWAPI/relay/helper/common.go:138` `FlushWriter`；`http.NewResponseController` 的用法参照 `NEWAPI/relay/helper/stream_scanner.go:80` | — |
| 注释帧 | 透传 `: PING` | `NEWAPI/relay/helper/common.go:231` `PingData` 写出的是 `": PING\n\n"` | 启动阶段把它当注释事件，留在前缀里 |

### 2.6 `internal/control` 与 `cmd/nahookctl`

| nahook 接口 | 签名 / 路由 | 说明 |
|---|---|---|
| `Serve` | `func Serve(ctx context.Context, socket string, s *config.Store, g *gate.Registry, st *obs.Stats) error` | 启动时删掉残留的 socket → Listen → chmod 0600 |
| 路由 | `GET/PUT/DELETE /v1/retry`、`GET /v1/stats`、`POST /v1/reload` | design §5 |
| nahookctl 子命令 | `retry get|set|unset`、`stats`、`reload`，全局参数 `--socket` | design §6.5 |

### 2.7 `internal/obs`

| nahook 接口 | 签名 | 对照 | 说明 |
|---|---|---|---|
| `NewLogger` | `func NewLogger(w io.Writer) *slog.Logger` | Go 标准库 `log/slog` 的 `slog.NewJSONHandler` | 事件和字段见 design §5 |
| `Counter` 与常量 | `type Counter int`；`Intercepted`、`RateLimited`、`Retries`、`Released`、`GaveUp`、`Passthrough`、`BadGateway`、`ClientGone` | — | 键名见 design §6.7 |
| `NewStats` | `func NewStats() *Stats` | — | — |
| `(*Stats).Inc` | `func (s *Stats) Inc(c Counter)` | — | 原子加 1 |
| `(*Stats).Snapshot` | `func (s *Stats) Snapshot() map[string]int64` | — | 所有计数器都列出 |

## 3. 每个接口的逻辑验证

| 接口 | 前置条件 | 不变量 | 失败路径 |
|---|---|---|---|
| `classify` | `r.Body` 没被读过 | 判定为不拦截时，转发给 NewAPI 的请求体和原始请求体逐字节一致 | 读请求体出错 → 返回 error；`ServeHTTP` 在 ctx 已取消时不写响应，否则回 502 |
| `startup.Read` | 响应是 200 且为 `text/event-stream` | `Prefix` 是已读字节的完整前缀；`Prefix` 加 `Rest` 正好等于原始流 | 任何异常都放行，不会凭空造出错误 |
| `IsRateLimit` | `e` 来自 `upstreamError` | 同一个输入，结果和 h3 的 429 分支一致（只除了 `insufficient_quota`） | — |
| `serveIntercept` | `body` 是完整缓存下来的请求体 | 在 `release` 或 `proxy429.Write` 之前 `w` 没有被写过；每次尝试只发一次上游请求；`n ≤ RetryMax` | ctx 被取消 → 不写响应，计 `client_gone`，直接返回；其他转发错误 → 计 `bad_gateway`，回 502 |
| `(*Gate).Wait` | — | 等待时长 < `until - now`；只用自己的计时器 | ctx 被取消 → 返回 `ctx.Err()` |
| `(*Gate).OnRateLimit` | `elapsed ≥ 0` | `until` 只增不减；存下来的 `streak` 始终 ≤ `streakCap`；`base ≤ maxBase`，所以 `base << (s-1)` 不会溢出 | — |
| `(*Store).SetRetryMax` | `n ≥ 0`，scope 不为空 | 文件内容和内存快照一致；数据面读不到中间状态 | 校验失败，或 rename 之前写盘失败 → 快照不变，返回错误；rename 之后目录 fsync 失败 → 快照切换，返回 `ErrDirSync` |
| `proxy429.Write` | `w` 没被写过 | 状态码 429；`Content-Type` 和响应体格式与 NewAPI 一致 | — |
| `control.Serve` | 由 systemd 创建好 `RuntimeDirectory` | socket 权限 0600 | Listen 失败 → 进程退出，systemd 自动重启 |
| `obs.(*Stats)` | — | 只用原子操作，不拿锁；`rate_limited = retries + gave_up` | — |

## 4. 和 design.md 的一致性核对

| design 章节 | interface 位置 | 结论 |
|---|---|---|
| §1.2 classify | 2.5 | 一致 |
| §1.3 Read / inspect / IsRateLimit | 2.1 | 一致 |
| §1.4 proxy429.Write | 2.2 | 一致 |
| §1.5 Gate | 2.3 | 一致 |
| §1.6 serveIntercept | 2.5 | 一致（放行时调用 `ResetModel`） |
| §1.7 Store | 2.4 | 一致 |
| §1.8 release / passthrough / copyFlush | 2.5 | 一致 |
| §5 控制口 API | 2.6 | 一致 |
| §6.7 obs | 2.7 | 一致 |
