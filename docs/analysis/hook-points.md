# 流式与非流式：每一层能拿到什么，限流转 429 能挂在哪

基线版本：Caddy v2.11.4（官方构建，只有 132 个标准模块，没有插件）、NewAPI v1.0.0-rc.45（测试环境在跑的版本）、nahook `dev`。

出处写法：

- NewAPI：`文件:行号@rc.45`。
- nahook：`文件:行号`（当前 dev）。
- Caddy：`文件:行号@v2.11.4`，路径省略 `modules/caddyhttp/`。
- Go 标准库：`【Go】文件:行号`。
- 社区：`caddy#号`、`new-api#号`。
- 标【推论】的条目是从源码推出来的，还没在测试环境实测，第 10 节列了对应的实测项。

---

## 0. 结论

1. **Caddy 原生读不到响应体，也拦不住 SSE 的 200。**
   - `handle_response`、`intercept`、`replace_status`、`lb_retry_match`、`unhealthy_status` 都只看状态码和响应头。
   - 上游 SSE 的响应头一到，Caddy 就把 200 提交给客户端（`reverseproxy/streaming.go:247-251、302-304@v2.11.4`）。
   - 所以流式里"写在 200 体内的限流"，只能由 Caddy 的上游、而且自己先不写字节的一层来处理。这就是 nahook 现在的位置。
   - 唯一的替代位置是自己编一个 Caddy transport 插件（4.6），不推荐。
2. **非流式的限流本来就是 HTTP 429**（NewAPI 用完 `RetryTimes` 之后）。Caddy 原生能看到状态码和头，能补头、能换错误页，但读不到体。上游给的 `Retry-After` 在 NewAPI 这一层就丢了（2.3）。
3. **流式限流有三种形态**（第 3 节）：
   - **S5**：上游先回 200，再在启动帧里报限流。这是 nahook 现有的功能。
   - **S6**：上游在 NewAPI 发心跳之前就回了 HTTP 429。客户端收到的已经是 429。
   - **S7**：NewAPI 的心跳先提交了 200，之后的终态错误写在流里。客户端现在收到的是 200。
   - 测试环境的心跳是 1 秒，所以上游稍慢的限流都落在 S7。**S7 是离"限流一律变 429"最近、也最大的缺口**：nahook 还没向客户端写过字节，完全可以改成 429，而且不用重试（9.1）。
4. **头部信息：** 流式响应和所有错误响应都不带上游的限流头，nahook 代发的 429 也不带 `Retry-After`。补 `Retry-After` 有两条路：
   - nahook 按冷却门的剩余时间给，值是准的；
   - Caddy 给一个固定默认值，值是粗的，而且 Caddyfile 的写法有坑（4.4，已在测试环境用 `caddy adapt` 确认）。
5. **Caddy 2.11.4 和 2.6.2 不一样了：** POST 在 dial 失败时的重试已经修好（caddy#7547、PR #7715、#7719）。"nahook 路由不写 `lb_try_duration`"这条规则当初是在 2.6.2 上实测出来的，需要在 2.11.4 上重测（第 10 节 V3）。
6. **社区在做同一件事：** NewAPI 上游有 new-api#3142（open）和 PR new-api#6446（open），要在流式启动阶段把限流变回 429 并重试。它合入以后，会和 nahook 的重试叠加，需要跟踪（9.6）。

---

## 1. 链路上的四个位置

```text
客户端
  │  HTTPS
  ▼
Caddy ─────────── ① 入口代理：原生只看 HTTP 层（方法、路径、查询串、请求头、状态码、响应头）；
  │                   请求体只能用 {http.request.body} 整体读进内存（4.3）
  │  POST /v1/responses、/v1/chat/completions
  ▼
nahook ────────── ② 入站旁路：完整请求体 + NewAPI 的响应头 + SSE 启动帧；
  │                   决定之前对客户端一个字节都没写，所以还能改状态码
  ▼
NewAPI ────────── ③ 中继：选渠道、RetryTimes、心跳、计费；不改源码时只能调面板参数
  │  渠道 Base URL
  ▼
（出站旁路）────── ④ 可选，目前没有部署：挂在渠道 Base URL 上，看到上游的原始响应
  │                   （HTTP 429 和上游的头，或者 SSE 帧）；回给 NewAPI 什么，决定它重不重试、换不换渠道
  ▼
上游（中转站 / OpenAI）
```

**状态码只能在第一个字节写出之前改。** 流式请求里，某一层一旦向下游写了字节（NewAPI 的心跳、第一个事件），它下游的各层就再也改不了这一跳的状态码。

nahook 的做法是自己先不写：NewAPI 对 nahook 已经提交了 200，但 nahook 对 Caddy 和客户端还没有，所以 nahook 这一跳的状态码还能定。Caddy 做不到这一点，因为它收到 SSE 响应头就立即转发。

---

## 2. NewAPI rc.45 实际发出什么

### 2.1 流式请求

