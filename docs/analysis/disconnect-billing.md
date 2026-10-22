# 客户端断开与计费差异：机制、场景、识别与按用户批量统计

基线：NewAPI v1.0.0-rc.45（测试环境；生产是 rc.44。下面引用的代码在 rc.45、rc.46 和最新 main 上相同，rc.44 没有逐行核对）；Caddy v2.11.4；nahook `build-fe009dc7`（T16）。

出处写法和 [hook-points.md](hook-points.md) 相同：NewAPI `文件:行号@rc.45`；nahook `文件:行号`；【推论】是从源码推出、还没实测的。第 10 节列了要在测试环境做的实测。

---

## 0. 结论

1. **根因在 NewAPI：它发往上游的请求不跟客户端连接绑定。** 文本类请求用的是 `http.NewRequest`，不带客户端的 context（`relay/channel/api_request.go:321`@rc.45；本地的最新官方代码 `:329` 也还是这样）。
   - 客户端断开以后，上游请求照常跑完。
   - 非流式：NewAPI 读完整个响应体，**照常计费**；写回客户端失败，最多在 stdout 打一行 `failed to copy response body`（`service/http.go:74-77`），响应体小于写缓冲时连这一行都没有。**数据库日志里没有任何"客户端已断开"的标记。**
   - 流式：要等上游回了响应头，读流的循环才发现客户端走了（`relay/helper/stream_scanner.go:304-307`），记 `end_reason=client_gone`，按已收到的内容计费，提示词照算（`service/responses_usage.go:88-116`）。
2. 所以只要 **下游放弃的时刻 < 我方拿到结果的时刻 ≤ 我方的超时上限（等上游响应头 1800s）**，就是"我方计费、下游没收到、下游也不向它的用户计费"。下游如果也是 NewAPI，并且会对超时或 5xx 重试，还会叠成**重复计费**：同一个提示词我方扣了几次，下游一次都没收到。
3. **光看 NewAPI 数据库识别不出来**，因为缺少"客户端什么时候断开"这一半。要靠一条断开记录（nahook 或 Caddy 访问日志）去关联 NewAPI 的计费记录。关联键是开始时间加模型：NewAPI 请求 ID 的头 23 位就是请求开始的 UTC 时间，精确到纳秒（`common/utils.go:243-257`）。
   - Caddy 2.11.4 把"等响应头时客户端断开"在访问日志里记成 `status:0`（2.11.5 起改记 499），默认级别下错误日志里什么都不记（8.4）。LT 也没开访问日志。
   - 只看"context 被取消"分不清客户端是"没收到就走"还是"收完才走"：非流式正常完成以后，context 同样会被取消（new-api#7842）。要由能看到响应有没有完整送出的一层来判断，这正是 nahook 的位置（5.1）。
4. 按用户统计时，配置故障有明显的指纹：
   - 断开耗时集中在某个固定值，说明有一层网关超时卡在这里；
   - 多条请求在同一时刻一起断，说明对方在重启或网络抖动；
   - 断开耗时很短而且固定，说明超时设得太小；
   - 断开之后紧跟一模一样的请求，说明对方在自动重试。

   第 6 节把这些写成了可以自动跑的规则。
5. **社区现状：** 官方把非流式的这类差异定性为"预期行为"或"链路超时配置问题"（new-api#4186、#2974）。和我们同型的 #7231 被以 not_planned 关闭。让上游请求跟着客户端断开的 PR #4500 一直没合。业界分两派：
   - "断开就停"：NewAPI 流式、OpenRouter 流式、LiteLLM 的 `cancel_on_disconnect`。代价是上游如果不跟着停，亏的是中转自己（new-api#4344）。
   - "跑完照收"：NewAPI 非流式、OpenRouter 非流式、TrueFoundry 默认。代价就是本文的差异。

   详见第 8 节。
6. **不改 NewAPI 源码，就拦不住上游继续跑。** nahook 或 Caddy 断开的只是到 NewAPI 的连接，NewAPI 照样等上游、照样计费。在不改源码的前提下，能做的只有三件：识别出来、对账、和下游对齐超时。

