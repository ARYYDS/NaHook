# 01-nahook design：Caddy 入口 hook（冷却门 + 抖动同站重试 + 代 429）

## §0 范围与边界

### 范围

nahook 是一个独立的常驻进程，部署在 LT 宿主机上，串在 Caddy 和 NewAPI 之间，只处理"流式 Responses / Chat Completions 在启动阶段被上游限流"这一种情况。它做五件事：

1. 识别 NewAPI 透传出来的 200 SSE 启动帧里的限流错误；
2. 在客户端收到任何字节之前，把它换成一个和 NewAPI 格式一致的 HTTP 429（下称"代 429"）；
3. 按冷却门加全抖动等待一段时间后，把原请求原样重新 POST 给 NewAPI，也就是同站重试；
4. 重试用完后，把代 429 返回给客户端；
5. 重试上限通过控制口函数热修改，一行命令就生效。

### 不在范围

| 内容 | 去处 |
|---|---|
| 按用户切渠道、探测、别名计费、粘滞 TTL | `渠道监控/自动调度/`（知识库，v2） |
| 换渠道时断开思考链 | `渠道监控/自动调度/05-reasoning-break.md` |
| 读取 NewAPI 管理接口、个人访问令牌 | `渠道监控/自动调度/02-admin-api.md` |
| 非流式请求 | 不处理：LT 当天非流式限流 0 次（`自动调度/07-lt-measurements.md` §2） |
| 输出内容开始之后才出现的错误 | 不处理：已经发出的字节撤不回；LT 当天 0 次 |
| 修改 NewAPI 源码、面板、渠道 | 不做：NewAPI 用官方镜像，面板零改动 |

### 与相邻模块的约定

| 相邻模块 | 约定 |
|---|---|
| Caddy 2.6.2（LT 宿主机） | 只把 `POST /v1/responses`、`POST /v1/chat/completions` 交给 nahook，用 `lb_policy first` 把 NewAPI 列为第二个上游。nahook 连不上时，Caddy 直接把请求交给 NewAPI（见 00-总体架构 §3） |
| NewAPI（官方镜像，`127.0.0.1:3000`） | nahook 是它的普通 HTTP 客户端，原样转发客户端的鉴权头和来源 IP 头。nahook 依赖的 NewAPI 行为见 §1.1 |
| 上游中转站 | 不直接打交道，nahook 只看 NewAPI 透传出来的帧 |
| `自动调度`（v2） | v2 只在 §1.6 的"拦截"分支上加一条路，不改本 SPEC 中冷却门和重试上限的语义（`自动调度/03-switch-algorithm.md` §7） |

### 前置约束

- 生产 NewAPI 必须**关闭** `RESPONSES_STREAM_STARTUP_GUARD_ENABLED`（h3），或者直接用官方镜像。护栏开着时 NewAPI 回的是 HTTP 429，nahook 不拦截，只透传。
- 生产 NewAPI 面板里的 `RetryTimes` 保持现值 1，只管 HTTP 层错误。nahook 的上限和它独立。
- 客户端必须经过 Caddy。直连 `:3000` 的请求会绕过 nahook（`自动调度/07-lt-measurements.md` §1）。

## §1 核心算法

### 1.1 算法来源

| 算法 | 出处 |
|---|---|
| 启动帧读取与判定 | `H3/relay/channel/openai/responses_startup_error.go:94` `readResponsesStartupStream`、`:171` `inspectResponsesStartupEvent`、`:205` `responsesStartupFrameError`、`:247` `classifyResponsesStartupError`（只取 429 分支，去掉 `insufficient_quota`）；`渠道监控/429-vs-quota.md`「建议的事件判定」第 2、3 档 |
| 代 429 的格式 | `NEWAPI/controller/relay.go:93-122`（defer 里的错误输出）、`NEWAPI/relaykit/types/error.go:13-19` `OpenAIError`、`:184` `ToOpenAIError`、`NEWAPI/service/error.go:116` |
| NewAPI 把流内错误当成功透传 | `NEWAPI/relay/channel/openai/relay_responses.go:92`、`:98`；`NEWAPI/relay/responses_handler.go:66` |
| 冷却门与全抖动 | AWS Builders' Library（只在一层重试、全抖动）；Google SRE Handling Overload；`渠道监控/4-4-tpm-retry.md` §7（50ms 下限、可取消的等待）；`自动调度/06-community.md` §2 |
| 窗口基准用尝试耗时 | `自动调度/07-lt-measurements.md` §2（提示 3–28ms 拿到时已过期；被拦截的尝试耗时约 1288ms） |
| hook 的重试次数和 NewAPI 不相关 | `NEWAPI/controller/relay.go:172`、`:177`；`自动调度/01-newapi-native.md` §1 |
| 通过函数热修改上限、初始值 3 | 架构确认与用户拍板（见 `../specaudit.md` §7） |