```text
Relay 循环（controller/relay.go:162@rc.45），每次尝试：
  doRequest
    SetEventStreamHeaders（relay/channel/api_request.go:550-551）
        只往响应头里写 Content-Type: text/event-stream 等，不提交
    心跳协程（面板开了心跳才有，api_request.go:556）
    relayClient.Do(req) 等上游的响应头（api_request.go:569）
      └ 每隔 ping_interval 写一次 ": PING"（api_request.go:479）──► 第一次写 = 向下游提交 200
  上游回 HTTP 非 200
    └ RelayErrorHandler（service/error.go:87）读上游的体，生成 NewAPIError：
        状态码 = 上游状态码，再经渠道"状态码映射"改写（service/error.go:140）
        上游的响应头一个都不带出来
    └ DecideRelayRetry（service/relay_error.go:20）只看状态码；可重试就进入下一次尝试（会换渠道）
  上游回 200 SSE
    └ StreamScannerHandler（relay/helper/stream_scanner.go:83）逐帧转发；空闲时照样发心跳（:182）
    └ 上游的头只抄 X-Reasoning-Included、X-Codex-Turn-State（stream_scanner.go:55-71、150）
    └ 帧里的 error、response.failed 原样转发，handler 返回 nil
      （relay/channel/openai/relay_responses.go:70-99）
      ──► 对 NewAPI 来说这是成功：不重试，不换渠道
Relay 的 defer（controller/relay.go:92-110）：有 NewAPIError 时
    先在 message 末尾加 " (request id: <rid>)"（:96），再 c.JSON(状态码, {"error":…})（:107）
    ├ 还没提交过：状态码就是上面那个码；Content-Type 仍是 text/event-stream，
    │            因为 gin 只在没有 Content-Type 时才设 application/json（第 10 节 V1 实测）
    └ 已经提交过（心跳写过）：状态码写不出去，JSON 原样接在心跳后面，没有 "data:" 前缀
                             （测试环境实测：T10 T-F17 的 rc.45 部分，见 spec/specaudit.md Q20）
```

`#7857` 之后（rc.45 不含，h3 含），这个 defer 改成了：

- 已提交时写 `data: {"error":…}` + `data: [DONE]`；
- 未提交时显式设 `Content-Type: application/json`。

### 2.2 非流式请求

```text
Relay 循环，每次尝试：
  doRequest：不设 SSE 头，没有心跳
  上游回 HTTP 非 200 ──► 同 2.1：RelayErrorHandler → 状态码映射 → DecideRelayRetry
  上游回 200，但体是 {"error":{"type":"…",…}}
    └ OpenaiHandler（relay/channel/openai/relay-openai.go:282）、OaiResponsesHandler（relay_responses.go:35）
      生成 NewAPIError，状态码 = 200
    └ DecideRelayRetry：2xx 一律不重试（service/relay_error.go:44）
  上游回 200 正常体 ──► IOCopyBytesGracefully（service/http.go:44）：上游的头全部照抄
Relay 的 defer：c.JSON(状态码, {"error":…})；Content-Type 是 application/json，没有 Retry-After
```

### 2.3 头里有什么

| 情况 | NewAPI 给下游的头 | 依据 |
|---|---|---|
| 任何响应 | `X-Oneapi-Request-Id`（本次请求的 rid）、`X-New-Api-Version`、CORS 头 | middleware/request-id.go、cors.go |
| 流式成功 | 上游的头只抄 `X-Reasoning-Included`、`X-Codex-Turn-State` | relay/helper/stream_scanner.go:55-71@rc.45 |
| 非流式成功 | 上游的头全部照抄（除了 `Content-Length`、`X-Oneapi-Request-Id`）；上游给了 `x-ratelimit-*` 就会带下来 | service/http.go:31-61@rc.45 |
| 中继错误（流式、非流式） | 不抄上游任何头，**没有 `Retry-After`** | service/error.go:87-138、controller/relay.go:92-110@rc.45 |
| NewAPI 自己的全局限流 | 429 + `Retry-After`，空体 | middleware/rate-limit.go:139-146@rc.45 |
| NewAPI 自己的模型限流 | 429 + OpenAI 格式的 JSON | middleware/model-rate-limit.go:96、120、145、153@rc.45 |

所以，**上游的限流信息（剩余额度、重置时间、`Retry-After`）在流式响应和错误响应里都到不了 Caddy，只剩体里的 message**（例如 "Please try again in 1.2s"）。

---

## 3. 场景矩阵：每一层看到什么