---

## 1. 机制：一次慢的非流式请求在三方之间怎么走

以下面这个例子为准：下游网关 300s，我方等上游最多 1800s，上游 400s 才出结果。

```text
时间   下游中转 D（自己也跑 NewAPI）     我方 Caddy → nahook → NewAPI                     上游
 0s    D 的用户发请求 ─► D ─────────────► 收到；NewAPI 生成请求 ID（头 23 位 = 开始时间）
                                         NewAPI 发往上游（不绑客户端连接）─────────────► 开始生成
300s   D 的网关超时，关掉到我方的连接 ─► Caddy 取消 ─► nahook 取消 ─► NewAPI 的 context 被取消
       D 判这次请求失败，不向它的用户扣费   但 NewAPI 发往上游的请求不受影响，继续等
       （D 如果会对超时重试：再发一次 R2 ─► 我方又开始一份，见下）
400s                                     NewAPI 拿到上游结果 ◄──────────────────────── 上游完成，向我方计费
                                         读完响应体 → **向 D 的令牌扣费**（日志 use_time=400）
                                         写回 D：连接早已关闭，写失败（最多打一行 stdout）
结果：上游 → 我方：计费 1 次；我方 → D：计费 1 次（R2 也慢的话是 2 次）；D → 它的用户：0 次
      D 看自己的账：请求失败、没收钱，但在我方的余额少了
```

上游超过 1800s 才回头时，NewAPI 等响应头超时，记 `do_request_failed`（500，不计费）。500 在默认重试范围内，所以会换渠道再试，每次重新计时。这时我方不向 D 计费，但上游可能已经向我方计费了，这是反方向的差异【推论，取决于上游】。

---

## 2. 链路上的超时

| 位置 | 机制 | 默认 | 到点会怎样 | 出处 |
|---|---|---|---|---|
| 我方 NewAPI → 上游：建连 / TLS | Transport | 30s / 10s | 500 `do_request_failed`，可重试 | `service/http_client.go:79-98`@rc.45 |
| 我方 NewAPI → 上游：等响应头 | `RELAY_RESPONSE_HEADER_TIMEOUT` | **1800s** | 同上。对非流式来说，这就是生成时长的上限 | `common/init.go:116`、`service/http_client.go:108-116` |
| 我方 NewAPI → 上游：整个请求 | `RELAY_TIMEOUT` | 0（不限） | — | `common/init.go:114`、`service/http_client.go:129-131` |
| 我方 NewAPI 流式：两行之间空闲 | `STREAMING_TIMEOUT` | 300s | 不算错误，截断后正常收尾 | `common/init.go:185` |
| 我方 Caddy → nahook / NewAPI：等响应头 | `response_header_timeout` | 30m（我们的 Caddyfile） | 给客户端回 504 | README §1.5 |
| 我方 Caddy ⇄ 客户端：TCP keepalive | `keepalive_idle/interval/count` | 15s / 15s / 9 次（约 150s 判死） | 静默断开时取消请求 | caddyserver/website `options.md` |
| 下游是 NewAPI 时，它发往我方的请求 | 同上三项 | 1800s / 0 / 300s；但很多站点会把 `RELAY_TIMEOUT` 设成一个固定值 | `RELAY_TIMEOUT` 到点：500，**会重试** | 同上 |
| 下游前面的网关、SDK | 见 8.3 的默认超时表 | Nginx 60s、Cloudflare 125s、openai-python 600s…… | 关闭连接；SDK 还会自动重试 | 8.3 |

**差异窗口 = （下游放弃的时刻，我方的上限 1800s］。** 下游放弃得越早，窗口越大。

---

## 3. 断开场景全表

"差异"指我方和下游对同一个请求计费不一致。

