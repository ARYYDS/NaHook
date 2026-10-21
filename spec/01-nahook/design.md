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
func classify(r *http.Request, cfg *config.Config) (body []byte, meta reqMeta, intercept bool, rest io.Reader, err error)
  if r.Method != POST || !slices.Contains(cfg.Match.Paths, r.URL.Path):
      return nil, _, false, r.Body, nil                   # 透传
  buf, err := io.ReadAll(io.LimitReader(r.Body, cfg.BodyLimit+1))
  if err != nil: return nil, _, false, nil, err        # ServeHTTP：ctx 已取消 → 计 ClientGone、不写；否则计 BadGateway、回 502
  if len(buf) > cfg.BodyLimit:
      return nil, _, false, io.MultiReader(bytes.NewReader(buf), r.Body), nil   # 太大：透传，不拦截
  var head struct { Stream *bool `json:"stream"`; Model string `json:"model"` }
  if json.Unmarshal(buf, &head) != nil || head.Stream == nil || !*head.Stream:
      return buf, _, false, nil, nil                      # 坏 JSON 或非流式：透传
  if len(cfg.Match.Models) > 0 && !slices.Contains(cfg.Match.Models, head.Model):
      return buf, _, false, nil, nil
  return buf, reqMeta{Model: head.Model}, true, nil, nil
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
    line, overflow, readErr := readLine(reader, MaxBytes - prefix.Len() - event.Len())
        # readLine 用 ReadSlice 逐块拼一行，一旦超过给定上限就立即返回，
        # 防止上游发一个超长、又没有换行的行时把内存吃光（h3 的 ReadString 没有这个上限）
    if overflow:
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
      return Result{VerdictRelease, prefix+event, reader}   # EOF 或读错：原样放行；没收尾的最后一个事件也并进 Prefix，
                                                            # 保证 Prefix+Rest == 原始字节

func inspect(eventName, data string) (decision, UpstreamError)
  if data == "": return decisionContinue                    # 纯注释事件
  var f struct { Type string; Code json.RawMessage; Message string; Param json.RawMessage
                 Error json.RawMessage; Response *struct{ Status json.RawMessage; Error json.RawMessage } }
  if json.Unmarshal(data, &f) != nil: return decisionRelease # 例如 "[DONE]"：放行（h3 在这里回 502）
  typ := f.Type; if typ == "" { typ = eventName }
  e, ok := upstreamError(typ, &f)
  if ok:
      if IsRateLimit(e): return decisionRateLimit, e
      return decisionRelease                               # 其他错误：原样放行
  if typ ∈ {"response.created", "response.in_progress", "response.queued"}: return decisionContinue
  return decisionRelease                                   # 第一个非启动事件（Chat 的首个 chunk 没有 type，也走这里）

func upstreamError(typ string, f *frame) (UpstreamError, bool)   # 逐行对照 H3 responses_startup_error.go:205-245
  JSON null 一律当作字段不存在
  switch:
    f.Error 有值          → e = errorValue(f.Error)
    f.Response.Error 有值 → e = errorValue(f.Response.Error)
    typ == "error"        → e = {Message: f.Message, Code: canonical(f.Code)}   # 和 h3 一样，只取 code 和 message，Type、Param 留空
  failed := typ ∈ {"response.failed", "response.error"} || stringValue(f.Response.Status) == "failed"   # 不区分大小写；H3 :220-227
  if failed:
      if 上面没取到错误: e = {Message: f.Message, Code: canonical(f.Code)}
      if trim(e.Message) == "": e.Message = "Responses startup event reported failure"   # H3 :232
  return e, 取到了错误 || failed

func errorValue(raw) UpstreamError                          # 等同 NEWAPI relaykit/dto/openai_response.go:605 GetOpenAIError
  对象   → 按 map[string]any 解：type、message、param 只在值是字符串时才取；code 取原值，经 canonical 处理
  字符串 → {Message: 该字符串, Type: "error"}
  其他   → {Message: fmt.Sprint(值), Type: "unknown_error"}