| # | 场景 | Caddy 看到的状态 / Content-Type | 限流信息在哪 | Caddy 原生能识别 | nahook 现在的处理 | 能不能变成 429 |
|---|---|---|---|---|---|---|
| S1 | 非流式，成功 | 200 / json | 成功体；上游给了才有 `x-ratelimit-*` 头 | — | 透传（classify 不拦） | — |
| S2 | 非流式，上游 HTTP 429，RetryTimes 用完 | 429（或映射后的码）/ json | 体的 message、code | 能（状态码） | 透传 | 已经是 429 |
| S3 | 非流式，上游 200 + `{"error":…}` 体 | **200** / json | 体 | 不能 | 透传 | 只有读体的一层能改 |
| S4 | NewAPI 自己限流（全局 / 模型） | 429 | 头（全局限流有 `Retry-After`）或体 | 能 | 透传 | 已经是 429，而且不该重试 |
| S5 | 流式，上游 200，启动帧里限流 | 200 / event-stream | SSE 体（`error`、`response.failed`、Chat 的 `{"error":…}` 块） | **不能** | 拦截：冷却后重试，用完上限代 429 | nahook（现有功能） |
| S6 | 流式，上游很快回 HTTP 429（心跳还没发，或心跳关着），RetryTimes 用完 | 429 / **event-stream**（rc.45，V1 实测）或 json（`#7857` 起） | 体（裸 JSON） | 能（状态码） | 透传（intercept.go:41） | 已经是 429 |
| S7 | 流式，心跳已提交 200，之后上游 HTTP 429，RetryTimes 用完 | 200 / event-stream | `: PING`… 之后的裸 JSON（rc.45），或 `data:{"error"}` + `[DONE]`（`#7857` 起） | 不能 | rc.45：读到 EOF 放行；`#7857` 起：规则 2 透传 | **能，但现在没做**：nahook 还没向客户端写字节（9.1） |
| S8 | 流式，首个 delta 之后才限流 | 200 / event-stream | SSE 体 | 不能 | 已放行 | 任何一层都不能 |
| S9 | 流式 Claude `/v1/messages`、Gemini | 200 / event-stream | `event: error` + `rate_limit_error` 等 | 不能 | 不经过 nahook（路径不在 match.paths） | 要扩展（9.4） |
| S10 | 流式，请求体超过 `body_limit` 或不是 JSON | 同 S5–S8 | — | — | 透传 | 不处理 |

测试环境面板的心跳是 1 秒：上游在 1 秒内没回响应头的请求，限流都会落在 S7，而不是 S6。

---

## 4. Caddy 层：能拿到什么，能挂在哪

### 4.1 请求在 Caddy 里经过的位置

```text
client ─► server：PrepareRequest，建 replacer
       ─► 路由匹配器：执行到这条路由时才求值；同一组里多个匹配器的顺序不确定（routes.go:256-282、441-458）
       ─► request_body / header / encode / intercept（包住 ResponseWriter）
       ─► reverse_proxy.ServeHTTP
             prepareRequest：克隆请求；request_buffers
             循环：选上游 → Transport.RoundTrip ──拿到响应头──►
                   被动健康（状态码、延迟）→ response_buffers → 设置 {rp.*}
                   → lb_retry_match → handle_response / replace_status
                   → finalizeResponse：WriteHeader + copyResponse（SSE 立即 flush）
             出错 → tryAgain → 重试，或者 statusError → handle_errors
```

### 4.2 每个阶段能拿到什么

| 阶段 | 能拿到的信息 | 钩子 | 流式 | 非流式 | 出处 |
|---|---|---|---|---|---|
| 请求匹配 | method、path、host、header、query、protocol、tls、remote_ip、client_ip、vars；`expression`（CEL，能用全部占位符） | 命名匹配器 | ✔ | ✔ | matchers.go、celmatcher.go:165-178 |
| 请求体 | `{http.request.body}`、`{http.request.body_base64}`（2.11.0 起）：整体读进内存 | expression、header_up、log_append | ✔ | ✔ | replacer.go:217-245 |
| 选上游、重试 | 上游地址、在途数、失败数；错误类型；`{rp.is_transport_error}` | lb_policy、lb_retries、lb_try_duration、lb_retry_match、request_buffers | 只在响应头之前 | ✔ | reverseproxy/reverseproxy.go:603-729、1324-1398 |
| 响应头（代理内） | `{rp.status_code}`、`{rp.status_text}`、`{rp.header.*}`、`{rp.upstream.latency}` | handle_response（`status`、`header` 匹配器）、replace_status、header_down、lb_retry_match、unhealthy_status | 能用，但流式出错时状态还是 200 | ✔ | reverseproxy.go:1076-1161 |
| 响应头（代理外） | 写给客户端的状态和头 | `header`（defer、match、`?`、`>`）、`intercept`（`{resp.*}`） | ✔（intercept 一旦匹配就整体缓冲） | ✔ | headers/headers.go:387-401、intercept/intercept.go:128-201 |
| 响应体 | **没有"读体再决定"的钩子**。copy_response 只转发；`log_append {http.response.body}` 只写日志且整体缓冲；templates 只处理 html、plain、markdown | — | ✗ | ✗ | copyresponse.go:58-84、logging/logappend.go:63-115 |
| 错误 | `{err.status_code}`、`{err.message}`、`{err.id}`、`{rp.*}` 的残值 | handle_errors | 只在响应头写出之前出错时触发；流开始后出错直接断连 | ✔ | server.go:778-800、reverseproxy.go:1273-1284 |
| 健康 | 被动：fail_duration、max_fails（默认 1）、unhealthy_status、unhealthy_latency、unhealthy_request_count | 同左 | latency 只计到响应头；request_count 把在途的 SSE 也算进去 | ✔ | reverseproxy.go:404-411、1084-1098、1442-1453 |

