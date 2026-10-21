# 渠道监控架构与 429 实现

当前代码相对官方 New API 最新部分，只多了 Responses 流式启动护栏。渠道探测、优先级重试、Uptime Kuma 面板仍是官方同一套。补丁解决的是：**上游已经限流，却以 200 SSE 进客户端，重试循环进不去。**

## 1. 三层结构

```text
                    ┌─────────────────────────────────────────┐
                    │  控制台 / 监测接口                       │
                    │  渠道测试  系统任务  Uptime Kuma  性能   │
                    └───────────────┬─────────────────────────┘
                                    │
        ┌───────────────────────────┼───────────────────────────┐
        ▼                           ▼                           ▼
  定时/手动探测              运行时转发（每次用户请求）           对外状态页
  testChannel()              Relay() 重试循环                    /api/uptime/status
  自动禁用 / 自动恢复        DecideRelayRetry + 换渠道            只展示，不切流量
```

| 层 | 作用 | 429 时做什么 |
|---|---|---|
| 探测 | 周期或手动打一条真实上游请求，更新响应时间、自动禁用/恢复 | 记失败；是否禁用看 `AutomaticDisableStatusCodes`（默认只有 401，**429 不禁用**） |
| 运行时 | 用户请求失败后按优先级再选渠道 | HTTP 429 可重试；**流内 429 要靠补丁才能重试** |
| 面板 | 把 Uptime Kuma 公共状态页拉进控制台 | 不参与选路 |

探测层发现渠道挂了，可以自动禁用，让运行时选路不再打到它。运行时 429 默认**不**禁用渠道，只换下一条再打。限流结束后高优先级渠道仍可被选中。

## 2. 官方最新：429 怎么到客户端

官方重试开关在 `DecideRelayRetry`（`service/relay_error.go`）。`RetryTimes > 0` 且状态码落在 `AutomaticRetryStatusCodes`（默认含 `409-499`，因此含 429）时，动作为 `retry`。

```text
用户请求
  → Distributor 按分组+模型选最高优先级渠道
  → Relay 循环 attempt = 0 .. RetryTimes
       转发上游
       成功 → 结束
       失败 → DecideRelayRetry
            retry → CacheGetRandomSatisfiedChannel(retry)  // retry 是优先级下标
            stop  → 把最后一次错误回客户端
```

**HTTP 429 + JSON**（流还没开始）：`RelayErrorHandler` 带上 `StatusCode=429`，重试循环可以换渠道。这是官方已有行为。

**HTTP 200 + SSE 错误事件**（Responses / 部分中转）：官方在 `DoRequest` 里先写 `text/event-stream` 响应头，再读上游。上游用 200 打开流，前几帧是：

```text
event: response.created
data: {"type":"response.created",...}

data: {"type":"error","error":{"code":"rate_limit_exceeded","message":"TPM ..."}}

data: {"type":"response.failed","response":{"status":"failed",...}}
```

此时客户端已经在收 SSE。`Relay()` 的 defer 看到 `Writer.Written()` 且 Content-Type 是 event-stream，走 `writeRelayStreamError`，把错误再塞进同一条流。重试循环即使判定 `retry`，对客户端来说也已经是「一次成功打开的流里的报错」，无法整单换渠道重放。

实测（`docs` 外的 429 实验室）：非流 JSON 高缓存时常全 200，打不出限流；`POST /v1/responses` + `stream:true` 更容易打出 **流内 error**，形态就是上面这种 200 SSE。

## 3. 补丁：启动护栏把 200 SSE 收成可重试 429

提交：`38e69e300` `fix(relay): guard Responses stream startup and clean stream end`

开关（默认关）：

```text
RESPONSES_STREAM_STARTUP_GUARD_ENABLED=true
```

生效条件（`ResponsesStartupGuardActive`）：

- 环境变量打开
- 本请求是流式
- `RelayMode == Responses`
- 渠道类型是 OpenAI 或 Azure

实现步骤：

1. `DoRequest`：护栏激活时 **不** 调用 `SetEventStreamHeaders`，下游还是未提交的 HTTP 响应。
2. `OaiResponsesStreamHandler` 先跑 `guardResponsesStartupStream`：最多读 128 个启动事件 / 256 KiB，看到 `response.failed` / `type=error` / `status=failed` 就停。
3. `classifyResponsesStartupError` 把 `rate_limit_exceeded`、`RateLimitReached`、`TooManyRequests`、数字 `429` 等标成 **HTTP 429**，`skipRetry=false`。
4. 处理函数带着这个错误返回，**客户端一个字节都没收到**。
5. `Relay()` 循环调用 `DecideRelayRetry` → `retry_status_matched` → `getChannel` 按下一档优先级选另一条中转站 → `SetupContextForSelectedChannel` 换 Base URL / Key / 模型映射 → 再打。