func canonical(raw) json.RawMessage                         # 先解成 any，再用 encoding/json 编回去：
                                                            # 和 h3 输出 OpenAIError.Code（类型是 any）时得到的字节完全一样，例如 429.0 → 429

func IsRateLimit(e UpstreamError) bool
  code := lower(trim(codeText(e.Code)))                    # codeText：字符串取原值，其他值用 fmt.Sprint(解出来的 any)，和 h3 的 fmt.Sprint 一致
  text := replace(code + " " + lower(e.Type), "-"→"_", " "→"_", ":"→"_")
  return code == "429" ||
         containsAny(text, "rate_limit_exceeded", "rate_limit_error", "ratelimitreached",
                           "too_many_requests", "toomanyrequests") ||
         containsFold(e.Message, "Rate limit reached")
  # 刻意不含 insufficient_quota / quota_exceeded：额度耗尽在同站重试没有意义（429-vs-quota.md 第 3 档）
```

### 1.4 代 429 `proxy429.Write`

```text
func Write(w http.ResponseWriter, upstream http.Header, e startup.UpstreamError)
  # upstream 是 NewAPI 最后一次尝试的响应头。h3 的 429 和这次尝试出自同一个 NewAPI 请求，
  # 中间件加的头它都有，所以按白名单把这些头抄过来：
  #   X-Oneapi-Request-Id（NEWAPI middleware/request-id.go:16）
  #   X-New-Api-Version（NEWAPI middleware/cors.go:20，main.go:198 全局挂载）
  #   Vary，以及所有 Access-Control-* 头（NEWAPI router/relay-router.go:16 挂的 CORS；
  #     少了它们，浏览器端客户端会因为跨域校验失败而读不到 429 的内容）
  # 其他头一律不抄：SSE 专用的 Cache-Control、X-Accel-Buffering、逐跳头、Content-*、Date，
  # 以及 NewAPI 从上游转发过来的头——h3 在这条错误路径上都不会带这些
  body := json.Marshal(envelope{Error: openAIError{
            Message: e.Message, Type: e.Type, Param: e.Param, Code: e.Code /* startup 已做过 canonical 处理 */ }})
  if 序列化失败: 把 Code 换成 nil 再序列化一次      # 生产上走不到；先序列化再提交状态码，保证响应体一定是合法 JSON
  # 字段顺序 message, type, param, code，和 NEWAPI relaykit/types/error.go:13-19 一致
  # 用 json.Marshal（会转义 HTML、不加换行），和 gin 的 c.JSON 一致
  复制白名单里的头; w.Header().Set("Content-Type", "application/json; charset=utf-8")   # NEWAPI controller/relay.go:105
  w.WriteHeader(429)
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

const maxBase = 30 * time.Minute                  # 和 §6.4.1 的溢出推导是同一个上限（Caddy response_header_timeout 30m）