| # | 场景 | 谁先断 | 我方 NewAPI | 下游 D | 有没有差异 |
|---|---|---|---|---|---|
| C1 | 非流式，D 的网关或 SDK 超时（例如 300s），上游 < 1800s 完成 | D | 完成后照常计费 | 不计费 | **有**：我方多收 |
| C2 | C1，加上 D 自动重试（例如 D 的 NewAPI `RELAY_TIMEOUT=300` → 500 → 换渠道或再请求我方） | D | 每次都计费 | 0 次，或者只算成功的那一次 | **有，而且翻倍** |
| C3 | 非流式，上游超过 1800s | 我方 | 500，不计费，可能重试 | D 早就走了 | 我方和 D 一致；上游可能向我方计费【推论】 |
| C4 | 流式，D 在拿到响应头之前就断开（例如 nahook 正在冷却等待） | D | nahook 立即停止重试；NewAPI 那次尝试如果已经发往上游，上游照跑，回头后读流循环发现断开，`client_gone`，按已收到的计费 | 不计费 | 小：有过事件就计提示词 |
| C5 | 流式，输出中途 D 断开（用户取消、空闲超时） | D | `client_gone`，按已收到的计费 | 看 D 自己的策略 | 看双方策略 |
| C6 | 流式，D 的 `STREAMING_TIMEOUT`（300s）因为上游长时间不出字而触发 | D | 同 C5 | 截断后正常收尾，D 按收到的计费 | 基本一致；我方心跳能防止它触发【推论】 |
| C7 | 我方 nahook、Caddy 重启或 reload | 我方 | 正在等的请求被切断；NewAPI 照样等上游并计费 | 收到 502 或断开，不计费 | **有**：我方多收，而且是很多用户同时 |
| C8 | 网络静默断开（keepalive 约 150s 才判死） | 网络 | 和 C1 一样，只是发现得晚 | 自己超时以后多半会重试 | **有** |
| C9 | 客户端超时设得太小（例如 httpx 默认 5s） | D | 几乎每个请求都是 C1 | 几乎从不成功 | **有，而且量大** |

---

## 4. 现在的数据里能看到什么

| 来源 | 有什么 | 缺什么 |
|---|---|---|
| NewAPI 日志表 `logs`（`model/log.go:57-78`@rc.45） | `request_id`（头 23 位 = 开始时间，纳秒，UTC）、`created_at`（写日志的时刻 ≈ 结束）、`use_time`（秒，向下取整）、`is_stream`、`type`（2 消费、5 错误）、`quota`、`prompt_tokens`、`completion_tokens`、`user_id`、`token_id`、`token_name`、`group`、`model_name`、`channel`、`upstream_request_id`、`other`（流式有 `stream_status.end_reason`） | **非流式没有断开标记**。`ip` 只有用户开了"记录 IP"才有（`model/log.go:286-310`）。关掉"消费日志"开关时，消费记录整行都没有（`:418`） |
| NewAPI stdout | 写回失败时一行 `failed to copy response body` | 不可靠：响应体小于写缓冲时不会出现；也没有用户字段 |
| Caddy 错误日志（journald） | 5xx 一类的代理错误，带请求头（`Authorization` 记成 REDACTED） | 客户端在等响应头时断开，默认级别下没有任何记录（只有 DEBUG 级的 `upstream roundtrip`）（8.4） |
| Caddy 访问日志 | **LT 没开**。开了以后，"等响应头时断开"记成 `status:0, size:0`，`duration` 约等于断开时刻（2.11.4；2.11.5 起记 499）；流式中途断开记已写出的状态码（例如 200）和字节数 | 没有模型；`Authorization` 默认打码，要按用户分组得配哈希过滤器；`X-Api-Key`、`x-goog-api-key` 默认会**明文**进日志（8.4） |
| nahook | 计数器 `client_gone`、`passthrough` | **没有逐请求的日志**：哪个请求、断在第几秒、什么模型，都不知道 |

---

## 5. 逐请求识别：断开记录关联计费记录

### 5.1 断开记录要有的字段

由 nahook 在请求结束时打一条（需要开发，见 9.1）；或者由 Caddy 访问日志提供（字段少一些，见 9.1 的对比）：