```text
官方（护栏关）                         补丁（护栏开）
──────────────                         ──────────────
DoRequest 先写 SSE 头                  DoRequest 不写头
上游 200 + SSE error                   缓冲启动帧
客户端已看到流                         分类成内部 429
writeRelayStreamError                  返回错误，Writer 未 Written
无法整单换渠道                         DecideRelayRetry → 换渠道
```

其它分类（同一函数）：

| 上游码/文案 | 内部状态 | 是否重试 |
|---|---|---|
| rate_limit / 429 / TPM | 429 | 重试 |
| model_at_capacity / overloaded | 503 | 重试 |
| invalid_api_key | 401 | 重试 |
| permission_denied | 403 | 重试 |
| context_length / invalid_request | 400 | 停止 |
| server_error | 500 | 重试 |
| 客户端取消 | 408 | 停止 |
| 启动超时 | 504 | 停止 |

400 和客户端取消带 `skipRetry`。401 会重试（官方重试码含 401–407）；是否自动禁用渠道是另一条规则，默认只禁 401。

护栏范围：只覆盖 **OpenAI/Azure 的 `/v1/responses` 流式**。Chat Completions 流式仍可能先写 SSE 头；Claude 流式用另一块保活（`25372e30b`），在准备和重试期间对客户端发 `: PING`，429 HTTP 仍可换渠道并在同一连接上续流。

## 4. 换到「其他中转站的其他模型」

选择键是 **客户端模型名 + 分组**，不是上游真实模型名。

1. 同一分组里多条渠道都声明同一个客户端模型（例如 `gpt-4o`）。
2. **Priority 必须拉开**。`retry` 是优先级下标：第 1 次用最高档，429 后用第二档。同档多条是权重随机，可能再次抽到刚 429 的渠道。
3. 备份渠道用「模型映射」把客户端名改成该中转站的上游模型。
4. `RetryTimes` 默认 0（只打一次）。控制台 **请求策略 → 最大重试次数** 必须大于 0。含义是「不含首次」：`RetryTimes=2` 一共 3 次尝试。

运行时 **不会** 自动改客户端请求的模型名。换模型只发生在备份渠道的 mapping 里。

停在当前站、不换渠道的情况：Token 绑定渠道、亲和 strict、`RetryTimes=0`、400/`skipRetry`、重试预算用尽。

`AppendUsedChannel` 只写日志轨迹 `12->34->56`，选择器不排除刚失败的 ID。

## 5. 探测层（渠道监控本体）

`monitor_setting`：

| 字段 | 默认 | 作用 |
|---|---|---|
| `auto_test_channel_enabled` | false | 定时测渠道 |
| `auto_test_channel_minutes` | 10 | 周期 |
| `channel_test_mode` | `scheduled_all` | 测哪些渠道 |
| `channel_test_concurrency` | 1 | 并发，1–32 |

模式：

- `scheduled_all`：除手动禁用外全测
- `auto_ban_only`：只测开了自动禁用的渠道
- `passive_recovery`：只测自动禁用状态的渠道，且测试失败时不再禁用（只尝试恢复）

环境变量 `CHANNEL_TEST_FREQUENCY`（分钟）会打开定时并强制 `scheduled_all`；`CHANNEL_TEST_ENABLED` 覆盖开关。

执行路径：`RegisterScheduledSystemTasks` → `channelTestHandler` → `runChannelTestTask` → `performChannelTests`。多实例用系统任务租约去重。手动「测试所有渠道」入队同一类型任务，已有进行中的会 409。

单渠道测试 `GET /api/channel/test/:id` 走 `testChannel()`，真正打上游；成功则 `UpdateResponseTime`。失败是否禁用看 `ShouldDisableChannel`：总开关 `AutomaticDisableChannelEnabled`、渠道 `AutoBan`、状态码规则（默认 401）、关键词。

429 探测失败通常 **不会** 把渠道禁用掉，所以运行时仍可能打到限流站；要避开它，靠优先级阶梯 + 重试，而不是自动禁用。

## 6. 配置清单（429 切站要同时满足）

1. `RetryTimes >= 1`
2. `AutomaticRetryStatusCodes` 含 429（默认 `409-499` 已含）
3. 同分组、同客户端模型、备份渠道更低 Priority
4. 备份渠道模型映射指向该中转站真实模型
5. Token 不锁死单渠道；亲和不要 strict
6. Responses 流式：`RESPONSES_STREAM_STARTUP_GUARD_ENABLED=true`
7. 不要把渠道状态码映射把 429 改成 400（判定前会改写 `StatusCode`）