### 1.2 请求分类 `proxy.classify`

```text
func classify(r *http.Request, cfg *config.Config) (body []byte, meta reqMeta, intercept bool, rest io.Reader)
  if r.Method != POST || !slices.Contains(cfg.Match.Paths, r.URL.Path):
      return nil, _, false, r.Body                        # 透传
  buf := 最多读 cfg.BodyLimit+1 字节
  if len(buf) > cfg.BodyLimit:
      return nil, _, false, io.MultiReader(bytes.NewReader(buf), r.Body)   # 太大：透传，不拦截
  var head struct { Stream *bool `json:"stream"`; Model string `json:"model"` }
  if json.Unmarshal(buf, &head) != nil || head.Stream == nil || !*head.Stream:
      return buf, _, false, nil                           # 坏 JSON 或非流式：透传
  if len(cfg.Match.Models) > 0 && !slices.Contains(cfg.Match.Models, head.Model):
      return buf, _, false, nil
  return buf, reqMeta{Model: head.Model}, true, nil
```

### 1.3 启动帧读取与判定 `startup.Read` / `startup.inspect` / `startup.IsRateLimit`

```text
const MaxEvents = 128          # H3 responses_startup_error.go:25
const MaxBytes  = 256 << 10    # H3 responses_startup_error.go:24

type Verdict int               # VerdictRelease | VerdictRateLimit
type UpstreamError struct { Message, Type, Param string; Code json.RawMessage }
type Result struct { Verdict Verdict; Prefix []byte; Rest *bufio.Reader; Err UpstreamError }

func Read(body io.Reader) Result
  reader := bufio.NewReader(body); prefix, event := bytes.Buffer{}
  eventName := ""; dataLines := nil; events := 0
  loop:
    line, readErr := reader.ReadString('\n')
    if prefix.Len()+event.Len()+len(line) > MaxBytes:
        return release(prefix+event+line, reader)          # 超限：原样放行（h3 在这里回 502）
    event.Write(line)
    switch trim(line):
      前缀 "event:" → eventName = 去掉前缀后的值
      前缀 "data:"  → dataLines = append(dataLines, 去掉前缀后的值)
      前缀 ":"      → 注释（例如 ": PING"），只留在 event 里
    if trim(line) == "" || readErr == io.EOF:
      if event.Len() > 0:
        events++
        if events > MaxEvents: return release(prefix+event, reader)
        d, e := inspect(eventName, join(dataLines, "\n"))
        prefix.Write(event)
        switch d:
          decisionContinue  → 继续读下一个事件
          decisionRelease   → return Result{VerdictRelease, prefix, reader}
          decisionRateLimit → return Result{VerdictRateLimit, prefix, reader, e}
      event.Reset(); dataLines = nil; eventName = ""
    if readErr != nil:
      return Result{VerdictRelease, prefix, reader}         # EOF 或读错：原样放行，由调用方写出已读的部分

func inspect(eventName, data string) (decision, UpstreamError)
  if data == "": return decisionContinue                    # 纯注释事件
  var f struct { Type string; Code json.RawMessage; Message string; Param any
                 Error json.RawMessage; Response *struct{ Status string; Error json.RawMessage } }
  if json.Unmarshal(data, &f) != nil: return decisionRelease # 例如 "[DONE]"：放行（h3 在这里回 502）
  typ := f.Type; if typ == "" { typ = eventName }
  e, ok := upstreamError(typ, &f)    # 和 h3 responsesStartupFrameError 取错误的顺序一样：
                                     #   f.Error（对象或字符串）→ f.Response.Error → typ=="error" 时用顶层 code/message
  if ok:
      if IsRateLimit(e): return decisionRateLimit, e
      return decisionRelease                               # 其他错误：原样放行
  if typ ∈ {"response.created", "response.in_progress", "response.queued"}: return decisionContinue
  return decisionRelease                                   # 第一个非启动事件（Chat 的首个 chunk 没有 type，也走这里）

func IsRateLimit(e UpstreamError) bool
  code := lower(trim(unquote(e.Code)))
  text := replace(code + " " + lower(e.Type), "-"→"_", " "→"_", ":"→"_")
  return code == "429" ||
         containsAny(text, "rate_limit_exceeded", "rate_limit_error", "ratelimitreached",
                           "too_many_requests", "toomanyrequests") ||
         containsFold(e.Message, "Rate limit reached")
  # 刻意不含 insufficient_quota / quota_exceeded：额度耗尽在同站重试没有意义（429-vs-quota.md 第 3 档）
```