```text
start_ns   nahook 收到请求的时刻（UTC 纳秒）
end_ns     结束时刻
outcome    delivered（完整送达） | client_gone（客户端先走） | 429 | 502 | ...
phase      client_gone 时在哪一段：wait_headers（等 NewAPI 回头，非流式就是在等整段生成）
           | startup（读启动帧）| gate_wait（冷却等待）| streaming（放行之后）
path、stream、model、client_ip（X-Real-IP）、user_agent
rid        NewAPI 的 X-Oneapi-Request-Id（回过头才有；非流式在 wait_headers 断开时没有）
```

`outcome` 和 `phase` 是这条记录里最重要的两项。NewAPI 那一侧在非流式**正常完成以后** context 也会被取消（new-api#7842），只看 context 取消分不清"没收到就走"和"收完才走"。nahook 能分清，因为它知道两件事：

- NewAPI 的响应有没有到：`forward` 有没有返回；
- 响应体有没有完整写出：`copyFlush` 有没有写完。

这是让 nahook 来打这条记录的主要理由。

### 5.2 关联方法

nahook 和 NewAPI 在同一台机器上，nahook 读完请求体就转发，两边记的开始时间相差在毫秒级以内【推论，待实测分布】。

```text
对每条断开记录 G：
  候选 = NewAPI 日志里 model_name == G.model
         且 |开始时间(request_id 头 23 位) − G.start_ns| ≤ 50ms
  有 rid 时直接按 request_id 精确匹配，不用候选
  候选有多条：取开始时间最接近的一条，并标"有歧义"
关联上以后：
  type=2 且 quota>0，并且 created_at > G.end_ns   ⇒ B 类：**计费但没送达**（对账差异）
  type=5（错误）                                   ⇒ D 类：断开且没计费
  暂时没有日志                                     ⇒ NewAPI 还在等上游（挂起中），稍后再关联
```

没有 nahook 记录、只有 Caddy 访问日志时（例如不经过 nahook 的路径），也能关联，只是没那么准：

- 取 `status=0` 的记录，开始时间 = `ts − duration`；
- 和 NewAPI 日志按开始时间（±50ms）、客户端 IP（要求用户开了"记录 IP"）配对；
- 再用 Caddy 的 `bytes_read`（请求体长度）和 `prompt_tokens` 粗略对一下。

消费记录的 `model_name` 取 `GetBillingModelName()`：配了计费别名时是别名，否则就是客户端请求的模型名（`service/text_quota.go:229`、`relay/common/relay_info.go:860-868`@rc.45）。所以关联时，配了计费别名的模型要先换算一次。

### 5.3 不逐条关联也能估：按用户的超时上限推算

知道某个用户的超时上限 m（第 6 节 R1 能自动识别）以后，NewAPI 日志里这个用户 `is_stream=0`、`type=2`、`use_time > m` 的记录**基本都是 B 类**。用这个就能估差异金额：

```sql
-- 示例：MySQL；m 由 R1 得出；只读
SELECT user_id, token_id, COUNT(*) AS n, SUM(quota) AS quota
FROM logs
WHERE type = 2 AND is_stream = 0 AND use_time > :m
  AND created_at BETWEEN :from AND :to AND user_id = :uid
GROUP BY user_id, token_id;
```

---

## 6. 按用户批量统计与自动识别

### 6.1 用户维度

- **主键**：NewAPI 的 `user_id`、`token_id`（关联以后才有）。关联之前先用 `client_ip + user_agent` 粗分。
- **下游中转站**：靠运营打标最可靠（分组或用户备注）。自动线索【推论】：
  - 请求来自固定的几个服务器 IP；
  - User-Agent 是程序默认值（例如 `Go-http-client/1.1`），不是浏览器或 CLI；
  - 并发高、模型多、全天匀速；
  - 断开耗时集中在一个固定值（R1）；
  - 断开后紧跟同样的请求（R4）。
- 每个用户按天累加：请求数、断开数、断开率、B 类条数、B 类金额，以及 R1–R6 命中的次数和参数。

### 6.2 识别规则

阈值都是初始值，要在测试环境和生产数据上校准（第 10 节）。

