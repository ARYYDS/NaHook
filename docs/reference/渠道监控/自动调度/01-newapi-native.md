# NewAPI 原生调度机制与边界

行号按官方 `QuantumNous/new-api` `206e0fa5c`，路径相对 `C:\code\NewAPI\NEWAPI\`。

## 1. 重试只作用于单个请求

| 事实 | 位置 |
|---|---|
| 每个请求新建 `retryParam`，`Retry` 从 0 开始 | `controller/relay.go:172` |
| 重试循环 `retry = 0..RetryTimes`，一共 1+R 次 | `controller/relay.go:177` |
| 每次失败调 `DecideRelayRetry(c, err, RetryTimes - retry)` | `controller/relay.go:226` → `service/relay_error.go:20` |
| 流内限流（200 SSE）被逐帧转发，handler 返回 nil | `relay/channel/openai/relay_responses.go:92`、`:98` |
| handler 返回 nil 后只记 `stream_not_successful`，不进重试判定 | `controller/relay.go:218` → `service/request_policy.go:138` |
| 只有上游非 200 才进错误路径 | `relay/responses_handler.go:66` |
| 流式请求在发上游前就设置 SSE 头，开启保活时还会起 ping | `relay/channel/api_request.go:477-485` |

结论：

- hook 每重新 POST 一次，NewAPI 都当成一个全新请求，预算重新计算，NewAPI 没有字段能把它们串起来。
- **hook 的次数大于 `RetryTimes` 时不会被压回去。** NewAPI 的策略兜不了 hook 的底。
- 每个客户端请求打到上游的总次数：
  - 全部是流内限流：1 + H（H 是 hook 上限）。
  - 某次尝试里混了 HTTP 错误、NewAPI 内部又重试了：最坏 (1+H)×(1+R)。

## 2. 重试时按优先级往下换

`service/channel_select.go:96-114` 的注释和实现：

- retry=0 用最高优先级，retry=1 用下一档……这个分组的优先级用完，就换下一个分组。
- 同一档有多条渠道时按权重随机，可能再次抽到刚失败的那条。选路器不会排除刚失败的渠道 ID，`AppendUsedChannel` 只记日志轨迹。

所以原生行为是"一出错就换档"，不是"同站重试 N 次再换"。LT 只有一档一条渠道（#4），表现出来就是 `4→4`。

## 3. 渠道亲和

| 事实 | 位置 |
|---|---|
| 亲和只在 retry==0 时查 | `service/channel_select.go:312-313` |
| 请求成功（`Writer.Status() < 400`）后记录亲和 | `middleware/distributor.go:128` → `service/channel_affinity.go:726` |
| `SwitchOnSuccess` 记的是最终成功的那条渠道 | `service/channel_affinity.go:737` |
| 默认 TTL 3600s（规则 TTL 为 0 时用它） | `setting/operation_setting/channel_affinity_setting.go:122` |
| 开了 Redis 就存 Redis，命名空间 `new-api:channel_affinity:v1` | `service/channel_affinity.go:30`、`:82-110` |
| 出厂规则 `codex cli trace` | `channel_affinity_setting.go:125-137` |
| 该规则：模型 `^gpt-.*$`、路径 `/v1/responses`、key 取请求体 `prompt_cache_key` | `:126-129` |
| 该规则 `SkipRetryOnFailure: true`，换算成严格模式 | `:134` → `service/request_policy.go:160` |
| 严格模式：出错直接不重试（`strict_session`） | `service/relay_error.go:29` |
| 严格模式：亲和指向的渠道不可用时，直接回 503 `strict_session_binding_unavailable` | `service/channel_select.go:344` |

影响：

1. **h3 的 `4→4` 只对没带 `prompt_cache_key` 的请求有效。** 真实 Codex 会话会带这个字段，命中严格规则后就不重试。这是读代码得出的推断：LT 在 2026-10-20 的日志里命中会话规则 0 次，没有实测过。
2. 官方镜像下，流内限流返回的是 200，满足 `Status() < 400`，所以**失败的流也会把亲和续期到原渠道**。
3. 别名模型如果以 `gpt-` 开头，会命中这条规则 → 查到原渠道 → 原渠道不提供这个别名 → 503。别名命名规则见 [04](04-alias-billing.md)。

## 4. 普通用户不能指定渠道

- `sk-xxx-<渠道ID>` 这种写法只对管理员令牌有效：`middleware/auth.go:704`；普通用户会被拒（403）：`:719`。
- 全仓库添加渠道绑定（`AddPin`）的地方只有两处：管理员令牌，和任务插件（`middleware/task_plugin.go:1111`）。

## 5. 计费按模型，不按渠道

计费只看模型名和分组倍率，仓库里没有按渠道定价的实现。想"按新渠道计费"，就只能让新渠道对应一个独立的模型名，也就是别名，见 [04](04-alias-billing.md)。

## 6. 外部不能写入亲和

- 只有统计接口 `GET /api/option/channel_affinity_cache`，以及按规则或全部清空的 `DELETE /api/option/channel_affinity_cache`，两个都要求 root（`router/api-router.go:225-226`）。没有按 key 写入的接口。
- 也可以直接写 Redis 里的亲和键，但不推荐：
  - 依赖内部键格式，升级后可能失效；
  - 只对带 `prompt_cache_key` 的请求有效；
  - LT 的 Redis 没有对宿主机开放端口，hook 连不上。

## 7. 相关默认值

| 项 | 默认 | 位置 |
|---|---|---|
| 自动重试状态码 | 100-199、300-399、401-407、409-499、500-503、505-523、525-599 | `setting/operation_setting/status_code_ranges.go:21-29` |
| 永不重试 | 504、524 | `:31-34` |
| 自动禁用状态码 | 只有 401 | `:17` |
| 保活 ping | 关 | `setting/operation_setting/general_setting.go:28` |
| gzip | 只挂在 `/api` 上，`/v1` 转发路由没有 | `router/api-router.go:20` |
| `RetryTimes` | 0 | `common/constants.go:143` |