### 4.3 请求体：`{http.request.body}`

```go
// replacer.go:217-230@v2.11.4
case "http.request.body":
    if req.Body == nil { return "", true }
    defer req.Body.Close()
    buf := new(bytes.Buffer)
    _, _ = io.Copy(buf, req.Body) // can't handle error, so just ignore it
    req.Body = io.NopCloser(buf)  // replace real body with buffered data
    return buf.String(), true
```

- 读完会把体换成内存缓冲，后面的 `reverse_proxy` 还能完整发出一次。
- 没有上限，官方注释写着只用于调试（app.go:53-54）。
- 和 `request_body max_size` 一起用时，体会被**静默截断**，不会返回 413（caddy#7691，修复在 v2.11.6，2.11.4 里还有）。
- 在 Caddy 里按 `"stream": true` 或模型分流**是写得出来的**。下面这段已在测试环境用 `caddy validate` 和 `caddy adapt` 确认能编译：

  ```caddy
  handle @relay {                  # 外层先用 method + path 收窄
  	@sse expression `{http.request.body}.matches('"stream"\\s*:\\s*true')`
  	reverse_proxy @sse 127.0.0.1:19080 localhost:3000 { … }
  	reverse_proxy localhost:3000
  }
  ```

  代价是每个命中的请求都要把整个体读进内存，再跑一遍正则；带图片的请求体可能有好几 MB。正则里不要写 `{n}` 这类量词：以字母开头的 `{…}` 会被当成占位符。

  V4 实测（临时 Caddy）：流式请求（包括 `"stream" : true` 和 300KB 的请求体）走 nahook；非流式请求（包括 300KB）直达 NewAPI，请求体完整。正则看不懂 JSON 结构：顶层 `"stream": false`、只在 `metadata` 里有 `"stream":true` 的请求也会被送进 nahook，再由 nahook 判为非流式、原样透传，结果不受影响，只是多走一跳。
- **结论：分流交给 nahook 的 `classify`（N1），Caddy 只按 method + path 收窄。** 两边都读体没有收益。

### 4.4 响应：只有状态码和头

- `handle_response`：
  - 匹配器只有 `status` 和 `header`（reverseproxy/responsematchers.go:27-45）；
  - 读不到响应体：加体占位符的 PR #7797 没合入就关了，caddy#4420、#6914 还开着；
  - 匹配后如果路由里没有 `copy_response`，路由结束就关掉上游体（reverseproxy.go:1190-1192），对 SSE 来说就是断流；
  - 在它里面再 `reverse_proxy` 到别处时，原请求体已经被读掉了，开了 `request_buffers` 也一样【推论】。
- `intercept`：
  - 从 v2.8.0 开始有，标为实验功能；
  - 一旦匹配就整体缓冲到上游结束，SSE 会被憋住；
  - 2.11.4 里它的 `replace_status` 会被静默丢掉（caddy#7805，修复在 v2.11.6）。
- `header` 指令补 `Retry-After`（测试环境 `caddy adapt` 的结果，V2 实测证实）：

  | 写法 | 编译结果 | 效果 |
  |---|---|---|
  | `header { match { status 429 }  ?Retry-After 5 }` | **拆成两个 handler**：一个只要求 429、什么都不做；另一个只要求"没有 Retry-After"，然后设成 5 | **所有没有 `Retry-After` 的响应都会被加上，包括 200**：坑 |
  | `header { match { status 429 }  >Retry-After 5 }` | 一个 handler：`deferred` + 要求 429 + 设成 5 | 只对 429 生效，但会覆盖 NewAPI 全局限流自己给的 `Retry-After` |
  | JSON：`"require": {"status_code": [429], "headers": {"Retry-After": null}}` | — | "429 且没有时才补"只能用 JSON 写，Caddyfile 表达不了【推论】 |

### 4.5 重试与健康

- **什么时候重试**（tryAgain，reverseproxy.go:1324-1398）：
  - 没配 `lb_retries` 和 `lb_try_duration`：永不重试（:1326-1328）。我们现在就是这样。
  - dial 失败：任何方法都重试。
  - 其它往返错误（连接重置、`response_header_timeout` 等）：只重试 GET，除非配了 `lb_retry_match`。
  - 按状态码重试：`lb_retry_match { expression `{rp.status_code} == 429` }` 从 v2.11.3 开始有（PR #7569）。但 2.11.4 有个坑：只引用请求字段的 expression 会让成功的响应也被重试，最后返回 502（caddy#8166，修复还没发版）。
- **`request_buffers`**：
  - 体积 ≤ N 时整体缓冲，每次重试都从缓冲重放（reverseproxy.go:535-547、568-570）。这个能力从 v2.11.0 才真正生效（PR #7360）。
  - 体积 > N 时只缓冲一部分，不能重放。
  - 能重放不等于语义安全：上游可能已经把请求交给模型，重放会重复计费。