| 编号 | 指纹 | 规则 | 结论 |
|---|---|---|---|
| R1 超时上限 | 断开耗时堆在同一个值 | 某用户一天内的断开耗时按 1 秒分桶，取众数 m；落在 [m−2s, m+2s] 的 ≥ 5 条，并且占这个用户断开数的 ≥ 50% | "这个用户有一层超时在 m 秒"。按 8.3 的默认值猜是哪一层：30 GCP LB、60 Nginx / AWS ALB、125 Cloudflare（旧值 100、120）、300 Vercel 默认或常见自设值、600 openai / anthropic SDK、1800 NewAPI 默认的等响应头上限 |
| R2 同时断开 | 不同时刻开始的请求，在同一时刻一起断 | 某用户 2 秒内断开 ≥ 5 条，并且这几条的开始时间相差 ≥ 10 秒（排除 R1 那种"一起开始、一起超时"） | 下游重启、发布或网络抖动。**同一时刻涉及 ≥ 3 个用户** ⇒ 更像是我方的问题（C7）：查当时 nahook、Caddy、NewAPI 有没有重启或 reload |
| R3 过短 | 断开耗时又短又固定 | R1 识别出的 m < 15s，或者某用户 50% 以上的断开发生在 10 秒以内 | 客户端超时设得太小（例如 httpx 默认 5s）。非流式时几乎每个请求都是 B 类 |
| R4 重试 | 断开之后紧跟同样的请求 | 断开后 m+30s 以内，同一用户、同一模型又来一个请求体大小相同的请求；关联以后，B 类后面跟着 `prompt_tokens` 相同的另一条消费记录 | 自动重试；同一份提示词被重复计费。常见来源：openai-python、anthropic SDK 对超时和 504/524 默认重试 2 次；下游 NewAPI 对 500 会换渠道重试（8.3、8.1）。没有 nahook 记录时，用 Caddy 的 `bytes_read` 判断"请求体相同" |
| R5 我方上限 | 耗时贴着我方的上限 | NewAPI 错误日志里 `use_time ≈ 1800`，或者 Caddy 504 的耗时 ≈ 30 分钟 | 上游卡住了，或者我方的上限需要调整 |
| R6 静默断开 | 断开原因是 keepalive 判死 | Caddy 的错误串里有连接超时，而不是对方关闭（需要开 Caddy 日志） | 网络问题；断开耗时会比真实中断晚约 150s，所以不参与 R1 |

**次数累加和告警**（示例，阈值待定）：

- 单个用户一天内 B 类金额超过阈值，或者第一次识别出 R1 上限，就发通知，附上证据：上限值、条数、金额、几条样本的请求 ID。
- R2 一天内 ≥ 3 次，就提醒对方检查重启和网络。
- R3、R4 一旦命中就提醒，因为这两种会持续造成损失。

---

## 7. 下游是中转站时的几个特点

1. **下游自己的 NewAPI 会重试**：它对我方的超时（500 `do_request_failed`）和 5xx 默认会重试，这就是 C2 的来源。每次重试在我方都是独立的一次计费。
2. **下游常把 `RELAY_TIMEOUT` 设成固定值**（例如 300）。到点它判失败、不向它的用户计费。这就是你例子里的"客户中转不记账"。
3. **下游前面还有它自己的网关**（Nginx 默认 60s、Cloudflare 100s）。这一层影响的是它和它的用户之间，不直接影响我方。但它的用户断开后，下游 NewAPI 也不会取消发往我方的请求（同一份代码），所以我方照样计费、下游照样向它的用户计费。这种情况**双方一致**，亏的是下游。
4. **流式的心跳**：我方 NewAPI 开着心跳（每秒一个 `: PING`），能防止下游的 `STREAMING_TIMEOUT`（300s 空闲）触发【推论，NewAPI 读流时把注释行也算作活动】。但 nahook 在冷却等待期间不发任何字节（specaudit Q19）。下游这时在等响应头，受它的 `RELAY_RESPONSE_HEADER_TIMEOUT`（1800s）管，正常情况下不会触发。

---

## 8. 社区 issues 与各方做法

2026-10-22 查的。NewAPI 的相关代码在 rc.46 和 main 上没有变化。

### 8.1 NewAPI / one-api