### 1.4 代 429 `proxy429.Write`

```text
func Write(w http.ResponseWriter, requestID string, e startup.UpstreamError)
  w.Header().Set("Content-Type", "application/json; charset=utf-8")     # NEWAPI controller/relay.go:105
  if requestID != "": w.Header().Set("X-Oneapi-Request-Id", requestID)  # NEWAPI middleware/request-id.go:16
  w.WriteHeader(429)
  body := json.Marshal(envelope{Error: openAIError{
            Message: e.Message, Type: e.Type, Param: e.Param, Code: e.Code /* 原样保留上游的 JSON 值 */ }})
  # 字段顺序 message, type, param, code，和 NEWAPI relaykit/types/error.go:13-19 一致
  # 用 json.Marshal（会转义 HTML、不加换行），和 gin 的 c.JSON 一致
  w.Write(body)
```

### 1.5 冷却门 `gate.Gate`

```text
type Gate struct {
    until  atomic.Int64   # 冷却截止时间（UnixNano），只增不减
    streak atomic.Int32   # 这个 key 连续被拦截的次数
    base   atomic.Int64   # 最近一次被拦截尝试的耗时（纳秒）
}

func Key(model, message string) string
  org := orgRe.FindStringSubmatch(message)       # orgRe = `in organization (org-[A-Za-z0-9]+)`
  return model + "|" + (org[1] 或 "-")

func (g *Gate) OnRateLimit(elapsed, floor time.Duration, streakCap int32)
  g.base.Store(max(elapsed, floor))              # floor = cfg.Gate.BaseFloor
  s := min(g.streak.Add(1), streakCap)           # streakCap = cfg.Gate.StreakCap
  t := now + g.base.Load() << (s-1)              # 窗口 1×、2×、4× base
  for cur := g.until.Load(); t > cur && !g.until.CompareAndSwap(cur, t); cur = g.until.Load() {}

func (g *Gate) OnReleased()
  g.streak.Store(0)

func (g *Gate) Wait(ctx context.Context) error
  remain := g.until.Load() - now
  if remain <= 0: return nil
  t := time.NewTimer(rand.N(remain)); defer t.Stop()   # 全抖动；每个等待者用自己的计时器，没有队列
  select { case <-t.C: return nil; case <-ctx.Done(): return ctx.Err() }
```

### 1.6 拦截重试主循环 `proxy.(*Handler).serveIntercept`

```text
func (h *Handler) serveIntercept(w, r, body []byte, meta reqMeta)
  n := 0                                              # 本请求已被拦截的次数
  for {
    t0 := now
    resp, err := h.forward(r.Context(), r, bytes.NewReader(body))
    if err != nil: writeBadGateway(w); return            # 连不上 NewAPI：和现在 NewAPI 挂掉一样
    if resp.StatusCode != 200 || !isEventStream(resp):
        passthrough(w, resp); return                     # 包括 NewAPI 自己的 429：它已按 RetryTimes 重试过
    res := startup.Read(resp.Body)
    if res.Verdict == startup.VerdictRelease:
        h.gates.ResetModel(meta.Model)                   # 这个模型下所有 key 的 streak 清零（§6.3）
        release(w, resp, res.Prefix, res.Rest); return
    resp.Body.Close()
    cfg := h.store.Current()                             # 每次都读当前快照：热修改马上生效
    key := gate.Key(meta.Model, res.Err.Message)
    g := h.gates.Get(key)
    g.OnRateLimit(now - t0, cfg.Gate.BaseFloor, cfg.Gate.StreakCap)
    max := h.store.RetryMax(meta.Model)
    if n >= max:
        proxy429.Write(w, resp.Header.Get("X-Oneapi-Request-Id"), res.Err); return
    n++
    if g.Wait(r.Context()) != nil: return                # 客户端已断开
  }
```

在 `release` 或 `proxy429.Write` 之前，**不会**对 `w` 做任何写入（包括 Header 以外的写入和 Flush）。

### 1.7 重试上限热修改 `config.(*Store).SetRetryMax`