func (g *Gate) OnRateLimit(elapsed, floor time.Duration, streakCap int32)
  base := min(max(elapsed, floor), maxBase)      # floor = cfg.Gate.BaseFloor；用本地值算窗口，不再回读 g.base，
  g.base.Store(base)                             # 避免并发写者把别人的 base 拿来用
  s := 用 CAS 把 g.streak 加 1，但不超过 streakCap  # 存下来的计数也封顶：/v1/stats 看到的 streak ≤ streak_cap，
                                                 # 也不会因为一直累加而溢出 int32
  if s < 1: s = 1                                # 只有绕过 config.Validate（streakCap < 1）才会走到，防止 << 负数
  t := now + base << (s-1)                       # 窗口 1×、2×、4× base；base ≤ 30m 且 s ≤ 22 时不会溢出（§6.4.1）
                                                 # 实现里窗口和截止时间都做饱和处理：万一溢出，取 math.MaxInt64，不会翻成负数
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
  h.st.Inc(obs.Intercepted)
  for {
    t0 := now
    resp, err := h.forward(r.Context(), r, bytes.NewReader(body))
    if err != nil:
        if r.Context().Err() != nil: h.st.Inc(obs.ClientGone); return   # 客户端已断开：不写、不算作 NewAPI 故障
        h.st.Inc(obs.BadGateway); writeBadGateway(w); return            # 连不上 NewAPI：和现在 NewAPI 挂掉一样
    rid := resp.Header.Get("X-Oneapi-Request-Id")       # 只用于日志
    if resp.StatusCode != 200 || !isEventStream(resp):
        h.st.Inc(obs.Passthrough); passthrough(w, resp); return   # 包括 NewAPI 自己的 429：它已按 RetryTimes 重试过
    res := startup.Read(resp.Body)
    if res.Verdict == startup.VerdictRateLimit && newAPIFinalError(res.Err.Message, rid):
        # NewAPI 自己的终态错误：心跳先提交了 200，它按 RetryTimes 重试完以后把错误写进了流里，
        # 消息末尾带着本次请求的 id（NEWAPI controller/relay.go:96、:99-101、:242；common/utils.go:269）。
        # 和非 200 一样原样透传，不叠加重试（R2）
        h.st.Inc(obs.Passthrough); release(w, resp, res.Prefix, res.Rest); return
    if res.Verdict == startup.VerdictRelease:
        h.gates.ResetModel(meta.Model)                   # 这个模型下所有 key 的 streak 清零（§6.3）
        h.st.Inc(obs.Released); log release{request_id: rid, attempt: n}
        release(w, resp, res.Prefix, res.Rest); return
    resp.Body.Close()
    h.st.Inc(obs.RateLimited)
    cfg := h.store.Current()                             # 每次都读当前快照：热修改马上生效
    key := gate.Key(meta.Model, res.Err.Message)
    g := h.gates.Get(key)
    elapsed := now - t0
    g.OnRateLimit(elapsed, time.Duration(cfg.Gate.BaseFloorMS)*time.Millisecond, cfg.Gate.StreakCap)
    max := h.store.RetryMax(meta.Model)
    log intercept{request_id: rid, key, attempt: n, elapsed_ms: elapsed, max}
    if n >= max:
        h.st.Inc(obs.GaveUp); log give_up{request_id: rid, key, attempts: n+1}
        proxy429.Write(w, resp.Header, res.Err); return
    n++
    h.st.Inc(obs.Retries)
    w0 := now
    if g.Wait(r.Context()) != nil: h.st.Inc(obs.ClientGone); return   # 客户端已断开
    log wait{key, wait_ms: now - w0}
  }
```

`newAPIFinalError(msg, rid) = rid != "" && strings.HasSuffix(strings.TrimSpace(msg), " (request id: "+rid+")")`。`rid` 必须和本次响应头里的 `X-Oneapi-Request-Id` 完全相同：上游原样转发的帧不会带这个 id；中转站自己如果也跑 NewAPI，它的 id 和本站的不同，所以不会误判。

`ServeHTTP` 的不拦截分支处理 `forward` 错误的方式相同：ctx 已取消时只计 `ClientGone`，否则计 `BadGateway` 并回 502；成功时计 `Passthrough`。日志事件和字段见 §5，计数器见 §6.7。

在 `release` 或 `proxy429.Write` 之前，**不会**对 `w` 做任何写入（包括 Header 以外的写入和 Flush）。

### 1.7 重试上限热修改 `config.(*Store).SetRetryMax`

```text
func (s *Store) RetryMax(model string) int
  c := s.p.Load()
  if v, ok := c.Retry.PerModel[model]; ok { return v }
  return c.Retry.Max