| 编号 | 内容 | 状态 | 和我们的关系 |
|---|---|---|---|
| new-api#7231 | 非流式请求客户端超时以后仍继续上游请求，断开后照常扣费。日志特征：`failed to copy response body: write: broken pipe`，`use_time_seconds=2308`，GIN 访问日志记 `200 / 38m28s` | closed，not_planned | **和我们完全同型**；日志特征可以直接拿来用 |
| new-api#4186、#4184 | 客户端断开以后上游请求没取消 | closed，not_planned。维护者答复"预期行为"，认为强行断开会丢上游的计费、让网关亏钱 | 官方对非流式的立场 |
| new-api#4500（PR） | 客户端断开时取消上游请求，并且不再重试 | **open**，没有维护者评审；基线已经变了，要重做才能合 | 合了才能从根上消除差异 |
| new-api#4185、#3469（PR） | 同一方向 | 未合并、open | — |
| new-api#4344 | 反方向：自己改成断开就取消以后，本地不计费，上游（另一家中转）照收全额 | closed | "断开就停"的代价 |
| new-api#6589（PR） | Bedrock 渠道断开即取消 | merged，rc.23 | 只有 Bedrock |
| new-api#5710（PR） | 流式断开后关上游、记 `client_gone` | merged，rc.17 | 只管流式 |
| new-api#7427（PR） | 流在用量到达前被截断时估算用量，提示词照算 | merged，rc.38 | C4、C5 的计费依据 |
| new-api#7842、#7844 | 非流式正常完成以后 context 也会被取消，被性能统计判成"忽略" | open | 只看 context 取消分不清两种情况（5.1） |
| new-api#2974、#4364、#5145 | 下游看到 504/524，但已经扣费 | 维护者定性为反代/CDN 超时配置问题 | 同类问题的官方回答 |
| new-api#6949（PR） | 加 `RELAY_RESPONSE_HEADER_TIMEOUT`，默认 1800s（之前没有上限，挂过 31.5 小时） | merged，rc.28 | **我方的 1800s 就是这个默认值** |
| new-api#6532 | 中间件总是重新生成 request_id，忽略客户端传来的 `X-Oneapi-Request-Id` | closed，not_planned | 跨系统不能按我方 ID 关联，只能按时间 |
| new-api#5071、#4569、#7621 | 请求到达时间打点、按 ID 查用量、日志里记调用方 IP 和客户端标识 | open | 都是我们对账需要、官方还没有的 |
| new-api#7657 | 容器重启时没有结算完在途请求 | open | C7 的另一面 |
| one-api#184、#518 | 超时但扣费；中转别人的 one-api 时扣费异常 | closed | one-api 同样用不绑 context 的 `http.NewRequest` |

### 8.2 其他网关和上游厂商

| 对象 | 非流式断开 | 流式断开 | 出处 |
|---|---|---|---|
| NewAPI rc.45 | 不取消（Bedrock 除外），全额计费 | 关上游，按已收到或估算的计费，记 `client_gone` | 第 0 节 |
| LiteLLM | 默认不取消；`cancel_on_disconnect: true` 时取消，并按 499 记录 | `/chat/completions` 会关上游；`/v1/responses`、`/v1/messages` 还不会 | BerriAI/litellm PR #30223、#30245，issue #36123 |
| OpenRouter | 继续处理，**按完整响应计费** | 支持取消的厂商立即停止并停止计费；不支持的照常全额计费 | openrouter.ai/docs/api_reference/streaming |
| TrueFoundry | 默认跑完、按实际计费；可以配置成取消，取消后不记成本 | 分厂商：有的读完再计费，有的取消后按估算计费 | truefoundry.com/docs/ai-gateway/stream-disconnect-cost-accounting |
| OpenAI（上游） | 文档只说"断开连接就取消同步响应"，**没写怎么计费**；社区有"524 超时仍被计费"的报告 | 社区：关流以后停止生成，少量在途 token 照算 | developers.openai.com 的 background 指南；community.openai.com |
| Anthropic（上游） | 没有找到计费条文；建议超过 10 分钟的请求用流式或 Batches；SDK 会拒绝预计超过 10 分钟的非流式请求 | 未查到 | platform.claude.com/docs/en/api/errors |