```text
func (s *Store) RetryMax(model string) int
  c := s.p.Load()
  if v, ok := c.Retry.PerModel[model]; ok { return v }
  return c.Retry.Max

func (s *Store) SetRetryMax(scope string, n int) (old int, err error)   # scope = "*" 或模型名
  if n < 0: return 0, ErrInvalidRetry
  s.wmu.Lock(); defer s.wmu.Unlock()        # 只把控制面的写入串行化；数据面只读 atomic，不经过这把锁
  next := clone(s.p.Load())
  old = 旧值; 写入 next.Retry.Max 或 next.Retry.PerModel[scope]
  if err := next.Validate(); err != nil: return
  if err := Persist(s.path, next); err != nil: return    # 先落盘，失败就不切换：文件和内存始终一致
  s.p.Store(next)
  log.Info("config.retry_max_changed", scope, old, n, uid)

func (s *Store) UnsetRetryMax(model string) error         # 删除模型级配置，回到全局值；流程同上
func (s *Store) Reload() error                            # 由 SIGHUP 和 POST /v1/reload 触发：Load → Validate → Store；失败就保留旧快照
```

### 1.8 放行与透传 `proxy.release` / `proxy.passthrough` / `proxy.copyFlush`

```text
func release(w, resp, prefix []byte, rest io.Reader)
  copyHeaders(w.Header(), resp.Header)        # 去掉逐跳头
  w.WriteHeader(resp.StatusCode)
  w.Write(prefix); flush(w)
  copyFlush(w, rest)                          # 每读到一块就写出并 flush
  resp.Body.Close()

func passthrough(w, resp) = release(w, resp, nil, resp.Body)

func copyFlush(w, src)
  buf := make([]byte, 32<<10)
  for { n, err := src.Read(buf); if n > 0 { w.Write(buf[:n]); http.NewResponseController(w).Flush() }; if err != nil { return } }
```

## §2 架构

### 逻辑架构

```text
                ┌────────────── config.Store：atomic.Pointer[Config] ◄──── control（unix socket）◄── nahookctl
                │                    ▲ 每到一个决策点读一次                      ▲ SIGHUP → Reload
Caddy ──► proxy.Handler.ServeHTTP                                               │
              │ classify ──不拦截──► forward ──► passthrough ──► Caddy           │
              │ 拦截                                                             │
              ▼                                                                  │
          serveIntercept ──► forward ──► NewAPI 127.0.0.1:3000                   │
              │                  ▼                                               │
              │             startup.Read ──放行──► release ──► Caddy              │
              │                  │ 限流                                           │
              │                  ▼                                               │
              │             gate.Registry.Get(key).OnRateLimit ──► stats ─────────┘
              │                  │ n ≥ RetryMax → proxy429.Write ──► Caddy
              │                  │ 否则 → gate.Wait（抖动）→ 回到 forward
              └──► obs（slog JSON 写到 stdout，由 journald 收集；原子计数器）
```

### 模块表

| 模块（Go 包） | 职责 | 依赖 |
|---|---|---|
| `cmd/nahook` | 进程入口：加载配置、启动数据面和控制面两个监听、处理信号 | `internal/*` |
| `cmd/nahookctl` | 命令行工具：把子命令转成控制口的 HTTP 请求 | 只用标准库 |
| `cmd/mockrelay` | 测试用的假上游（只部署在测试环境） | 只用标准库 |
| `internal/config` | `Config`、`Load`、`Validate`、`Persist`、`Store` | 标准库 |
| `internal/startup` | 启动帧读取与判定 | 标准库 |
| `internal/gate` | 冷却门、`Registry`、`Key` | 标准库 |
| `internal/proxy429` | 代 429 输出 | `internal/startup`（`UpstreamError`） |
| `internal/proxy` | `Handler`、分类、转发、放行、透传、拦截主循环 | `config`、`startup`、`gate`、`proxy429`、`obs` |
| `internal/control` | 控制口服务端 | `config`、`gate`、`obs` |
| `internal/obs` | 日志和计数器 | 标准库 |

### 依赖方向

`cmd/*` → `internal/proxy`、`internal/control` → `config`、`startup`、`gate`、`proxy429`、`obs`。没有循环依赖。不 import 任何 NewAPI 代码，NewAPI 只作为对照实现。

### 进程与通道