- **我们以前看到的 "body closed by handler"**：
  - 这是 Go 的 HTTP/2 服务端请求体被关以后再读时报的错（【Go】h2_bundle.go:4124、6628-6638），客户端经 h2 连到 Caddy 时出现。HTTP/1 下同一个问题报的是 "invalid Read on closed Body"，即 caddy#5454：2023 年以 invalid 关闭，答复是"POST 要重试就先缓冲请求体"。
  - 当时是 2.6.2：第一次尝试时 Transport 关掉了体，第二次尝试读体失败。这算往返错误，于是 NewAPI 也被记了一次失败。
  - 2.11.4 已修好 dial 失败时的关体问题（caddy#7547 → v2.11.2；PR #7715、#7719 → v2.11.4）。**所以 nahook 没起来时，`lb_try_duration` 很可能已经能安全地把 POST 转给 NewAPI**【推论，第 10 节 V3】。
- **`response_header_timeout`**：
  - 从请求（含体）写完开始计时，到收到响应头为止（【Go】transport.go:2925-2931）；之后不计时，SSE 的时长不受限制。
  - nahook 持着响应头等冷却的时间，全部算在这 30 分钟里。
- **`unhealthy_status 429` 不要配**：nahook 回一个 429，就会被标为不可用 10 秒，期间 `first` 策略把流量全部绕过 nahook【推论】。`unhealthy_request_count` 也会把长时间的 SSE 算成在途请求。
- `stream_timeout`、`stream_close_delay` 只管 101 升级的连接，对 SSE 无效（streaming.go:216-217）。

### 4.6 插件路线（自编 Caddy）

| 位置 | 做法 | 难点 |
|---|---|---|
| transport 插件（较干净） | 包装 `HTTPTransport`，在 `RoundTrip` 里遇到 200 + event-stream 就读首帧（跳过 `: PING`）；是限流就关掉上游体，合成一个 `*http.Response{429}`；否则用 `MultiReader(已读部分, 剩余部分)` 放行。之后 Caddy 自己的 handle_response、handle_errors、lb_retry_match、被动健康看到的都是"真 429" | 要用 xcaddy 构建，替换官方二进制；读首帧要自带超时（`response_header_timeout` 管不到）；重放要靠 `request_buffers`；冷却门、重试上限、控制口都得在插件里重写一遍 |
| handler 插件 | 自己写 ResponseWriter，先攒住前几帧再决定 | `ResponseRecorder` 只在 WriteHeader 时决定一次是全部缓冲还是全部直通（responsewriter.go:137、157-162），做不到"攒几帧再决定"；要实现 `FlushError`，否则 Flush 会穿透下去提前提交 200；要自己 recover `ErrAbortHandler`；请求体要自己缓冲 |

**结论：** 插件能做，但等于把 nahook 搬进 Caddy，同时丢掉官方二进制和 `apt-mark hold` 的版本控制。现阶段不推荐。

### 4.7 Caddy 原生能做的事，和不要配的项

| 能做 | 写法 | 场景 |
|---|---|---|
| 非流式 429 透传 | 默认行为 | S2、S4、S6 |
| 只给 429 加 `Retry-After` | `header { match { status 429 }  >Retry-After N }`（不要用 `?`，见 4.4） | S2、S6，以及 nahook 的代 429 |
| nahook 不可用时的错误页 | `handle_errors` + `{err.status_code}` | 两个上游都不可用 |
| nahook 没起来时自动转给 NewAPI | `lb_try_duration`（2.11.4 待重测，V3） | nahook 重启的那几秒 |

| 不要配 | 原因 |
|---|---|
| `unhealthy_status 429` | 会把 nahook 标坏，流量绕过 nahook |
| `lb_retry_match {rp.status_code} == 429` | 会把 nahook 的代 429 重试到 NewAPI，等于绕过 nahook；POST 还需要 `request_buffers`；2.11.4 还有 caddy#8166 |
| `flush_interval -1` | 不需要：SSE 默认就立即 flush |
| 在 Caddy 里按请求体分流 | 见 4.3 |

---

## 5. nahook 层的挂接点