### 8.3 常见网关和 SDK 的默认超时（R1 用）

| 组件 | 默认 | 到点 | 出处 |
|---|---|---|---|
| Nginx `proxy_read_timeout` | 60s（两次读之间） | 504；客户端先走记 499 | nginx.org 文档 |
| Cloudflare | **125s**（旧值 100s、120s） | 524；企业版可调到 6000s | developers.cloudflare.com 的 524 说明 |
| AWS ALB idle timeout | 60s | 504；客户端先走记 460 | AWS ALB 文档 |
| GCP 外部 HTTP(S) LB | 30s | 502/503；客户端先走记 status 0 | GCP 文档 |
| Azure Front Door | 30s（16–240s 可调） | 504 | Microsoft 排障文档 |
| Vercel Functions | 300s | 504 | Vercel 文档 |
| Netlify Functions | 60s | — | Netlify 文档 |
| openai-python / openai-node | 600s；**超时和 408/409/429/≥500 都会重试 2 次** | 下游网关回 504/524 后会再发请求 | openai-python `_constants.py`、`_base_client.py` |
| anthropic-sdk-python | 600s，重试 2 次 | — | `_constants.py` |
| httpx | **5s** | — | `httpx/_config.py` |
| requests、Go http.Client | 不设就不超时 | — | 各自文档 |
| LangChain ChatOpenAI | 默认 None，等于不超时【推论】 | — | langchain-openai 源码 |
| Codex CLI | 流式空闲 300s；请求重试 4 次，流重试 5 次 | — | `codex-rs/model-provider-info/src/lib.rs` |
| NewAPI rc.45 | 等响应头 1800s；整个请求不限；流式空闲 300s | 等响应头超时 → 500，会重试 | 第 2 节 |

### 8.4 Caddy 日志（v2.11.4，源码核实）

- **断开会传到 NewAPI**：反代用的是客户端请求的 context，客户端一断，Caddy 就中止到后端的请求。
- **等响应头时断开**：
  - 访问日志记 `status:0, size:0`。`server.go:856` 的原注释是 "will be 0 if no response is written by us"。
  - `duration` 约等于断开时刻。
  - 默认级别下错误日志里没有记录。
  - 改记 499 的 caddy#7827 合在 2.11.4 之后，首个包含它的 tag 是 v2.11.5。但 2.11.6 起有新的默认读超时回归（caddy#8103，2.11.7 修复），升级前要先评估。
- **流式中途断开**：WARN `aborting with incomplete response`；访问日志记已经写出的状态码和字节数，`duration` 可能为 0【推论】。
- **凭据**：
  - 默认只把 `Cookie`、`Set-Cookie`、`Authorization`、`Proxy-Authorization` 记成 REDACTED。
  - **`X-Api-Key`、`x-goog-api-key` 不在名单里**，开访问日志时必须单独过滤。
  - 要按令牌分组，可以用日志的 `filter` 编码器加 `hash` 过滤器（SHA-256 前 4 字节）。哈希 `Authorization` 之前要先开 `log_credentials`，否则哈希的是常量 "REDACTED"。

### 8.5 业界的识别做法

- **监控"客户端先断开"的比例，并且和延迟放在一起看。** 499 的比例随延迟一起上涨，往往是 5xx 的前兆；一个慢请求最终记成 499 还是 504，取决于哪一层的超时更短（"超时链"分析）。各组件里对应的标记不同：Nginx 499、ALB 460、GCP status 0、Envoy `DC`、Caddy ≤2.11.4 记 0、LiteLLM 499。
- **现实中的超时大多是整数**（30、60、100、125、300、600 秒），断开耗时会堆在这些值附近。所以延迟直方图的桶边界要覆盖这些值，否则看不出来。
- **Google SRE 对重试的建议：** 每个请求最多尝试 3 次；客户端级重试比例低于 10% 才重试；请求里带上"第几次尝试"，后端按次数统计；只在紧邻的上一层重试。**截止时间和取消要一路往下传**，避免为已经放弃的请求白干活。这正好是 NewAPI 非流式缺的那一环（sre.google 的 handling-overload、addressing-cascading-failures 两章）。