| 进程 | 监听 / 通道 | 说明 |
|---|---|---|
| `nahook`（systemd `nahook.service`） | 数据面 `127.0.0.1:19080`（HTTP/1.1）；控制面 `/run/nahook/ctl.sock` | 每个请求一个 goroutine；没有后台 worker |
| `nahookctl` | 连接 `/run/nahook/ctl.sock` | 短进程，只有 root 能用 |
| 发往 NewAPI | `http://127.0.0.1:3000` | 共用一个 `http.Transport` |

## §3 管线与时序

### 3.1 不拦截（非流式、不命中的模型、请求体太大）

```text
Caddy ──POST──► nahook.classify ──否──► forward ──► NewAPI
Caddy ◄────────── passthrough（每块 flush）◄────────────┘
```

### 3.2 被拦截后重试成功

```text
Caddy      nahook                        NewAPI                上游
  │─POST──►│ classify=拦截                  │                      │
  │        │─forward（第 0 次）────────────►│─────────────────────►│
  │        │◄─200 SSE: created, error(429)─│◄─────────────────────│  约 1.3s（LT）
  │        │ startup.Read=RateLimit；OnRateLimit；n=0 < max=3
  │        │ Wait：rand(0, until-now)
  │        │─forward（第 1 次，请求体原样）►│─────────────────────►│
  │        │◄─200 SSE: created, delta…─────│◄─────────────────────│
  │◄─200 + 前缀 + 后续──│ release；OnReleased                      │
```

### 3.3 重试用完，返回代 429

```text
  … 第 3 次重试仍被拦截：n=3 ≥ max=3
  │◄─429 {"error":{…}}─│ proxy429.Write（X-Oneapi-Request-Id 取自最后一次尝试）
```

### 3.4 热修改上限

```text
root$ nahookctl retry set 5 --model gpt-6-astra
nahookctl ─PUT /v1/retry {"scope":"gpt-6-astra","n":5}─► control
control ─► Store.SetRetryMax ─► Validate ─► Persist（临时文件 + fsync + 改名）─► atomic.Store
正在重试中的请求在下一次被拦截时读到 5
```

### 3.5 nahook 宕机

```text
Caddy ─连 127.0.0.1:19080 被拒─► 按 lb_policy first 换下一个上游 ─► NewAPI :3000
（客户端看到的就是官方原样行为：流内限流照常以 SSE 错误送达）
```

### 3.6 客户端断开

```text
客户端断开 ─► Caddy ─（是否取消发往后端的请求：由 T-C1/T-C2 实测决定）─► r.Context() 取消
           ─► forward 和 Wait 返回 ─► 关闭到 NewAPI 的连接
```

## §4 并发、队列与状态

### 队列与信号

**没有队列。** 冷却门不排队：每个等待者用自己的 `time.Timer`，等待时长从 `[0, until-now)` 里独立随机抽取，和排第几个无关。

### 原子变量与锁

| 对象 | 机制 | 谁读 / 谁写 |
|---|---|---|
| `config.Store.p` | `atomic.Pointer[Config]` | 数据面只读；控制面写 |
| `config.Store.wmu` | `sync.Mutex` | 只有控制面的写入路径用；数据面从不拿这把锁 |
| `gate.Gate.until/streak/base` | `atomic.Int64` / `Int32`，配 CAS | 数据面读写 |
| `gate.Registry.m` | `sync.Map` | 数据面读写；key 的数量 = 模型数 × org 数，量很小，不做清理 |
| `obs` 计数器 | `atomic.Int64` | 数据面写；控制面读 |

### 时限

| 时限 | 值 | 出处 |
|---|---|---|
| 数据面 `http.Server` | 不设读写超时（Go 默认值）：流式响应不能设 WriteTimeout | Go net/http 默认值 |
| 发往 NewAPI 的响应头超时 | 不设 | 由 Caddy 的 `response_header_timeout 30m` 兜底（LT Caddyfile） |
| 启动帧阶段 | 不另设计时器 | NewAPI 的 `STREAMING_TIMEOUT` 默认 300s（`NEWAPI/common/init.go:185`）到期会关流 → `Read` 拿到 EOF → 放行 |
| 抖动等待 | `[0, until-now)` | §1.5 |
| 空闲连接 | `IdleConnTimeout 10m`，`MaxIdleConnsPerHost 2000` | 和 LT Caddyfile 里的 `keepalive 10m`、`keepalive_idle_conns 2000` 一致 |

### 状态机