func (s *Store) SetRetryMax(scope string, n int) (old int, err error)   # scope = "*" 或模型名
  if n < 0 || scope == "": return 0, ErrInvalidRetry
  s.wmu.Lock(); defer s.wmu.Unlock()        # 只把控制面的写入串行化；数据面只读 atomic，不经过这把锁
  cur := s.p.Load(); next := clone(cur)     # clone 深拷贝 Match.Paths、Match.Models、Retry.PerModel：已发布的快照绝不就地修改
  old = 修改前这个 scope 的生效值            # scope=="*" 取 cur.Retry.Max；模型名取 retryMaxFor(cur, scope)
  写入 next.Retry.Max 或 next.Retry.PerModel[scope]
  if err := next.Validate(); err != nil: return
  err = Persist(s.path, next)
  if err != nil && !errors.Is(err, ErrDirSync): return 0, err   # rename 之前失败：文件和内存都不变
  s.p.Store(next)                           # rename 已经完成：磁盘上已是新内容，内存必须跟上
  return old, err                           # err 为 ErrDirSync 时表示"已生效，但目录 fsync 失败，掉电后不保证持久"
  # Store 不打日志：uid 是控制口请求才有的信息，日志由 control 打（§6.4）

func (s *Store) UnsetRetryMax(model string) error         # 删除模型级配置，回到全局值。没有该模型的配置时返回 ErrRetryNotSet，不写盘；落盘的规则同上
func (s *Store) Reload() error                            # 由 SIGHUP 和 POST /v1/reload 触发：持 wmu，Load → Validate → 检查只能重启才生效的字段 → Store
  # listen、control_socket、newapi 只在启动时生效（§6.1 的 Transport 在构造时就固定了）。
  # 这三项和当前快照不一样时，返回 ErrRestartRequired，快照不变
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
  │◄─429 {"error":{…}}─│ proxy429.Write（白名单头取自最后一次尝试，§1.4）
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
| `forward(ctx, r, body)` | 用 `http.NewRequestWithContext` 新建请求：方法照抄；URL 是 `newapi` 地址加上 `r.URL` 的路径（含 `RawPath`）和 `RawQuery`，不设 `RequestURI`（客户端请求设了会报错）；`Host = r.Host`（Caddy 默认原样转发 Host，NewAPI 现在看到的就是它）；请求头去掉逐跳头后原样复制，再设 `Accept-Encoding: identity`（`startup.Read` 要读明文；走到 nahook 的只有 relay 路由，NewAPI 在 relay 路由上不压缩，gzip 只挂在 `NEWAPI/router/api-router.go:20`、`dashboard.go:13`、`web-router.go:23`）。请求体：`body` 是 `*bytes.Reader` 时，`ContentLength` 和 `GetBody` 由 `http.NewRequestWithContext` 自动设好（Go net/http 对 `*bytes.Reader` 的特殊处理）；否则（流式透传）手动设 `ContentLength = r.ContentLength`，不设 `GetBody`。**不改动** `X-Forwarded-For`、`X-Real-IP`、`Authorization` |
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
  - 400：`ErrInvalidRetry`、`ErrInvalidConfig`，或者请求 JSON 不合法；
  - 404：`ErrRetryNotSet`，也就是 `DELETE` 的模型本来就没有模型级配置；
  - 409：`ErrRestartRequired`，也就是 reload 改了 `listen`、`control_socket` 或 `newapi`（这时快照不变）；
  - 500：其他写盘错误（这时文件和内存都不变）。
- `ErrDirSync` 回 200，响应体里加 `"warning":"applied; directory fsync failed, not durable across power loss"`。这时修改已经生效。
- 日志：`PUT`、`DELETE`、`reload` 成功后，由 control 打 `config.retry_max_changed` 或 `config.reloaded`，字段包括 `scope`、`old`、`new`、`uid`。
  - `uid` 在 Linux 上通过 SO_PEERCRED 取（`syscall.GetsockoptUcred`，放在只在 linux 下编译的文件里）；其他平台记成 `-`。

### 6.4.1 `internal/config` 的校验和读写规则

`Load`：

- 用 `json.Decoder` 并调用 `DisallowUnknownFields`。不认识的键直接报错，防止键名写错后被静默当成 0。
- 一个完整的 JSON 对象之后如果还有别的内容，也报错。

`Persist`：

- 落盘时把空的 `per_model` 写成 `{}`，空的 `paths`、`models` 写成 `[]`，和 §5 的样例一致。
- 目录 fsync 失败时返回包装了 `ErrDirSync` 的错误。这时 rename 已经完成。

`(*Config).Validate`：