| 编号 | 位置 | 现在做什么 | 拿得到的信息 | 可以扩展的方向 |
|---|---|---|---|---|
| N1 | `classify` proxy.go:127-153 | POST + 路径精确匹配 + 体 ≤ `body_limit` + `"stream": true` + 模型精确匹配 | 完整请求体、全部请求头 | 模型通配或前缀；按令牌、分组或请求头开关；识别 Claude、Gemini 的流式请求；让非流式请求也进入拦截（9.3） |
| N2 | `forward` proxy.go:199-240 | 每次尝试都是一个全新的 NewAPI 请求，体原样 | 同 N1 | 告诉 NewAPI 这是第几次尝试（例如 `X-Nahook-Attempt`），方便对日志 |
| N3 | 状态码 / Content-Type 分支 intercept.go:40-46 | 非 200 或非 SSE 一律透传 | NewAPI 的状态码、全部响应头、完整体（非流式） | S2、S4、S6 的 429 加 `Retry-After`、统一体格式；S3 识别后改成 429 |
| N4 | `startup.Read` startup.go:102；窗口 startup.go:20-27（256KiB、128 个事件） | 读启动帧，决定放行还是限流 | 心跳、事件名、data、启动阶段的全部字节 | 识别 rc.45 心跳后的裸 JSON（S7）；识别 Claude 的 `event: error` |
| N5 | `IsRateLimit` error.go:89-96 | 固定规则：code 429、`rate_limit_exceeded` 等、"rate limit reached" | 错误的 code、type、message | 规则放进配置；决定"过载"（`server_is_overloaded`、529）是否按限流处理 |
| N6 | `newAPIFinalError` intercept.go:128-133 | 认出 NewAPI 自己的终态错误后透传，避免两层重试叠加 | 同 N5，再加上 rid 和"前面只有心跳" | 透传改成"不重试，直接代 429"（9.1） |
| N7 | 冷却门 gate.go:46（OnRateLimit）、:92（Wait）；键 registry.go:16 | 按"模型 + 上游 org"冷却，全抖动 | 每个键的冷却截止时间 | 代 429 时把剩余冷却时间作为 `Retry-After`（9.2）；键里加令牌或分组 |
| N8 | 重试上限 intercept.go:83-91，控制口 `/v1/retry` | 全局设一个，或按模型设 | — | 按路径、按令牌设 |
| N9 | `proxy429.Write` proxy429.go:54-80 | 429 + NewAPI 格式的体；按白名单抄头；不加 `Retry-After` | 最后一次尝试的头和错误 | 加 `Retry-After`（9.2）；Claude 格式的 429 体（9.4） |
| N10 | `release` / `passthrough` stream.go:16-28 | 前缀 + 后续流原样写出 | 放行之后的所有字节 | 只能观察（计数、日志），不能再改状态码 |
| N11 | 计数器 obs.go、日志、`nahookctl` | 8 个计数器 | — | 按键计数；`nahookctl config show`（Q22） |

非流式请求今天在 N1 就被分到透传（proxy.go:94-105），根本不会进 `serveIntercept`。

---

## 6. 出站旁路（渠道 Base URL）

早先的调研（[hook-without-fork.md](../reference/渠道监控/hook-without-fork.md) 第 3 节）给过另一种位置：把渠道的 Base URL 指向一个本地进程，由它再转给真正的上游。

```text
NewAPI ──渠道 Base URL──► 出站旁路 ──► 上游
                            │ 上游已经回了 HTTP 429：原样回给 NewAPI（带着上游的 Retry-After）
                            │ 上游回 200 SSE：读启动帧
                            │   命中限流 ──► 回给 NewAPI 一个 HTTP 429（SSE 不交给 NewAPI）
                            │   没命中   ──► 读过的字节 + 后续流交给 NewAPI
                            ▼
NewAPI 看到的是 HTTP 429 ──► DecideRelayRetry ──► 按 RetryTimes 重试，可以换到别的渠道
```

| | 入站（nahook，现在） | 出站（渠道 Base URL） |
|---|---|---|
| 看到的东西 | NewAPI 的输出：心跳、改写过的错误、NewAPI 的头 | 上游的原始响应：上游的头（`Retry-After`、`x-ratelimit-*`）、原始 SSE |
| 重试 | 整单重发给同一个 NewAPI，NewAPI 每次独立选渠道 | 交给 NewAPI 的重试循环，可以换渠道 |
| 心跳 | 不受影响：心跳被 nahook 攒住，客户端看不到 | NewAPI 在等出站旁路时照样发心跳；最后失败时，终态错误仍在流里（S7） |
| 部署 | Caddy 一条路由 | 每个渠道都要改 Base URL，再用 Header Override 告诉旁路真正的上游 |
| 适合 | 同站重试；对客户端保证"要么成功，要么 429" | 多渠道之间换站 |

两者不冲突：出站负责"换站"，入站负责"最后给客户端一个 429"。

---

## 7. NewAPI 面板参数（不改代码能调的）

| 参数 | 影响哪些场景 | 说明 |
|---|---|---|
| 失败重试次数 `RetryTimes` | S2、S6、S7 | 只对 HTTP 层错误起作用；S5 的限流对 NewAPI 来说是成功，不会重试 |
| 心跳开关和间隔 | S6 和 S7 的分界 | 关掉心跳，S7 全部变成 S6（真 HTTP 429）；代价是长时间没有输出时，客户端或中间代理可能断开 |
| 渠道"状态码映射" | S2、S6 | 把上游的某个状态码改成另一个；200 不参与映射（service/error.go:152@rc.45） |
| 自动重试状态码范围、永不重试的状态码 | S2、S6、S7 | 决定 `DecideRelayRetry` 对哪些码重试 |

---

## 8. 社区 issues

### 8.1 Caddy

