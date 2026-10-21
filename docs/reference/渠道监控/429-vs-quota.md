# 中转站「Rate limit reached … in organization」：429 还是额度耗尽

对象是 **上游 NewAPI 类中转站** 回给本网关的错误，不是直连官方 OpenAI 控制台。本网关把中转站当一条渠道（类型常为 OpenAI 兼容 / New API）。

## 实验室里这条报错是什么

实测（Astra 流式）原文：

```text
HTTP 429
error.type = tokens
error.code = rate_limit_exceeded
message = Rate limit reached for gpt-6-astra in organization org-… on tokens per min (TPM):
          Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms.
```

`gpt-6-lunna` 同一句式。这是中转站 **原样转发** 的 TPM 墙：有 Limit/Used/Requested、有 `Please try again in Xms`、code 是 `rate_limit_exceeded`。这是限流，不是该站账号额度花光。

`org-…` 是中转站上游的 organization，不是本站用户。

## 事件驱动能不能分开判

能。挂钩已经在每次渠道失败时发生：`ProcessChannelError` + `DecideRelayRetry`。现在这两处 **只看 HTTP 状态码**（429 → 重试；禁用码默认只有 401），**不读** `error.code` / `error.type` / message。所以事件在，判断没做。

中转站常见几档，按字段就能分开：

| 来源 | HTTP | type | code | message 特征 | 事件上该做什么 |
|---|---|---|---|---|---|
| 本站用户额度 | 403 | `new_api_error` | `insufficient_user_quota` | `用户额度不足` | 已 `skipRetry`，到不了换渠道 |
| 本站模型 RPM 中间件 | 429 | `new_api_error` | 空 | `您已达到请求数限制` | `Abort`，不到 Relay |
| 中转站 TPM/RPM | 429 | `tokens` / `rate_limit_error` | `rate_limit_exceeded` | `Rate limit reached for {model} in organization` + `TPM`/`RPM` + `Please try again in` | **重试其他渠道，不禁用** |
| 中转站自己重试用尽 | 429 | `new_api_error` | 空或 `bad_response_status_code` | `当前分组上游负载已饱和` | 再换下一条本站渠道 |
| 中转站/上游额度耗尽 | 常 429 或 403 | `insufficient_quota` 或 `new_api_error` | `insufficient_quota` / `quota_exceeded` | `exceeded your current quota` / `额度不足`，**没有** try again in Xms | **换渠道，并可事件禁用该渠道/Key** |

启动护栏当前把 `insufficient_quota` 和 `rate_limit_exceeded` 都标成 429 可重试。换站这一点对额度耗尽也合理（换另一家中转）；缺的是 **额度耗尽应顺手禁用这条渠道**，TPM 则不要禁用。

## 建议的事件判定（只看中转站回包）

在 `ProcessChannelError` 里对这一次失败分类，不另起轮询：

1. `type == new_api_error` 且文案是本站额度/本站 RPM → 停，不换站。
2. `code ∈ {rate_limit_exceeded, RateLimitReached, 429}` 且 message 含 `tokens per min` / `requests per min` / `Please try again in` → **TPM/RPM 429**，重试，不禁用。
3. `code ∈ {insufficient_quota, quota_exceeded}` 或 message 含 `额度不足`/`exceeded your current quota` 且没有 `try again in` → **额度耗尽**，重试其他站，并走现有 `DisableChannel` 事件。
4. `当前分组上游负载已饱和` → 该中转站内部已经换过渠道仍 429，本站再换下一条渠道。
5. 其余 429 → 按现状重试，不禁用。

`gpt-6-lunna in organization … TPM … try again in 28ms` 落在第 2 档。不需要外挂监测程序才能判断；外挂只用来在护栏关闭、200 SSE 直出时把同一事件捞回来。

## 现网缺口

- `DecideRelayRetry`：429 一律 `retry_status_matched`。
- `ShouldDisableChannel`：默认不匹配 429。
- `classifyResponsesStartupError`：`insufficient_quota` 与 TPM 合并。
- 中转站若做了状态码映射（429→200）或只在 SSE 里带 error，HTTP 层全是 200，必须靠启动护栏把帧收成内部错误后，同一套字段判定才用得上。