| 字段 | 规则 | 理由 |
|---|---|---|
| `listen` | 非空 | 数据面要监听 |
| `control_socket` | 非空 | 控制面要监听 |
| `newapi` | 绝对的 http 或 https URL | `forward` 要拼请求地址 |
| `body_limit` | > 0 | `classify` 最多读 `body_limit+1` 字节 |
| `gate.base_floor_ms` | 0–1800000 | 0 表示不设下限；上限 30 分钟和 `gate.maxBase` 相同，是下一行溢出推导的前提 |
| `gate.streak_cap` | 1–22 | 截止时间是 `now + base << (s-1)`（UnixNano），s 至少为 1。base 最大是 30m（1.8e12 ns），1.8e12 × 2²¹ ≈ 3.8e18；当前 UnixNano 约 1.8e18，两者相加约 5.6e18，小于 2⁶³ ≈ 9.2e18。换成 2²²（≈ 7.5e18）的话，相加后就溢出了 |
| `retry.max` | ≥ 0 | 0 表示不重试，直接返回代 429 |
| `retry.per_model` | 值 ≥ 0；键不能为空，也不能是 `*`（`*` 留给全局） | 和 `SetRetryMax` 的 scope 规则一致 |

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

- 参数 `-listen`，默认 `172.17.0.1:18080`。对外表现为一个 OpenAI 兼容上游：只接受 `POST /v1/responses` 和 `POST /v1/chat/completions`，不校验鉴权，也不看请求里的模型。
- `POST /__mode`，请求体形如 `{"script":["rate_limit","rate_limit","ok"]}`：替换脚本，同时清空计数和到达记录。之后每来一个业务请求就按顺序取一项，取完后一直重复最后一项。脚本为空、含有不认识的项或参数不合法时回 400，原脚本不变。
- `GET /__log`：`{"count":N,"requests":[{"seq":1,"path":"/v1/responses","item":"rate_limit","at_unix_ms":1760000000123}, …]}`，按到达顺序排列；`at_unix_ms` 是收到请求头的时刻。
- 取脚本项、加计数、记到达时间在同一把锁里完成。并发请求各拿一项，不重复也不遗漏（T-F4 用 16 个并发）。
- 流式响应：`200`，`Content-Type: text/event-stream`，每写完一帧就 flush。
  - Responses 的帧是 `event: <type>\ndata: <json>\n\n`。
  - Chat Completions 的帧只有 `data: <json>\n\n`，最后一帧是 `data: [DONE]\n\n`。
- 帧的内容全部固定，不带时间戳，也不带随机数：T-F5、T-F6 要逐字节对比两次请求的结果。`id` 固定为 `resp_mock` 或 `chatcmpl-mock`，`created` 固定为 `1760000000`，`model` 固定为 `gpt-6-astra`。
- 限流文案 `<RL>` 固定为 `Rate limit reached for gpt-6-astra in organization org-mock on tokens per min (TPM): Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms.`，句式来自 `渠道监控/429-vs-quota.md` 第 12–14 行。`org-mock` 能被 `gate.Key` 认出来。
- 错误对象 `<RLE>` 固定为 `{"type":"tokens","code":"rate_limit_exceeded","message":<RL>}`。