```text
请求：S0 分类 ─拦截─► S1 转发 ─200 SSE─► S2 读启动帧 ─限流─► S3 拦截
                         │非 200 → 透传        │放行 → release       │ n ≥ max → 代 429
                         │连不上 → 502          │ctx 取消 → 结束       │ 否则 Wait → S1
冷却门（每个 key）：streak 0 ─拦截─► 1 ─► 2 ─► 3（封顶）；同一模型任意一次放行 → 0
                   until 只往后推，取最大值
```

## §5 数据模型与存储

### 配置 `/etc/nahook/config.json`

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

| 字段 | 出处 |
|---|---|
| `listen` 19080 | 架构确认；LT 上该端口空闲（2026-10-20 `ss -ltn` 实测） |
| `body_limit` 64MiB | 架构确认 |
| `match.models` | 架构确认；留空表示所有流式请求 |
| `retry.max` 3 | 用户拍板 |
| `gate.base_floor_ms` 50 | `渠道监控/4-4-tpm-retry.md` §7 |
| `gate.streak_cap` 3 | 架构确认：窗口最大 4× base |

### 控制口 API（HTTP over unix socket）

| 方法与路径 | 请求 | 响应 |
|---|---|---|
| `GET /v1/retry?model=M` | — | `{"scope":"M"或"*","max":3,"source":"per_model"或"global"}` |
| `PUT /v1/retry` | `{"scope":"*"或模型名,"n":3}` | `{"scope":…,"old":…,"new":…}` |
| `DELETE /v1/retry?model=M` | — | `{"scope":"M","removed":true}` |
| `GET /v1/stats` | — | `{"counters":{…},"gates":[{"key","until_ms","streak","base_ms"}]}` |
| `POST /v1/reload` | — | `{"ok":true}`；失败时回 500，并带上错误信息 |

### 代 429 的响应体

`{"error":{"message":"<上游原文>","type":"<上游 type>","param":"<上游 param 或空串>","code":<上游原始 JSON 值>}}`

### 日志（slog JSON，一行一条事件）

| 事件 | 字段 |
|---|---|
| `intercept` | `request_id`、`key`、`attempt`、`elapsed_ms`、`max` |
| `wait` | `key`、`wait_ms` |
| `release` | `request_id`、`attempt` |
| `give_up` | `request_id`、`key`、`attempts` |
| `config.retry_max_changed` | `scope`、`old`、`new`、`uid` |
| `config.reload_failed` | `error` |

### 存储位置

| 内容 | 位置 |
|---|---|
| 二进制 | `/usr/local/bin/nahook`、`/usr/local/bin/nahookctl` |
| 配置 | `/etc/nahook/config.json`（0600） |
| 控制口 | `/run/nahook/ctl.sock`（0600，由 systemd 的 `RuntimeDirectory=nahook` 创建） |
| 日志 | journald（`journalctl -u nahook`） |
| systemd 单元 | `/etc/systemd/system/nahook.service` |
| 测试环境 | 见 00-总体架构 §5 |

## §6 模块细节

### 6.1 `internal/proxy`

| 函数 | 说明 |
|---|---|
| `NewHandler(store *config.Store, gates *gate.Registry, st *obs.Stats, log *slog.Logger) *Handler` | 用 `newapi` 地址构造 `http.Transport{DisableCompression: true, MaxIdleConns: 2000, MaxIdleConnsPerHost: 2000, IdleConnTimeout: 10m}` |
| `(*Handler).ServeHTTP` | 调 `classify`；拦截走 `serveIntercept`，否则走 `forward` + `passthrough` |
| `forward(ctx, r, body)` | 新建请求：方法、`RequestURI`、请求头（去掉逐跳头）原样复制，`Accept-Encoding: identity`；设好 `ContentLength` 和 `GetBody`。**不改动** `X-Forwarded-For`、`X-Real-IP`、`Authorization` |
| 逐跳头 | `Connection`、`Keep-Alive`、`Proxy-Connection`、`Proxy-Authenticate`、`Proxy-Authorization`、`TE`、`Trailer`、`Transfer-Encoding`、`Upgrade`，以及 `Connection` 头里点名的字段（RFC 9110 §7.6.1） |
| `writeBadGateway` | 回 502 `{"error":{"message":"nahook: upstream unavailable","type":"nahook_error","param":"","code":"upstream_unavailable"}}` |

### 6.2 `internal/startup`

常量 `MaxEvents`、`MaxBytes`（§1.3）。启动事件集合 `{"response.created","response.in_progress","response.queued"}`，出处是 `H3/relay/channel/openai/responses_startup_error.go:196`。

### 6.3 `internal/gate`