| 编号 | 内容 | 状态 / 版本 | 和我们的关系 |
|---|---|---|---|
| #5454 | POST 负载均衡重试报 "invalid Read on closed Body"（v2.6.1） | 2023 年以 invalid 关闭，没修 | 我们 2.6.2 上 "body closed by handler" 的 HTTP/1 版本 |
| #7546 → PR #7547；#7713 → PR #7715、PR #7719 | dial 失败重试时请求体被关 | v2.11.2；v2.11.4 | 2.11.4 上 dial 重试已修，`lb_try_duration` 值得重测（V3） |
| PR #7360 | 带请求体的请求重试修复 | v2.11.0 | `request_buffers` 的重放从这里起真正可用 |
| #6267 → PR #7569 | `lb_retry_match` 可以按响应状态重试 | v2.11.3 | 能按 429 重试，但对我们的拓扑有害（4.7） |
| #8166 → PR #8168 | 只看请求字段的 expression 让成功的响应也被重试，返回 502 | 已修，未发版 | 2.11.4 里写 `lb_retry_match` 的大坑 |
| #7691 → PR #7969 | `{http.request.body}` 在 max_size 处静默截断 | v2.11.6 | 4.3 |
| #8206 → PR #8207 | `request_buffers unlimited` 转发截断的体 | 已修，未发版 | 不要用 unlimited |
| PR #6232 | 新增 `intercept` | v2.8.0 | 4.4 |
| #7805 → PR #7810 | `intercept` 的 `replace_status` 被静默丢掉 | v2.11.6 | 2.11.4 的 intercept 不可靠 |
| #4298、#6490；PR #7797（未合入）；#4420、#6914（open） | handle_response 读不到响应体 | — | 原生读不到响应体 |
| #4922 → PR #4952；#6666、#6669 回退 | `flush_interval -1` 时忽略客户端断开 | v2.6.0 引入，v2.9.0 回退 | 解释 2.6.2 上断开传不到后端 |
| PR #6471 | encode 在 flush 时冲刷压缩器 | v2.9.0 | 解释 2.6.2 上 encode + SSE 卡住 |
| #6293 → PR #7905 | encode 会把响应头憋到第一个字节才发 | v2.11.6 | 2.11.4 默认路由上客户端要等到第一个字节才看到 200；nahook 路由的响应头和前缀一起写出，不受影响 |
| #8196 | Go 1.26 下上游提前响应可能让 HTTP/1 连接错位 | open | nahook 只在请求体超过 `body_limit` 时边读边转，NewAPI 才可能提前响应；风险低，留意 |
| #6537 | 客户端走 HTTP/3 时 SSE 有问题 | open | 站点开了 h3 时留意 |
| #8143 | reload 时在途请求的模块被清理 | open | 长时间的 SSE 赶上 `systemctl reload caddy` 时留意 |

### 8.2 NewAPI / one-api

| 编号 | 内容 | 状态 | 和我们的关系 |
|---|---|---|---|
| new-api#3142 | Responses 流失败时，第一次向下游写之前不要提交 200 | open | 和 nahook 要解决的是同一件事 |
| new-api#6446 | 重试"预流阶段"的限流 SSE 错误 | PR open | 合入后 NewAPI 自己会重试 S5，和 nahook 叠加（9.6） |
| new-api#6523 | 流错误检测与渠道回退 | PR open | 同类 |
| new-api#2989、#7957、#7958、#6594 | 流式空回、只有 metadata 的 EOF、Responses 失败被记成成功 | open | 流式错误被当成功的同一类问题 |
| new-api#7857 | Claude 流在准备和重试期间保活 | 已合入（rc.45 之后） | 改变了 S7 的形态（2.1） |
| new-api#6981、#6982 | 活跃流里上游的注释心跳被丢弃；保留它们的修复 PR | open；PR 未合入就关了 | 心跳语义 |
| new-api#5049 | 中继重试应使用退避、抖动和 Retry-After | open | NewAPI 不利用 Retry-After（2.3） |
| new-api#7660 | 上游 429 带重置时间时临时封禁渠道 | open | 同上 |
| new-api#6560 | 429 返回结构化 JSON | PR open | NewAPI 自身限流的 429 格式 |
| new-api#7228、#6141 | 首 token / 首字节超时 | PR open；open | 持头期间的超时 |
| one-api#1774 | 并发太高时无法重试 | open | 旧版同类问题 |

---

## 9. 扩展方向

按收益和改动量排序。9.1 和 9.2 建议一起做；其余先不做，写明各自的触发条件。

### 9.1 S7：NewAPI 的终态限流改成 429，不重试（已拍板 D5，T16）

```text
现在：
NewAPI ─200 + ": PING"… + 终态错误─► nahook ── newAPIFinalError 成立 ──► 原样透传 ─► 客户端收到 200
改后：
NewAPI ─200 + ": PING"… + 终态错误─► nahook ── newAPIFinalError 成立，且 IsRateLimit
                                              ──► 不重试（NewAPI 已经重试过 RetryTimes 次）
                                              ──► proxy429.Write ─► 客户端收到 429
```

- 改动：
  - `startup.Read` 要能认出 rc.45 在心跳之后追加的裸 JSON。现在把它当成"读到 EOF 放行"，对应的测试是 `TestReadRawJSONAfterHeartbeatIsReleased`，届时要改。
  - intercept.go:49-56 的分支从 `release` 改成 `proxy429.Write`。
  - 不是限流的终态错误（例如 500）仍然透传。