---

## 9. 方案

### 9.1 识别和对账的数据链路（待拍板 D9）

```text
                ┌──────────── 我方主机 ─────────────────────────────────────────────┐
客户端/下游 ─► Caddy ──(可选) 访问日志 JSON ───────────────┐                          │
                │                                         │                          │
                └─► nahook ── 每个请求结束时打一条 request_end（5.1 的字段）─► journald │
                       │                                   │                          │
                       └─► NewAPI ── logs 表（计费）        │                          │
                                       │                   ▼                          │
                                       └──► 对账脚本（定时，只读）：关联（5.2）→ 分类 B/D
                                                           → 按用户累加 → R1–R6 → 日报 + 告警
                └───────────────────────────────────────────────────────────────────┘
```

| 断开记录从哪来 | 覆盖面 | 字段 | 改动 |
|---|---|---|---|
| nahook `request_end` 日志 | 只有经过 nahook 的路径：`/v1/responses`、`/v1/chat/completions`，流式和非流式都有 | 全（含 model、phase） | nahook 加一个日志事件，小 |
| Caddy 访问日志 | 所有路径（包括 `/v1/messages`、图片等） | 时间、耗时、状态（2.11.4 断开记 0）、IP、UA、请求体长度；没有 model。要按令牌分组，就得开 `log_credentials` 再用 `hash` 过滤 `Authorization`；还必须过滤 `X-Api-Key`、`x-goog-api-key`，否则会明文落盘（8.4） | 改 Caddyfile（加 `log`），需要你批准；要配日志轮转 |

**建议**：先做 nahook 的 `request_end` 日志，和 D6 一起排进 T17；再写对账脚本，只读 NewAPI 的 `logs` 表，数据库连接由你在主机上配，我不经手。两样都先在测试环境验证。等确实需要覆盖 Claude、Gemini 等其他接口时，再开 Caddy 访问日志。

### 9.2 B 类怎么处理（业务决定，待拍板 D10）

| 做法 | 效果 | 代价 |
|---|---|---|
| 只出报表 | 先知道有多少、是谁、多少钱 | 不解决差异 |
| 通知下游，附证据（"你的网关在 300s 断开，导致 N 次计费没送达，共 X 元"） | 让下游调大超时，或者改用流式；从根上减少 C1、C2、C9 | 要有一个联系渠道 |
| 按报表人工补偿 | 账面一致 | 上游已经向我方收过钱，补偿是我方的成本 |
| 调小我方的 `RELAY_RESPONSE_HEADER_TIMEOUT` | 差异窗口变小 | 全局生效，正常的长请求也会被砍 |
| 让 NewAPI 的上游请求跟着客户端断开（社区 PR new-api#4500，或者自己维护补丁） | 断开即停，不向下游计费 | 官方不收（8.1），自己维护补丁违背"不改 NewAPI 源码"的前提；上游如果不跟着停，照样向我方收全额，亏的变成我方（new-api#4344） |

**建议**：先出报表、通知下游，补偿个案人工处理；不动全局超时。

---

## 10. 要在测试环境实测的

| 编号 | 内容 | 需要 |
|---|---|---|
| M1 | 非流式请求、上游 N 秒后才回 JSON、客户端在 M（< N）秒断开：NewAPI 是否仍完成并计费，日志里 `use_time`、`created_at` 是什么 | mockrelay 增加"延迟 N 秒返回 JSON"的脚本项（现在只会回 SSE），属于开发 |
| M2 | nahook 和 NewAPI 两边记的开始时间差的分布（决定 5.2 的 50ms 窗口） | M1 + nahook 的 `request_end` 日志 |
| M3 | 验证 8.4 的源码结论：2.11.4 对"等响应头时断开"记 `status:0`；流式中途断开时 `duration` 记多少 | 临时 Caddy 实例 |
| M4 | 用一批请求模拟 R1（固定在 300s 断开）、R2（同时断开）、R4（断开后重试），检验规则的命中率 | 对账脚本 |