| 函数 | 说明 |
|---|---|
| `Registry.Get(key) *Gate` | 用 `sync.Map.LoadOrStore` 拿到同一个 `*Gate` |
| `Registry.ResetModel(model)` | 遍历 `sync.Map`，对前缀为 `model|` 的所有 Gate 调 `OnReleased`。放行时还不知道 org，所以要按模型把这些 key 一起清零；key 的数量是模型数 × org 数，很少 |
| `Registry.Snapshot() []GateStat` | 给 `/v1/stats` 用 |
| `Key(model, message)` | §1.5 |

### 6.4 `internal/control`

- 启动时先删掉残留的 socket 文件，再 `net.Listen("unix")`，然后 `os.Chmod(path, 0600)`。
- 错误码：
  - 400：`n < 0` 或 JSON 不合法；
  - 404：`DELETE` 的模型本来就没有模型级配置；
  - 500：写配置文件失败（这时内存里的配置不变）。

### 6.5 `cmd/nahookctl`

| 子命令 | 控制口调用 |
|---|---|
| `retry get [--model M]` | `GET /v1/retry` |
| `retry set N [--model M]` | `PUT /v1/retry`，不带 `--model` 时 scope 为 `*` |
| `retry unset --model M` | `DELETE /v1/retry` |
| `stats` | `GET /v1/stats` |
| `reload` | `POST /v1/reload` |

全局参数 `--socket PATH`：默认 `/run/nahook/ctl.sock`；测试实例用 `/run/nahook-test/ctl.sock`（00-总体架构 §5）。

退出码：成功 0；参数错误 2；控制口返回错误 1。

### 6.6 `cmd/mockrelay`（只在测试环境用）

- 监听 `172.17.0.1:18080`，对外表现为一个 OpenAI 兼容上游。
- 用 `POST /__mode` 设置行为脚本，例如 `{"script":["rate_limit","rate_limit","ok"]}`：每来一个请求按顺序取一项，取完后一直重复最后一项。

| 脚本项 | 行为 |
|---|---|
| `ok` | 正常流式：created → output_text.delta × 3 → completed（带 usage） |
| `rate_limit` | created → `{"type":"error","error":{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached for gpt-6-astra in organization org-mock on tokens per min (TPM): Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms. …"}}` → response.failed。文案来自 `渠道监控/429-vs-quota.md` |
| `rate_limit_failed_only` | 只发 response.failed，`response.error` 里带同样的错误 |
| `http429` | 直接回 HTTP 429 JSON |
| `hold:<秒>` | 先不回响应头，等指定秒数再按 `ok` 回 |
| `chat_rate_limit` | Chat Completions 格式：首个 chunk 就是 `{"error":{…}}` |
| `quota` | `insufficient_quota`，不应该被拦截 |

## §7 技术栈与依赖

| 项 | 选型 | 出处 / 说明 |
|---|---|---|
| 仓库 | `github.com/ARYYDS/NaHook`（公开）。开发机副本 `C:\code\NewAPI\NaHook`，只放 SPEC 和文档；Win11 副本 `O:\NaHook`，DSH 在这里写代码 | 用户拍板 |
| 语言 | Go；`go.mod` 写 `module github.com/ARYYDS/NaHook` 和 `go 1.26`，和 NewAPI 根模块一致（`NEWAPI/go.mod`） | — |
| 工具链（Win11） | `O:\WinCache\tools\go\bin\go.exe`，go1.27.2；`GOTOOLCHAIN=local`。系统自带的 go1.27.1 不用 | Win11 布局约定；2026-10-20 实测 |
| Go 缓存（Win11） | `GOCACHE=O:\WinCache\go-build`、`GOMODCACHE=O:\WinCache\go-mod`、`GOPATH=O:\WinCache\go`（已在用户环境变量里）；`GOTMPDIR=O:\WinCache\tmp` | Win11 布局约定；实测 |
| `-race` | `CGO_ENABLED=1`，并把 `O:\WinCache\tools\msys64\ucrt64\bin`（里面有 gcc）加到 PATH | 实测该路径下有 gcc |
| 依赖 | **只用标准库**（`net/http`、`encoding/json`、`log/slog`、`sync/atomic`、`math/rand/v2`） | 不新增第三方依赖；配置用 JSON 正是为了不引入 YAML 库 |
| 构建 | 在 Win11 上：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath`，输出到 `O:\WinCache\nahook\dist\<8hex>\` | LT 是 x86_64、Debian 13，没装 Go（实测） |
| 产物 | `O:\WinCache\nahook\dist\<8hex>\` 下的 `nahook`、`nahookctl`、`mockrelay`、`BUILDINFO.txt`（提交号、`go version`）和 `SHA256SUMS`；**不进 git** | — |
| 分发 | GitHub Release：tag 是 `build-<8hex>`，标为 prerelease，附件就是上面五个文件。由 Win11 上的 `gh release create` 发布。LT 从公开地址 `https://github.com/ARYYDS/NaHook/releases/download/build-<8hex>/<文件名>` 下载，用 `sha256sum -c SHA256SUMS` 校验 | 用户拍板（仓库已公开，LT 不需要令牌） |
| 运行 | systemd 257；`Restart=always`、`RuntimeDirectory=nahook` | LT 实测版本 |
| 入口 | 已有的 Caddy 2.6.2 | 不升级 |
| 测试环境 | Docker 29.8.2 上跑官方镜像 `calciumion/new-api`（固定 digest） | 见 00-总体架构 §5 |

