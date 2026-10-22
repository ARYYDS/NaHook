# LT 上补丁怎么跑，以及不改官方源码的 hook

## 1. LT（「LT 站点域名」）实际部署

主机 `「LT 主机名」`，容器 `luna-task-master`。

| 项 | 值 |
|---|---|
| 镜像 | `ghcr.io/alterem/new-api-h3:latest` |
| 镜像 revision | `38e69e300`（Responses 启动护栏那次提交） |
| 开关 | `RESPONSES_STREAM_STARTUP_GUARD_ENABLED=true` |
| 编排 | `/root/container/luna-task/compose.yaml` |
| 换镜像时间 | 2026-10-20 约 11:50 +08（提交后数分钟） |
| 换之前 | `calciumion/new-api:latest`（官方镜像，bak 在同目录） |

这不是官方进程里挂插件，而是 **整包换成带补丁的二进制**。

今日日志里护栏已经在干活（`gpt-6-astra`、渠道 `#4`、`/v1/responses` 流式）：

```text
attempt 1  channel 4  status 429  error_code=rate_limit_exceeded
           decision retry_status_matched  health=unchanged
attempt 2  channel 4  success  stream_status=completed
use_channel ["4","4"]
retry: 4->4
```

含义：上游 TPM 429 被收成内部 429，客户端没看到 SSE 错误帧，同一请求第二次打同一渠道成功（窗口只有约 28ms）。只有一条渠道时换站变成「同渠道立刻再打」。

## 2. 官方 NewAPI 有没有能挂钩的面

| 面 | 能否拦截 Responses 流并重试 |
|---|---|
| 任务 JS 插件 `openai_responses` | 会 **接管** 该模型的协议，不是包在 Go 中继外面。`utils.fetch` 限 4 次、15s、**1MiB、无流式**。不能读 SSE 启动帧再交给官方 relay。 |
| 渠道 `proxy`（http/socks） | 正向代理，到 HTTPS 中转站是 TLS，看不到 body。 |
| 渠道状态码映射 | 只改已解析的 HTTP 状态，200 SSE 错误进不去。 |
| 高级自定义渠道 | 改路径/转换器，不读 SSE。 |
| 官方 `DecideRelayRetry` | 只认 HTTP 状态码。HTTP 429 才会换渠道。 |

结论：官方进程 **内部没有**「中继完成后、写给客户端前」的 hook。要在不改官方源码的前提下复现 LT 补丁，只能把 200 SSE 特征码 **先变成 HTTP 429**，再交给官方重试循环。

## 3. 推荐 hook：挂在渠道 Base URL 上（出站）

```text
客户端
  → 官方 NewAPI（不改源码，RetryTimes≥1，多渠道拉开 Priority）
       → 渠道 Base URL = http://127.0.0.1:19080
            → hook 进程
                 → 真正的中转站 https://…
```

hook 只处理 `/v1/responses` 流式：

1. 把请求转到真实上游（目标用 Header Override，例如 `X-Real-Upstream`）。
2. 若上游已是 HTTP 429 JSON，原样返回。
3. 若上游是 HTTP 200 + `text/event-stream`：缓冲启动事件（与护栏相同：约 128 事件 / 256KiB）。
4. 命中特征码则 **对 NewAPI 返回 HTTP 429 JSON**（不要把 SSE 交给 NewAPI）。
5. 未命中则把已读字节 + 后续流拼接回 NewAPI。

官方 `DoRequest` 尚未写客户端 SSE 头（因为收到的是 429 不是 200 流），`RelayErrorHandler` 得到 429，`DecideRelayRetry` → 下一档渠道。

特征码（中转站回包）：

```text
error.code = rate_limit_exceeded | RateLimitReached | 429
error.type = tokens | rate_limit_error
message 含：Rate limit reached for … in organization
            tokens per min / requests per min
            Please try again in
```

额度耗尽（`insufficient_quota` / 额度不足，无 `try again in`）同样返回 429 给官方以便换站；若要禁用该渠道，hook 做不到，仍要官方自动禁用规则或另写管理 API。

渠道配置示例（官方后台，不改代码）：

- Base URL：`http://127.0.0.1:19080`
- Header Override：`X-Real-Upstream: https://中转站域名`
- 密钥仍填中转站 Key
- `RetryTimes` ≥ 1，备份渠道更低 Priority

hook 与 NewAPI 同机、只监听 localhost。渠道 `proxy` 字段不要填 hook（那是正向代理）。

## 4. 次选：挂在客户端和 NewAPI 之间（入站）

sidecar 看到的是 NewAPI **已经选完渠道** 的响应。检测到 200 SSE `rate_limit_exceeded` 后，只能 **整单再 POST 一次给同一 NewAPI**。官方每次请求独立选渠道，同 Priority 可能再抽到刚限流的那条。入站 hook **用不上** 官方的 `retry` 下标和 `use_channel` 轨迹。LT 这种「同请求 attempt 1 失败、attempt 2 成功」做不到。

## 5. 和 LT 现状对比

| | LT 现在 | 官方 + 出站 hook |
|---|---|---|
| 改官方源码 | 换成 `new-api-h3` 镜像 | 不换二进制 |
| 拦截点 | 进程内 `guardResponsesStartupStream` | Base URL 旁路进程 |
| 重试 | 官方 `Relay` 循环 | 同一套循环（看到的是 hook 给出的 HTTP 429） |
| 同渠道秒重试 | 可以（日志 `4->4`） | 可以 |
| 换其他中转站 | 要第二条更低 Priority 渠道 | 相同 |
| JS 插件 | 不需要 | 不需要 |

官方镜像继续用 `calciumion/new-api:latest` 时，出站 hook 是不改源码能接上重试循环的路径。LT 已经用补丁镜像，hook 是可选的替代，不是当前线上机制。