| 脚本项 | 适用路径 | 行为 |
|---|---|---|
| `ok` | 两条都适用 | Responses：`response.created` → `response.output_text.delta` × 3 → `response.completed`，后者带 `usage`（`input_tokens` 10、`output_tokens` 3、`total_tokens` 13）。Chat：3 个带 `delta.content` 的 chunk → 一个带 `finish_reason:"stop"` 和 `usage`（10/3/13）的 chunk → `[DONE]` |
| `slow:<秒>` | 两条都适用 | 和 `ok` 一样，但 delta 改成每秒发一个，一共发 N 个（T-C3） |
| `hold:<秒>` | 两条都适用 | 先不回响应头，等 N 秒后再按 `ok` 回（T-C1） |
| `rate_limit` | Responses | `response.created` → `{"type":"error","error":<RLE>}` → `{"type":"response.failed","response":{"id":"resp_mock","status":"failed","error":<RLE>}}` |
| `rate_limit_failed_only` | Responses | 只发上一行里那个 `response.failed` 帧（T-F14） |
| `hold_rate_limit:<秒>` | Responses | 先不回响应头，等 N 秒后再按 `rate_limit` 回（T-F15：NewAPI 在等上游响应头期间发 `: PING`，见 `NEWAPI/relay/channel/api_request.go:482`） |
| `stall_rate_limit:<秒>` | Responses | 先回 200 和响应头并 flush，N 秒内一帧都不发，然后按 `rate_limit` 发帧（T-F15：NewAPI 在流式阶段发 `: PING`，见 `NEWAPI/relay/helper/stream_scanner.go:126`） |
| `chat_rate_limit` | Chat | 第一帧就是 `{"error":<RLE>}`，然后是 `[DONE]`（T-F13） |
| `quota` | Responses | 和 `rate_limit` 一样，但错误对象换成 `{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}`（`429-vs-quota.md` 第 33 行：这类文案里没有 `try again in`）；不应该被拦截（T-F12） |
| `http429` | 两条都适用 | 直接回 HTTP 429，`Content-Type: application/json`，响应体为 `{"error":{"message":<RL>,"type":"tokens","param":"","code":"rate_limit_exceeded"}}`（T-F7） |
| `hold_http429:<秒>` | 两条都适用 | 先不回响应头，等 N 秒后再按 `http429` 回（T-F17：NewAPI 的心跳先提交了 200，最终错误会以 SSE 帧的形式写出） |

- 脚本项和请求路径不匹配时（比如 `rate_limit` 碰上 chat 路径），回 400 `{"error":{"message":"mockrelay: item <项> does not apply to <路径>"}}`。这次请求照样记进 `/__log`。

### 6.7 `internal/obs`

| 函数 / 类型 | 说明 |
|---|---|
| `NewLogger(w io.Writer) *slog.Logger` | `slog.New(slog.NewJSONHandler(w, nil))`；`cmd/nahook` 传 `os.Stdout`，由 journald 收集 |
| `type Counter int` | 计数器编号；下表每一行一个常量 |
| `NewStats() *Stats` | 每个计数器一个 `atomic.Int64`，全部从 0 开始；进程重启即清零 |
| `(*Stats).Inc(c Counter)` | 原子加 1；数据面调用，不拿锁 |
| `(*Stats).Snapshot() map[string]int64` | 逐个 `Load`，键用下表的名字，所有计数器都在（值为 0 也列出）；给 `GET /v1/stats` 的 `counters` 用 |

| 常量 | 键名 | 什么时候加 1 |
|---|---|---|
| `Intercepted` | `intercepted` | `classify` 判为拦截，进入 `serveIntercept` |
| `RateLimited` | `rate_limited` | 一次尝试的启动帧被判为限流 |
| `Retries` | `retries` | 判为限流后还没到上限，进入 `Wait` |
| `Released` | `released` | 拦截路径上放行 |
| `GaveUp` | `gave_up` | 返回代 429 |
| `Passthrough` | `passthrough` | 不拦截的请求转发成功；或拦截路径上 NewAPI 回了非 200 或非 SSE |
| `BadGateway` | `bad_gateway` | 连不上 NewAPI，回了 502 |
| `ClientGone` | `client_gone` | `forward` 或 `Wait` 因为 `r.Context()` 取消而返回 |

不变量：`rate_limited = retries + gave_up`（每次判为限流后，恰好走这两条路中的一条）；每个被拦截的请求恰好以 `released`、`gave_up`、`passthrough`、`bad_gateway`、`client_gone` 之一结束。

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
| 只要 200 SSE 里出现限流帧就重试 | 打开心跳时，NewAPI 会先提交 200，按 RetryTimes 重试完以后再把自己的终态错误写进流里；nahook 再重试一轮，两层重试就叠加了 | 消息以 ` (request id: <本次的 X-Oneapi-Request-Id>)` 结尾时原样放行（§1.6；`NEWAPI/controller/relay.go:99-101`） |
