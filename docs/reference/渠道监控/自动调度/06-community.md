# 社区与大厂做法

检索时间：2026-10-20，用 brave-search，结果按条读过原文。这一轮**没有找到**"用外部 hook 让 NewAPI 按普通用户切渠道"的现成实现；同类功能都做在网关内部（LiteLLM、sub2api）。

## 1. 调度与切换

| 来源 | 做法 | 我们采纳什么 |
|---|---|---|
| NewAPI 官方文档、CSDN、[linux.do 讨论](https://linux.do/t/topic/515184) | 靠优先级、权重、自动重试、渠道测试、自动禁用；社区问"怎么自动换渠道"，回答都是调权重 | 证实 NewAPI 没有按用户、按事件切渠道的原生能力 |
| [all-api-hub](https://github.com/qixing-jk/all-api-hub) | 独立的外部程序，通过 API 管渠道、做可用性测试 | 和"外部程序 + 管理接口"思路最接近，但它不改单个请求的路由 |
| [LiteLLM 健康检查驱动路由](https://docs.litellm.ai/docs/proxy/health_check_routing) | 后台定期探测（默认 300s），结果超过 600s 视为过期；按错误类型设置"容忍几次失败再冷却"；可以忽略 429/408 这类瞬时错误；所有部署都不健康时放开过滤、继续放行 | 探测结果设 TTL；全挂时留在原渠道；探测不把 TPM 型 429 当作渠道不健康 |
| [LiteLLM 负载均衡](https://docs.litellm.ai/docs/proxy/load_balancing) | 遇到 429 立即让该部署冷却；按 order 逐级回退；usage-based-routing-v2 按 TPM/RPM 余量选部署，用 Redis 原子计数，多实例共享 | 按渠道统计用量和健康；多实例时状态要共享 |
| LiteLLM [#45312](https://github.com/BerriAI/litellm/issues/45312) | **坑**：一个客户端的 429 让共享部署冷却了整个 retry-after，所有人都受影响 | 冷却和摘除的 key 要划得窄，且有上限 |
| LiteLLM [#45645](https://github.com/BerriAI/litellm/issues/45645) | **坑**：Responses 流中途的 429 被记到了回退的那个部署上 | 事件只记到产生它的那次尝试 |
| sub2api [README](https://github.com/Wei-Shaw/sub2api/blob/main/README_CN.md) | 401/402/403/429/529/5xx 切账号；400/413/422 不切，也不改账号状态 | 只有限流类错误触发切换 |
| sub2api [#6992](https://github.com/Wei-Shaw/sub2api/issues/6992) | **坑**：粘性会话把并发全集中到同一个账号 | 用 P2C 选候选，不总选第一个 |
| vm2api [#163](https://github.com/dofastted/vm2api/issues/163) | **坑**：粘性 TTL 长达 86400s，账号限额后旧会话仍被钉死 | 目标被摘除，路由立刻失效 |
| [Envoy AI Gateway](https://aigateway.envoyproxy.io/docs/api/) | 网关从请求体取出模型名，写成 `x-ai-eg-model` 头，按这个头路由；支持按优先级回退 | 和"改写模型名来选路"同一个思路 |
| [Envoy 异常摘除](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/outlier) | 摘除时长 = 基础时长 × 连续被摘次数；`max_ejection_percent` 限制最多摘除的比例；到期自动恢复 | 摘除按次数退避，最多摘一半 |
| [Linkerd](https://linkerd.io/2016/03/16/beyond-round-robin-load-balancing-for-latency/) / [Envoy Peak EWMA](https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/contrib/load_balancing_policies/peak_ewma/peak_ewma) | P2C（随机抽两个、选负载更低的）配合延迟的指数移动平均 | 在健康渠道里用 P2C 选 |

## 2. 重试与过载（v1 冷却门也用到）

| 来源 | 要点 |
|---|---|
| [AWS Builders' Library](https://builder.aws.com/content/3EumjoZascWd1oZiEgL8ORlv3qE/timeouts-retries-and-backoff-with-jitter) | 抖动是为了避免同步重试；只在一层重试（5 层各重试 3 次会放大到 243 倍）；本地用令牌桶限制重试 |
| [Google SRE：Handling Overload](https://sre.google/sre-book/handling-overload/) | 每个请求最多 3 次尝试；重试占比低于 10%；只在被拒绝那层的直接上一层重试；自适应节流 K=2 |
| [OpenRouter 流式文档](https://openrouter.ai/docs/api_reference/streaming) | 响应提交前用 HTTP 状态码报错，提交后改发 SSE 错误事件；用 `: OPENROUTER PROCESSING` 注释保活 |
| CLIProxyAPI [#6554](https://github.com/router-for-me/CLIProxyAPI/issues/6554) | bootstrap 重试期间异步发心跳；第一次心跳之前出错回 HTTP 状态码，之后改发终止性 SSE 错误 |

## 3. Caddy

| 来源 | 要点 |
|---|---|
| [reverse_proxy 文档](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy) | `flush_interval` 为负值时，客户端提前断开也**不会取消**发往后端的请求；`text/event-stream` 响应无论怎么配都会立即刷新；后端连接失败时，POST 也一定会换下一个上游重试 |
| [caddy#6666](https://github.com/caddyserver/caddy/issues/6666) | "不取消后端请求"这个行为来自 2022 年合入的 #4952，2024 年 11 月被回退。LT 用的 2.6.2 大概率包含它，测试项 T-C1/T-C2 会验证 |
