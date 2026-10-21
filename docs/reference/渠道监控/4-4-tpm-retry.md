# LT `4→4`：同渠道 TPM 重试、RetryTimes、等待锁

本文补上只在对话里出现过的拆解。LT（`luna-task-master`，`ghcr.io/alterem/new-api-h3@38e69e300`）渠道只有 `#4 gpt-openai-api21`。

## 1. `4→4` 是什么

`4` 是渠道数据库 ID。`retry: 4->4` 和 `use_channel: ["4","4"]` 是 **同一条 HTTP 请求里打过哪些渠道**。

成功路径在 `Relay()` 里提前 `return`，**不会**打 `retry: 4->4` 那行 INFO，只在消费日志 `use_channel` 里能看到 `["4","4"]`。失败才会在循环结束后打印 `retry: 4->4`。

| 写法 | 含义 |
|---|---|
| `["4"]` | 第一枪成功 |
| `["4","4"]` | 第一枪 429，第二枪还是 #4 |
| `4->12` | 换到另一条渠道（本机没有第二条） |

## 2. 这条 429 在说什么

```text
Rate limit reached for gpt-6-astra
in organization org-xxxxxxxxxxxxxxxxxxxxxxxx   （快照已脱敏）
on tokens per min (TPM): Limit 40000000, Used 40000000, Requested 19224
Please try again in 28ms
```

TPM 桶：该 org 每分钟 4000 万 token，已经用完，这一枪还要约 19224（含缓存命中）。28ms ≈ `19224 / (40000000/60000)`。不是本站额度，不是 `encrypted_content`。

## 3. 失败详情（`…eRqlLRZV`）

请求策略决策：

1. 选 #4（11ms）
2. 上游 HTTP 429（2756ms）——护栏已把 SSE 收成 429
3. `retry_status_matched`，健康不变
4. 再选 #4（2758ms，只隔 **2ms**）
5. 再 429（3652ms）
6. `attempt_budget_exhausted`

`RetryTimes=1` → 一共 2 枪。官方循环不睡 `Retry-After`。

## 4. 同一秒为何部分成功、部分失败

14:02–14:03 并发打 #4：

| 结果 | 约数量 |
|---|---|
| 第一枪成功 | 57 |
| `4→4` 第二枪成功 | 5 |
| 两枪都 429 | 4 |

护栏有效。失败是同一只 4000 万桶仍满，第二枪立刻再撞。

## 5. RetryTimes 在哪设

**系统设置 → 请求策略 → 重试预算 → 最多重试次数**。键 `RetryTimes`，写入 `options` 表，运行时 `common.RetryTimes`。出厂 0。LT 现为 1。

不是护栏环境变量。`RESPONSES_STREAM_STARTUP_GUARD_ENABLED` 只决定能不能把 SSE 变成 429。

填 n 实际打 n+1 枪。改成 3 = 最多 4 枪，失败路径大约 8～12s 转圈，不自动等 28ms，更容易把桶钉死。

## 6. 护栏挂点

```text
Relay() 重试循环
  → doRequest：护栏开则不写 SSE 头
  → OaiResponsesStreamHandler
       guardResponsesStartupStream     ★
       classifyResponsesStartupError   ★ rate_limit → 429, skipRetry=false
  → DecideRelayRetry
  → ★ 等待应加在下一圈 getChannel 之前
```

源码：

- `relay/helper/stream_scanner.go` `ResponsesStartupGuardActive`
- `relay/channel/openai/responses_startup_error.go` `guardResponsesStartupStream`
- `relay/channel/openai/relay_responses.go` `OaiResponsesStreamHandler`
- `controller/relay.go` 重试 for

## 7. 等待锁该加在哪

面板没有重试间隔。

**等待（sleep）**：`Relay()` 里 `decision.Action == "retry"` 之后。仅当下一枪仍是同一 `channel.Id` 且本枪是 TPM 429：解析 `try again in 28ms` / `Retry-After`，可取消 sleep，下限约 50ms、上限约 2s。换渠道不睡。

**锁**：按渠道 ID 串行化 `4→4`，避免 16 路同时睡 28ms 再一起撞桶。不要锁第一枪选渠。

不要加在 `classifyResponsesStartupError` 里裸 Sleep（取消打不断；HTTP 429 JSON 走不到护栏）。