### 发布与分发流程

```text
Win11 O:\NaHook（dev 分支已推送，工作区干净）
  └─ 构建 ─► O:\WinCache\nahook\dist\<8hex>\{nahook,nahookctl,mockrelay,BUILDINFO.txt,SHA256SUMS}
  └─ gh release create build-<8hex> --target <完整提交号> --prerelease --title "build <8hex>" --notes-file BUILDINFO.txt <五个文件>
GitHub Release（公开）
  └─ LT：curl -fL 下载到 /root/nahook-dist/<8hex>/ ─► sha256sum -c SHA256SUMS
        ─►（请示用户）install 到 /usr/local/bin ─►（请示用户）systemctl restart
```

- 只有收到明确指令时才发 Release（T8 交付时发第一次）。
- 每次往 LT 上下载、安装、重启，都要先请示用户。
- 发布出去的 Release 不覆盖，也不删除：一个提交只对应一个 tag。

## §8 易错点

| 错误写法 | 后果 | 正确写法与依据 |
|---|---|---|
| 用 `httputil.ReverseProxy` 的默认 Director | 它会追加 `X-Forwarded-For`，NewAPI 看到的来源 IP 链就变了 | 自己写 `forward`，原样复制请求头（Go 文档：ReverseProxy 默认会追加 XFF） |
| 判定之前调用了 `w.WriteHeader`、`Flush` 或任何写入 | 200 已经提交，没法再回 429 | 只在 `release` 或 `proxy429.Write` 里写（§1.6） |
| 拿同一个 `r.Body` 重发 | 第二次发出去的请求体是空的 | 缓存成 `[]byte`，每次用 `bytes.NewReader`，并设置 `GetBody` |
| 用 `time.Sleep` 实现等待 | 客户端断开后也打断不了 | `Timer` + `select ctx.Done()`（`4-4-tpm-retry.md` §7） |
| NewAPI 回 HTTP 429 也重试 | 和 NewAPI 的 `RetryTimes` 叠加；还会把 NewAPI 自己的模型限流 429 当成上游限流 | 非 200 一律透传（`429-vs-quota.md` 表格第 2 行） |
| 把 `insufficient_quota` 也算作限流 | 额度已经耗尽，同站重试只是白白浪费 | `IsRateLimit` 不包含它（§1.3） |
| 缓冲超限、坏 JSON、EOF 时回 502（照搬 h3） | 比官方行为更差 | 一律放行（§1.3） |
| 用 `json.Encoder` 写代 429 | 末尾多一个换行，和 NewAPI 不再逐字节一致 | 用 `json.Marshal`（gin 的 c.JSON 不加换行、会转义 HTML；NewAPI 没有用 sonic 构建，见 `NEWAPI/Dockerfile:29`） |
| 在代 429 里改写或脱敏上游 message | 和 h3 不一致 | 原样保留。NewAPI 只会脱敏渠道自己的 Base URL；LT 的报错文案里没有渠道地址（`自动调度/07-lt-measurements.md` §2） |
| 配置的临时文件写到 `/tmp` 再改名 | 跨文件系统时 rename 不是原子的 | 临时文件放在同一目录，并对文件和目录都做 fsync |
| socket 文件残留 | 重启时 Listen 失败 | 启动时先删掉；或交给 `RuntimeDirectory` 管理 |
| 一直不读完、不关闭被拦截的那次响应体 | 连接泄漏 | 被拦截后立即 `resp.Body.Close()` |