- 场景：
  - 测试环境开着 1 秒心跳，上游一直回 HTTP 429。
  - 现在：客户端收到 200，然后是几个 `: PING`，最后是一段不带 `data:` 的 JSON，没有正常的结束事件。客户端多半会当作断流来处理，而不是限流【推论】。
  - 改后：客户端收到 HTTP 429，体里是 NewAPI 的原始错误，会按限流处理。上游的调用次数不变，仍然只有 NewAPI 的那几次。
- 建议：做。这是"限流一律变 429"最大的缺口，改动只在 `startup` 和 `proxy`。

### 9.2 代 429 带 `Retry-After`（已拍板 D5，T16）

```text
冷却门 g.until（N7）──► 剩余时间 = until − now（向上取整到秒，至少 1）──► proxy429.Write 设置 Retry-After（N9）
```

- 场景：
  - 客户端连续收到 429，但不知道要等多久，只能用自己的退避。
  - 如果退避比上游 org 的冷却短，就会再次撞上限流。
  - OpenAI 官方 Python SDK 会读 `retry-after-ms` 和 `retry-after`（openai-python `_base_client.py:856`）。Codex CLI 的仓库里有 `Retry-After` 解析（`codex-rs/http-client/src/retry_after.rs`），但没有核实它是否用于 Responses 的 429。
- 代价：要改 design §1.4 里"不加 `Retry-After`，和 NewAPI 保持一致"这一条。
- Caddy 的替代写法（4.7 的 `>Retry-After N`）只能给固定值，不推荐。
- 建议：和 9.1 一起做。S2、S6 的 429 是 NewAPI 的输出，是否也补 `Retry-After`，可以放到同一个任务里讨论。

### 9.3 非流式纳入拦截

- S2 已经是 429，nahook 能做的只有补 `Retry-After`。
- S3（上游 200 + 错误体）才是真正的缺口，但目前不知道有多频繁。
- 建议：先在 NewAPI 的错误日志里统计 `status_code = 200` 的错误条数，再决定要不要做。

### 9.4 Claude `/v1/messages` 和 Gemini

- 需要做的事：
  - `classify` 识别这两种请求，Gemini 是看路径 `:streamGenerateContent` 加 `alt=sse`；
  - `startup` 识别 `event: error` 和 `rate_limit_error`；
  - `proxy429` 输出 Claude 格式的 429 体。
- 建议：等这两种接口有实际流量和需求时再立项。

### 9.5 出站旁路（换站）

- 只有一个模型有多个渠道、需要在限流时换站时才有意义（第 6 节）。
- 建议：现在不做。

### 9.6 跟踪 NewAPI 上游

- new-api#6446 一旦合入，S5 会由 NewAPI 自己重试，nahook 和它叠加后上游调用次数会相乘。
- 建议：升级 NewAPI 之前先看这个 PR 的状态。如果已合入，要么把 nahook 的上限调成 0，要么用 N6 的规则认出它的终态错误。

### 9.7 Caddy 的 `lb_try_duration`

- 见 4.5 和第 10 节 V3。
- 建议：先实测。如果 2.11.4 上 dial 失败重试 POST 安全，nahook 重启那几秒就不会再有 502。

---

## 10. 实测

都在测试环境做，都需要先请示。V1、V2、V4 已在 2026-10-22 做完（用户批准，记录见 spec/specaudit.md §7.21）。

| 编号 | 内容 | 做法 | 结果 |
|---|---|---|---|
| V1 | S6 的 Content-Type | mockrelay 立即回 HTTP 429，心跳开着（1s），RetryTimes 3；直连和经站点各发一次 | **已测**：流式是 HTTP 429 + `Content-Type: text/event-stream`，体是裸 JSON，message 不带 request id，没有 `Retry-After`；mockrelay 被调用 4 次，没有心跳；非流式是 `application/json` |
| V2 | `header` 补 `Retry-After` | 临时 Caddy，两个端口分别用 `>` 和 `?`；`/200`、`/429`、带原值 30 的 `/429ra` 各请求一次 | **已测**：`>` 只加在 429 上，但把 30 改成了 5；`?` 连 200 也加上了，原值 30 保留 |
| V3 | `lb_try_duration` | 在 nahook 路由加 `lb_try_duration 5s`，停掉 nahook，发 POST | 待做：要换 Caddyfile、停 nahook，需要另行请示 |
| V4 | `{http.request.body}` 分流 | 临时 Caddy 加上 4.3 的表达式；流式、非流式、带空格、300KB、嵌套 `"stream":true` 各发一次，看 nahook 计数器 | **已测**：只有流式的走 nahook，请求体完整；嵌套的 `"stream":true` 被误送进 nahook，nahook 透传，结果正确（4.3） |

S7 在 rc.45 和关护栏的 h3 上的实际字节已经测过（T10 T-F17，spec/specaudit.md Q20），9.1 直接用它们做样本。

测试文件在测试主机的 `/root/nahook-t10/hook-validate/`；临时 Caddy 都只监听 127.0.0.1，到时自动退出，没有动正式的 Caddyfile。
