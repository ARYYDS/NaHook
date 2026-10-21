# new-api-h3 与官方 NewAPI 的差异，以及不改官方源码的接法

共同祖先：`32e587406` `feat(email): 添加 resend 邮件服务商接口 (#7880)`。

| 树 | HEAD | 相对祖先 |
|---|---|---|
| 官方 `QuantumNous/new-api` | `603a5ceb5` | +1：仪表盘展示缓存 token（#7773），与 429 无关 |
| `alterem/new-api-h3` | `38e69e300` | +1：Responses 流式启动护栏 |

h3 **没有**另一套选路/计费。重试仍是官方 `Relay` + `DecideRelayRetry`。h3 只补了「200 SSE 限流进不了这条循环」这一环。

## 1. 源码逻辑差（仅 `38e69e300`）

官方流式 `/v1/responses`：

```text
DoRequest 先 SetEventStreamHeaders（客户端已是 200 SSE）
  → 上游 HTTP 200 + 流内 rate_limit_exceeded
  → 错误只能 writeRelayStreamError 塞进同一条流
  → DecideRelayRetry 即使想 retry，头已经写出去
```

h3 打开 `RESPONSES_STREAM_STARTUP_GUARD_ENABLED=true` 且渠道为 OpenAI/Azure、模式为 Responses 流式时：

```text
DoRequest 不写 SSE 头
  → guardResponsesStartupStream 缓冲启动事件（≤128 事件 / 256KiB）
  → 命中 rate_limit_exceeded / TPM 文案 → 内部 HTTP 429，skipRetry=false
  → 客户端 0 字节
  → 官方 Relay 循环 retry_status_matched → 再选渠道
```

改动的文件：

| 文件 | 官方 | h3 |
|---|---|---|
| `constant/env.go` `common/init.go` | 无此开关 | `RESPONSES_STREAM_STARTUP_GUARD_ENABLED`，默认 false |
| `relay/channel/api_request.go` | 流式一律先写 SSE 头 | 护栏激活时不写 |
| `relay/channel/openai/responses_startup_error.go` | 无 | 新建：读启动帧、分类 429/503/401/403/400/500 |
| `relay/channel/openai/relay_responses.go` | 直接扫流 | 先 `guardResponsesStartupStream`；终态事件 `sr.Done()` |
| `relay/helper/stream_scanner.go` | 等到上游关连接；cleanup 后仍可能带着写超时 | 终态结束；handler 返回前清 `WriteDeadline` |
| `.github/workflows/docker-ghcr.yml` | 无此 workflow | 发布 `ghcr.io/alterem/new-api-h3` |

分类（`classifyResponsesStartupError`）把 `rate_limit_exceeded`、`RateLimitReached`、数字 429、`insufficient_quota` 都标成可重试 429。400 和客户端取消才 `skipRetry`。

LT（`luna-task-master`）跑的就是这颗二进制：`ghcr.io/alterem/new-api-h3:latest@38e69e300`，环境变量打开。日志 `retry: 4->4`、`use_channel:["4","4"]` 是官方重试循环吃到内部 429 之后的行为，不是另一套调度。

**h3 本身改了官方源码。** 换镜像 ≠ 在官方进程里挂钩子。

## 2. 不改官方源码时，功能怎么接

官方没有「中继完成后、写客户端前」的插件点。JS 任务插件会接管 `openai_responses`，`utils.fetch` 无流式。渠道正向 `proxy` 对 HTTPS 看不到 body。

要把官方循环用起来，只能让官方 **看到 HTTP 429**。做法：渠道 Base URL 指到本机 hook，hook 扮演官方眼里的「上游」：

```text
官方 calciumion/new-api（源码不动）
  RetryTimes ≥ 1，多渠道 Priority 拉开
    → 渠道 Base URL http://127.0.0.1:19080
         Header Override: X-Real-Upstream: https://中转站
         → hook
              缓冲 /v1/responses 启动 SSE
              命中特征码 → 对官方返回 HTTP 429 JSON
              否则把流原样交回
    → 官方 DecideRelayRetry 换渠道 / 再打
```

特征码与 h3 护栏同一套：`error.code=rate_limit_exceeded`，message 含 `Rate limit reached for … in organization`、`tokens per min`、`Please try again in`。

这样官方 `Relay`、优先级、`use_channel` 日志全部沿用。hook 只做 h3 里 `guardResponsesStartupStream` 那一层，放在进程外。

入站 sidecar（客户端 ↔ NewAPI）只能整单再 POST，用不上同一次请求里的 attempt 下标。

## 3. 对照

| | 官方 | h3 源码 | 官方 + 出站 hook |
|---|---|---|---|
| 二进制 | `calciumion/new-api` | 改过的 `new-api-h3` | 官方 |
| 200 SSE 限流 | 直出客户端 | 收成内部 429 | hook 收成 HTTP 429 再交给官方 |
| 重试 / 换渠道 | 仅 HTTP 429 | 同一套循环 | 同一套循环 |
| LT 现状 | 已不用 | **正在用** | 未部署 |

要「逻辑与 LT 相同、仓库仍是官方」，走出站 hook。要「与 LT 字节级相同」，继续用 h3 镜像。
