# specaudit：SPEC 阶段的全局审计

SPEC 范围：`00-总体架构`、`01-nahook`。知识库 `渠道监控/自动调度/` 不是 SPEC，不在这里审。

## 1. 全局结论

| 根因 | 全局解法 | 在哪里收口 |
|---|---|---|
| R1：官方 NewAPI 把流式启动阶段的上游错误当成功透传，handler 返回 nil，重试循环根本不会触发（`NEWAPI/relay/channel/openai/relay_responses.go:92`、`:98`） | 不改 NewAPI。在 Caddy 和 NewAPI 之间串一个 nahook，在客户端收到任何字节之前识别限流，并负责同站重试 | 01 design §0、§1.6 |
| R2：重试层之间互不关联，NewAPI 每个请求都从 0 开始计数（`NEWAPI/controller/relay.go:172`） | 每种错误形态只由一层负责重试：SSE 内的限流由 nahook 重试；HTTP 层错误由 NewAPI 的 `RetryTimes` 重试；nahook 遇到非 200 一律透传 | 01 design §1.6、§8 |
| R3：外部边界有不确定的地方（Caddy 是否传递客户端断开、真实帧的形态、3000 端口是否暴露） | 先做风险实验（T0），再在测试环境（甲）里实测，全部通过之后才动生产 | 00 tasks §1；01 audit §5 |
| R4：生产变更和换镜像、数据库迁移绑在一起 | 分三个阶段上线，每个阶段都能单独回退；换官方镜像之前先备份 MySQL | 00 design §1.1 |
| R5：模块表和签名里引用了某个包或某条失败路径，但没有把它细分成任务、接口和取值 | 每个任务开工前核对：签名里出现的每个类型，要么已经合入，要么在本任务的改动清单里；interface §3 的失败路径必须能在 design 的伪代码里找到对应分支 | 01 tasks 各任务的"前置"；01 interface §3、§4 |

## 2. 问题总表

| 编号 | 根因 | 大类·小类 | 类别 | 严重度 | 涉及 SPEC | 状态 |
|---|---|---|---|---|---|---|
| Q1 | R3 | 入口·Caddy 断开传递 | 不明 | P1 | 00、01 | 已关闭（T0，2026-10-21）：nahook 路由不写 `flush_interval -1`；T-C1、T-C2 用 HTTP/2 复测 |
| Q2 | R3 | 判定·真实帧形态 | 不明 | P1 | 01 | 待 K2（阶段 2 确认）；message 兜底规则已覆盖大多数情况 |
| Q3 | R3 | 部署·生产主机的 3000 端口暴露在公网 | 脆弱 | P1 | 00 | 待 K5（生产主机） |
| Q4 | R4 | 部署·换镜像后数据库回退 | 脆弱 | P1 | 00 | 正向已在 LT 上验证（2026-10-21）；回退方向待 T10 在 LT 上演练（K4） |
| Q5 | R3 | 测试·LT 上测试分组的模型定价 | 未细分 | P2 | 00 | 已关闭：T9 请求没有因定价被拒；T-F6 经 nahook 和直连扣费一致（quota 125） |
| Q6 | R2 | 计费·被拦截的尝试是否为 $0 | 不明 | P2 | 01 | LT 已测（T-F9）：每次 $0、记 1 行日志；真实 failed 帧带 usage 的情况待 P-2 |
| Q7 | — | 工程·源码放在哪、怎么构建 | 未细分 | P2 | 01 | 已关闭：仓库 `ARYYDS/NaHook`，在 Win11 的 `O:\NaHook` 上开发和构建 |
| Q9 | — | 工程·产物怎么从 Win11 送到 LT | 未细分 | P2 | 01 | 已关闭：用 GitHub Release（仓库已公开） |
| Q8 | R2 | 冷却门·base 没有上限 | 脆弱 | P2 | 01 | 待实测（T-F4、P-2） |
| Q10 | R5 | 观测·`internal/obs` 没有任务、接口和计数器定义 | 未细分 | P1 | 01 | 已关闭：design §6.7、interface 2.7，并入 T5 |
| Q11 | R5 | 转发·失败路径（读请求体出错、ctx 取消）在 design 和 interface 里不一致 | 不明 | P2 | 01 | 已关闭：`classify` 增加 error 返回值；ctx 已取消时不写响应、计 `client_gone`（design §1.2、§1.6） |
| Q12 | R5 | 转发·`forward` 没写 Host、URL 和请求体长度的规则 | 未细分 | P2 | 01 | 已关闭：design §6.1 |
| Q13 | R5 | 排期·T6 漏了对 T5 的依赖；T5 要等 G1，但排在 T7 前面 | 耦合 | P2 | 01 | 已关闭：`obs` 放进 T6，`cmd/nahook` 放进 T5；顺序改为 T7 → T6 → T5，T6 不依赖 T0（§7.9） |
| Q14 | R5 | 测试·audit 用到的 mockrelay 行为（拉长输出、PING 之后再限流）在 §6.6 里没有定义；`/__log`、`http429`、`quota` 的格式没写 | 未细分 | P2 | 01 | 已关闭：design §6.6 重写，共 11 个脚本项 |
| Q15 | R2 | 重试·打开心跳后，NewAPI 的终态错误以 200 SSE 的形式出现，nahook 会再重试一轮 | 耦合 | P1 | 01 | 重新打开，见 Q20：request id 规则只认得 NewAPI 自己生成的错误；K7：LT 的心跳一直开着（间隔 1s），生产相同 |
| Q16 | R3 | 部署·生产主机是哪台、基线是否和 LT 改动前一致 | 不明 | P1 | 00、01 | 待 K8（用户告知主机并批准只读核查） |
| Q17 | R3 | 部署·nahook 挂掉时 Caddy 不立即回落 | 脆弱 | P1 | 00 | 设计已改（00 §3.2、§6.1、§8：`fail_duration 10s`，不写 `lb_try_duration`）；LT 待用户装上 `Caddyfile.final2` 后复测 T-C4 |
| Q19 | R1 | 流式·启动阶段（含重试和冷却等待）客户端收不到任何字节，包括 NewAPI 的心跳 | 脆弱 | P2 | 01 | 已写进 01 design §1.8；影响面待 P-1、P-2 观察（和 K7 一起看） |
| Q20 | R2 | 重试·心跳之后 NewAPI 转发的上游错误认不出来，nahook 叠加重试 | 耦合 | P1 | 01 | 设计已改（01 design §1.3、§1.6、§8；interface 2.1、2.5）；待 T15 实现，并在 LT 上用关护栏的 h3 复测 T-F17 |
| Q18 | R3 | 流式·站点层 `encode` 在 nahook 路由上更容易被触发 | 脆弱 | P2 | 00、01 | 已关闭：用户拍板去掉站点层 `encode`（LT 在 T10 去掉；生产本来没有，K8 时确认） |

## 3. 逐项说明

### Q1 Caddy 2.6.2 + `flush_interval -1` 时客户端断开

- 证据：
  - Caddy 文档说负值时"不取消发往后端的请求"；
  - caddy#6666 记录了这个行为来自 #4952，2024 年 11 月回退；
  - LT 的生产站点配置了 `flush_interval -1`（2026-10-20 读过 Caddyfile）。
- 影响：如果断开传不到 nahook，客户端走了以后 nahook 还会按上限继续重试。
- 处理：T0 先按 `docs/T0/测试方案.md` 做最小实验（4 种组合各跑 3 次），T-C1/T-C2 再在完整链路上复测。
- 结论（T0，2026-10-21，详见 §7.12）：
  - 等待响应头阶段：写了 `-1`，客户端断开后后端连接一直开着（3/3，测到 18s）；不写，后端在 1ms 内看到连接关闭（3/3）。
  - 原因在源码：caddy v2.6.2 `reverseproxy.go:802-803` 在 `FlushInterval == -1` 时把请求上下文换成 `ignoreClientGoneContext`，客户端断开不会取消后端请求。判断依据只是配置值，和 HTTP/1.1 还是 HTTP/2 无关。
  - 流式阶段：两种配置都会传递。写了 `-1` 时 Caddy 靠"写给客户端失败"才发现，约 5s；不写约 2s。
  - 落点：nahook 路由不写 `flush_interval`（00 design §6.1）；SSE 本来就会立即 flush（`streaming.go:133-137`）；nahook 不加单请求总时限（01 design §4）。

### Q2 真实中转站的流内限流帧

- 证据：`429-vs-quota.md` 只记录了 message 原文和 `type=tokens`、`code=rate_limit_exceeded`；h3 的日志只有 message。
- 影响：`upstreamError` 可能从帧里取不到结构化的错误。
- 处理：`IsRateLimit` 增加了"message 里含 Rate limit reached"的兜底规则；阶段 2 开始后，把前几次拦截的原始帧截断后记日志，确认帧形态。

### Q3 3000 端口暴露

- 证据：LT 上容器的端口映射是 `0.0.0.0:3000->3000`（2026-10-20 `docker ps`）。生产主机和 LT 同构，推论是一样（未核实）。
- 影响：直连 3000 的客户端会绕过 nahook。LT 只做测试，影响不大；生产主机上必须处理。
- 处理：K5，需要用户在云控制台确认生产主机的安全组。

### Q4 换镜像之后数据库回退

- 证据：
  - h3 `38e69e3` 只比官方 `32e587406` 多一个护栏提交，那个提交没有改 `model/` 和 `setting/`。
  - 目标版本 rc.45（`6cf0d90b`）是 `32e587406` 的祖先，中间隔 4 个提交，涉及表结构的只有 `058c823`：给 `audit_logs` 加了复合索引 `idx_audit_time_event`。
  - rc.45 的 `migrateDB` 里没有按版本号判断的迁移，AutoMigrate 只建缺的、不删多的；不认识的 option 键只进内存，不报错。
  - 2026-10-21 在 LT 上从 h3 换到 rc.45：启动迁移在 1 秒内完成，没有报错，渠道和系统设置都正常读到。
- 影响：正向（h3 → rc.45）没有问题。回退方向（rc.45 → h3）从代码看兼容：h3 的表结构是 rc.45 的超集，启动时只会补上那个索引。
- 处理：K4。T10 期间在 LT 上演练一次 rc.45 → h3 → rc.45；生产阶段 3 之前备份 MySQL。

### Q5 LT 上测试分组的模型定价

- 影响：测试令牌所在分组对 `gpt-6-astra` 没有定价的话，NewAPI 可能会拒绝请求；T-F6、T-F9 也要看计费。
- 处理：K6。T9 实测请求没有被拒；T10 核对了扣费：T-F6 经 nahook 和直连都是 quota 125，T-F9 被拦截的尝试 quota 0。已关闭。

### Q16 生产主机未核实

- 证据：2026-10-21 用户告知 LT 已和生产隔离、只做测试，生产上有一套"一模一样"的环境；主机是哪台、能否访问都还不知道。
- 影响：生产阶段 T11–T13 的全部步骤（路径、端口、镜像、Caddy 配置）都以"生产和 LT 改动前一致"为前提。
- 处理：K8。由用户告知主机并批准只读核查，按 LT 上做过的那套只读检查逐项比对（镜像 digest、compose、Caddyfile、端口、宿主机服务）。

### Q17 nahook 挂掉时 Caddy 不立即回落

- 证据：T10 T-C4，`systemctl stop nahook` 之后的第一个请求等了 5.03s，收到 502，Caddy 日志是 `dial tcp 127.0.0.1:19080: connect: connection refused`；12s 后的请求才走到 3000。原因在 caddy v2.6.2：没配 `fail_duration` 时 `countFailure` 直接返回（`modules/caddyhttp/reverseproxy/healthchecks.go`），`Upstream.Healthy()` 只在配了被动检查时才看失败次数（`hosts.go`），所以每次重试，`lb_policy first` 都重新选中 19080，直到 5s 的 `lb_try_duration` 用完。
- 影响：nahook 进程挂掉以后、下一次主动健康检查之前（最多 10s），nahook 路由上的每个请求都要等 5s，然后收到 502。设计原来写的"被拒后立即换下一个上游"不成立。
- 第二次实测：加上 `fail_duration 10s` 以后，停掉 nahook 后的第一个请求 0.26s 收到 502，Caddy 日志是 `readfrom tcp 127.0.0.1:59272->127.0.0.1:3000: body closed by handler`；紧接着的两个请求立即收到 503 `no upstreams available`。Go 的 Transport 在连接失败时也会关闭请求体，重试时往 3000 写请求体就失败了，这次失败还记在 3000 头上，两个上游都成了不可用。Caddy 维护者在 caddyserver/caddy#5454 里说，POST 要重试就得先缓冲请求体（该 issue 以 not planned 关闭）。
- 处理：nahook 路由写 `fail_duration 10s`（`max_fails` 默认 1），不写 `lb_try_duration`（不重试）。发现连不上的第一个请求立即收到 502，同时 19080 被标成不可用，后面的请求直接选 3000。没有采用 `request_buffers`：那会让所有请求都先在 Caddy 里缓冲（请求体最大 64MiB 以上），只为了崩溃那一瞬间；而且 2.6.2 上缓冲后能不能重试成功也没有验证。客户端主动断开不算失败（`proxyLoopIteration` 排除了 `context.Canceled`）；默认路由没配被动检查，不受影响。生产阶段 1（T11）沿用。

### Q18 站点层 `encode` 和 nahook 路由

- 证据：caddy v2.6.2 `modules/caddyhttp/encode/encode.go` 的 `Write` 只在头还没写、并且这次写入超过 `MinLength`（默认 512）时才调用 `init` 开始压缩；头还没写时 `Flush` 什么都不做；开始压缩以后，Flush 也不冲刷压缩器（01 audit K9）。nahook 放行时，把启动阶段读到的帧（`response.created` 加第一个非启动事件）一次写出（`internal/proxy/stream.go:19`），所以经过 nahook 时 Caddy 的首次写入比直连大。T10 用 mockrelay 实测：首次写入约 320 字节，没有压缩，逐帧正常。
- 影响（推论，未实测）：真实上游的 `response.created` 带回完整的 response 对象（含 instructions），很可能超过 512 字节；客户端带 `Accept-Encoding` 时，后面的事件可能被攒在压缩器里。直连路由也有这个问题，nahook 路由上更容易触发。
- 处理：2026-10-21 用户拍板：站点层不写 `encode`（00 design §6.1）。LT 在 T10 去掉；用户告知生产本来没有，K8 时确认。不再需要首帧超过 512 字节的 mockrelay 脚本项。已关闭。

### Q19 启动阶段客户端收不到心跳

- 证据：T10 T-F15，脚本 `hold_rate_limit:5, hold:3`：客户端 11.9s 内没有收到任何字节，放行时 3 行 `: PING` 和 `response.created` 同时到达。nahook 在判定之前不写 `w`，这是能返回 429 的前提（01 audit §1 第一条要点）。
- 影响：直连官方 NewAPI 并打开心跳时，客户端每隔几秒就能收到 `: PING`；经过 nahook 时，整个启动阶段（上游迟迟不回、每次重试、冷却等待）都是静默的，空闲超时比这段时间短的客户端或中间层会断开。常见情况下静默很短：LT 上真实的限流帧约 1.3s 就到（01 design §3）；上游一直不回响应头时，最长到 Caddy 的 30m。
- 处理：这是"先判定、后写出"这个设计固有的代价，不改；已写进 01 design §1.8。生产阶段 1、2 观察有没有客户端在启动阶段断开（`client_gone` 计数和 NewAPI 日志），和 K7（心跳设置）一起看。

### Q20 心跳之后 NewAPI 转发的上游错误

- 证据：T10 在关掉护栏的 h3（`38e69e3`）上跑 T-F17（心跳打开、间隔 1s，`RetryTimes=0`，脚本 `hold_http429:3`）。直连时 h3 回 200：3 行 `: PING`，然后是 `data: {"error":{"message":"Rate limit reached … Please try again in 28ms.",…}}` 和 `data: [DONE]`，消息末尾**没有** request id。经过 nahook 时，nahook 把它当成上游限流，重试了 3 次（mockrelay 共收到 4 次，冷却等待 2195ms、10254ms、11397ms），客户端 35.9s 后拿到代 429。原因在 h3 源码：`Relay` 的 defer 先 `SetMessage(MessageWithRequestId(…))`（`controller/relay.go:96-99`），再由 `writeRelayStreamError` 输出 `ToOpenAIError(…)`（`:242-258`）；而 `SetMessage` 只改 `e.Err`，`ToOpenAIError` 遇到 `ErrorTypeOpenAIError` 时直接返回上游原来的错误对象（`relaykit/types/error.go:178-190`）。所以只有 NewAPI 自己生成的错误带后缀，转发上游的 OpenAI 错误不带。rc.45 没有 `writeRelayStreamError`，错误由 `c.JSON` 直接追加（没有 `data:`），nahook 读到 EOF 放行，不受影响（T10 T-F17 rc.45 部分）。
- 影响：生产阶段 2（h3 关护栏）和阶段 3（如果换成带 `#7857` 的官方版本）上，只要心跳打开、上游的 HTTP 错误回得比心跳间隔慢（或者 NewAPI 的几次重试加起来比心跳间隔长），每个这样的请求对上游的调用会放大到 (1 + `retry.max`) × (1 + RetryTimes) 倍，客户端还要多等几轮冷却。护栏开着时（阶段 1）心跳在启动阶段不发出，h3 直接回 HTTP 429，不受影响（T10 实测）。
- 处理：01 design §1.6 增加第 2 条规则：判定帧之前只有心跳（至少一个纯注释事件，没有别的事件），并且判定帧不带 type，就认定是 NewAPI 的终态错误，原样透传。只会往放行的方向误判（上游很慢、心跳已发出、上游第一帧就是不带 type 的限流错误时，客户端看到的和官方一样）。由 T15 实现，部署到 LT 后用关护栏的 h3 复测 T-F17；T11 之前完成。K7：LT 的心跳一直开着、间隔 1s，生产和 LT 一样（用户告知），所以生产上一定会走到这条路径，T15 必须在 T12 之前部署到生产。

### Q6 被拦截那几次尝试的计费

- 证据：`NEWAPI/service/responses_usage.go:107` 的逻辑是：流失败且没有输出时，不估算输入 token。
- 影响：如果上游在 failed 帧里带了 usage，NewAPI 会按它计费。
- 处理：T-F9 已在 LT 上实测（§7.16）：mockrelay 的 failed 帧不带 usage，每次被拦截的尝试记 1 行消费日志，quota 0，内容是 "Upstream returned no usage, so nothing was charged"。真实上游在 failed 帧里带 usage 时会按它计费，阶段 2（P-2）看真实日志确认。

### Q7 源码放在哪、怎么构建

- 处理：用户拍板，仓库是 `github.com/ARYYDS/NaHook`（公开）。开发机副本只放 SPEC 和文档；代码由 Win11 上的 DSH 在 `O:\NaHook` 开发，构建也在那里做。工具链和缓存见 01 design §7。

### Q9 产物怎么从 Win11 送到 LT

- 现状（2026-10-20 实测）：Win11 上 `gh` 的令牌已失效，需要用户重新执行 `gh auth login`。
- 处理：用户拍板，仓库改为公开，产物通过 GitHub Release 分发。LT 从公开地址下载，不需要令牌；下载、安装、重启各自请示用户（01 design §7 发布与分发流程）。

### Q8 冷却门的 base 没有上限

- 证据：design §1.5 里 base 取的是最近一次被拦截尝试的耗时。如果中转站拖了很久才回错误，窗口最大可以到 4 × 这个耗时。
- 影响：等待时间变长，但不会无限：只有 `retry.max` 次，并且受 Caddy 的 30m 限制。
- 处理：T-F4 和 P-2 实测 base 的分布；如果需要上限，按实测值回写 design §5。

### Q10–Q15 T5、T7 开工前的 SPEC 补漏

- 证据：
  - Q10：design §2 的模块表、interface 2.5 和 2.6 的签名都用了 `*obs.Stats`，但 tasks 里没有哪个任务负责 `internal/obs`，计数器也没有名字。
  - Q11：interface §3 写的是 `classify` 读请求体出错时回 502，但 design §1.2 的签名没有错误返回值；§1.6 不区分"连不上 NewAPI"和"客户端已断开"，客户端断开时也会回 502 并算作 NewAPI 故障。
  - Q12：Go 的客户端请求设了 `RequestURI` 会报错；不设 `Host` 的话，NewAPI 看到的 Host 会变成 `127.0.0.1:3000`，而 Caddy 默认原样转发 Host。
  - Q13：T6 要改 `cmd/nahook`，它会调用 `proxy.NewHandler`；`control.Serve` 也要用 `obs.Stats`。
  - Q14：T-C3 要求"把输出拉长到约 30s"，T-F15 要求 PING 出现在限流帧之前，§6.6 里都没有对应的脚本项；原来 T-F15 写的 `hold:5` 之后接 `rate_limit` 是两个请求，测不到这个场景。
  - Q15：官方 NewAPI 处理流式请求时，在调用上游之前就启动心跳（`NEWAPI/relay/channel/api_request.go:478-485`），第一次心跳就把 200 提交了。之后如果上游回了错误，NewAPI 照样按 `DecideRelayRetry` 换渠道重试（重试循环 `NEWAPI/controller/relay.go:176-226` 和 `NEWAPI/service/relay_error.go:20` 都不检查响应是否已提交）；全部失败后，`controller/relay.go:99-101` 用 `writeRelayStreamError`（`:242`）把错误写成 `data: {"error":…}`，消息已经在 `:96` 被加上 ` (request id: <id>)`，这个 id 和响应头 `X-Oneapi-Request-Id` 是同一个（`middleware/request-id.go:12-16`）。
- 处理：见问题总表各行。`Accept-Encoding: identity` 保持不变：走到 nahook 的只有两条 relay 路由，NewAPI 只在 `/api`、dashboard 和前端静态文件上做 gzip（`NEWAPI/router/api-router.go:20`、`dashboard.go:13`、`web-router.go:23`）。

## 4. 待拍板

| 编号 | 内容 | 位置 |
|---|---|---|
| — | 目前没有待拍板项（D1、D2 已于 2026-10-21 拍板，见 §7.16） | — |

## 5. 开工前待查索引

| 编号 | 内容 | 来源 | 对应任务 |
|---|---|---|---|
| K1 | Caddy 是否传递客户端断开 | 01 audit §5 | 已查清（01:T0）；01:T10 的 T-C1、T-C2 用 HTTP/2 复测 |
| K2 | 真实中转站的限流帧形态 | 01 audit §5 | 01:T12 |
| K3 | 官方镜像要固定的 digest | 01 audit §5 | 已定：rc.45 `sha256:6c63c7ee…87bc3f9` |
| K4 | h3 与 rc.45 来回切换的数据库兼容性 | 01 audit §5 | 正向已验证；回退方向在 01:T10 演练 |
| K5 | 生产主机的 3000 端口是否暴露在公网 | 01 audit §5 | 00:G3 |
| K6 | LT 上测试分组的定价 | 01 audit §5 | 已查清（01:T9、T10） |
| K7 | 生产主机的心跳设置 | 01 audit §5 | LT 已查清（心跳开着、间隔 1s）；生产在 K8 时确认 |
| K8 | 生产主机是哪台、基线是否和 LT 改动前一致 | 01 audit §5 | 01:T11 之前 |
| K9 | 站点层 `encode` 会不会憋住 SSE | 01 audit §5 | 已处理：去掉 `encode`（D1） |

## 6. 还没写 SPEC 的部分与写作顺序

| 内容 | 现状 | 顺序 |
|---|---|---|
| 自动调度 v2（切渠道、探测、别名计费、粘滞 TTL） | 只有知识库 `渠道监控/自动调度/`，接入点是 01 design §1.6 的"拦截"分支 | 等 `自动调度/08-open-decisions.md` 里的 D1–D5 拍板，并且 01 上线之后，再写 `02-自动调度` |
| 运维手册 | 01:T14 产出，是文档，不是 SPEC | 01:T11 之后 |

## 7. 决策溯源与过程记录

### 7.1 裁决和设计落点

| 裁决 | 设计落点 |
|---|---|
| 采用 E 方案：挂在入口 Caddy 上，不改 NewAPI 源码和面板 | 00 design §2；01 design §0 |
| 下游入口是 Caddy | 00 design §6.1、§6.2 |
| 只推迟触发了限流拦截的请求；冷却门和全抖动由 hook 实现；异步、不用同步锁、不排队 | 01 design §1.5、§4 |
| 返回给 NewAPI 格式的 429（代 429）；没有重试次数时直接返回 | 01 design §1.4、§1.6 |
| 只处理流式；非流式不出限流 | 01 design §0、§1.2 |
| 不跟随 NewAPI 的策略，改成用函数直接修改上限，一行命令热重载 | 01 design §1.7、§5、§6.5 |
| 重试上限初始值 3 | 01 design §5 `retry.max` |
| 测试环境用甲（LT 宿主机上另起测试实例、SQLite、假上游） | 00 design §2、§6 |
| 上线时机：先在 h3 上关护栏验证，再换官方镜像 | 00 design §1.1 |
| Caddy 是否吞掉客户端断开，纳入测试 | 01 audit T-C1–T-C4；00 tasks §1 |
| 自动调度只以知识库形式记录 | `渠道监控/自动调度/` |
| 仓库用 `github.com/ARYYDS/NaHook`；在 Win11 的 `O:\NaHook` 上开发和构建；L-GO 车道交给 Win11 上的 DSH | 01 design §7；01 tasks §0；`docs/任务书-DSH.md`、`docs/开工提示词.md` |
| LT 上的每一次替换操作都先请示用户 | 01 tasks T0、T9、T11–T13；00 design §1.1 |
| T0 需要一份完整方案 | `docs/T0/测试方案.md` |
| SPEC 从 `渠道监控/SPEC/` 迁到 NaHook 仓库的 `spec/`，以仓库为准 | `spec/README.md` |
| 仓库改为公开；产物用 GitHub Release 分发 | 01 design §7；01 tasks T8、T9 |
| 公开前去掉：上游 org 编号、LT 的 SSH 登录方式 | `docs/reference/渠道监控/4-4-tpm-retry.md`（快照已脱敏）；本文件 Q9 |

### 7.2 写 SPEC 时作者做的调整（需要用户知悉）

| 调整 | 原因 |
|---|---|
| 配置文件从 YAML 改成 JSON | 只用标准库，不新增依赖（01 design §7） |
| 冷却门的 base 从"移动平均"改成"最近一次被拦截尝试的耗时" | 平滑系数找不到出处，按规范不能写死一个数（01 design §1.5） |
| 放行时把同一模型下所有 key 的 streak 一起清零（`ResetModel`） | 放行时还不知道 org（01 design §6.3） |
| 缓冲超限、坏 JSON、EOF 时放行，不回 502 | 可用为先，不能比官方行为更差（01 design §1.3、§8） |
| 启动帧阶段不另设计时器，依赖 NewAPI 默认 300s 的 `STREAMING_TIMEOUT` | 避免另写一个没有出处的数（01 design §4） |

### 7.3 过程记录

- 2026-10-20：在对话里展示了 v1 架构，用户确认了三项：上限初始 3、测试环境用甲、按推荐的时机上线。之后开始写本 SPEC。
- 写之前读过本专题目录里的 `architecture.md`、`hook-without-fork.md`、`compare-h3-vs-official.md`、`4-4-tpm-retry.md`、`429-vs-quota.md`，以及 `自动调度/` 全部文件。
- 行号核对基准：`NEWAPI` 是 `206e0fa5c`，`H3` 是 `38e69e300`。

### 7.4 T1 审计（提交 `79b71fe`，由架构侧直接审计、直接修复）

DSH 报上来的拍板项，架构侧的裁决：

| 拍板项 | 裁决 | 回写位置 |
|---|---|---|
| 门禁 ④ 在 T1–T5 没法执行 | 还没有 `cmd/` 时用 `go build -trimpath ./...` 交叉编译；从 T6 起必须用 `./cmd/...` | 01 audit §3 |
| Validate 的规则清单 | 采纳 DSH 的 7 条，再加 `streak_cap ≤ 23`，以及 `per_model` 的键不能为空、不能是 `*` | 01 design §6.4.1 |
| `SetRetryMax` 返回的 `old` | 修改前这个 scope 的生效值 | 01 design §1.7；interface 2.4 |
| `config.retry_max_changed` 日志放在哪 | 由 control 打，uid 用 SO_PEERCRED 取；Store 不打日志 | 01 design §1.7、§6.4 |
| 任务书 §1.4 第 7 项和 §4 第 1 步的先后顺序 | 改写措辞；每个任务开工前把 `origin/main` 合进 `dev` | 任务书 §1.4、§4；开工提示词第 2 段 |

审计发现的问题，都在 `audit/T1` 分支上修复：

| 编号 | 级别 | 问题 | 依据 | 修复 |
|---|---|---|---|---|
| F1 | P1 | `Load` 会静默忽略不认识的键，键名写错时上限会悄悄变成 0 | 社区同类问题：go-health-checker#3、sigillum#42；Go 文档里 `Decoder.DisallowUnknownFields` 的说明 | `DisallowUnknownFields`，并拒绝对象后面多余的内容 |
| F2 | P1 | rename 之后目录 fsync 失败时，磁盘已经是新内容，但内存快照没有切换 | cplieger/atomicfile 的 PhaseDirSync 说明 | 新增 `ErrDirSync`；rename 已完成就切换快照，同时把这个错误返回给调用方 |
| F3 | P2 | 空的 scope 会写进 `per_model`；`*` 当键和全局写法冲突 | — | `SetRetryMax` 拒绝空 scope；`Validate` 拒绝空键和 `*` 键 |
| F4 | P2 | `streak_cap` 没有上限，T3 的左移会溢出 | design §6.4.1 的推导 | 加上 `≤ 23` 的校验 |
| F5 | P2 | `Reload` 能改只在启动时生效的三个字段，改了也不起作用 | design §6.1 | 新增 `ErrRestartRequired`，快照不变 |
| F6 | P3 | 空集合落盘成 `null`，和 SPEC 样例不一致 | design §5 | `Persist` 统一写成 `{}` 和 `[]` |

### 7.5 T2 审计（提交 `33f5527`，由架构侧直接审计、直接修复）

DSH 交付回报里自己做的决定和提出的风险，架构侧的裁决：

| 事项 | 裁决 | 回写位置 |
|---|---|---|
| 读错误时把没收尾的最后一个事件也并进 Prefix | 采纳。原伪代码会丢掉这部分字节，违反 `Prefix+Rest == 原始字节` | 01 design §1.3 |
| JSON `null` 当作字段不存在 | 采纳。和 h3 一致：h3 的 `any` 字段遇到 null 时是 nil | 01 design §1.3 |
| `response.failed` 只带顶层 code/message 时要不要兜底 | 补上 h3 `:220-232` 的兜底逻辑。真实帧的形态还没见过（K2），兜底能多覆盖一种形态；不是限流的仍然放行，不会比现在更差 | 01 design §1.3；interface 2.1 |
| 顶层 error 分支额外填了 Type 和 Param | 不采纳，改回和 h3 一样只取 code 和 message。T-F1 要求代 429 和 h3 逐字节一致 | 01 design §1.3 |
| 小数形式的数字码值（`429.0`） | 和 h3 对齐：code 先解成 any 再编回去，判定用 `fmt.Sprint` | 01 design §1.3；interface 2.1 |
| 空对象 `{}` 算作取到错误，最后放行 | 维持现状 | — |
| 启动帧阶段不设总时限 | 维持现状。T5 的 `forward` 必须用 `r.Context()` 发请求，客户端断开（前提是 Caddy 能传过来，K1）时，读响应体会因 ctx 取消而返回 | 01 design §4、§1.6 |

审计发现的问题，都在 `audit/T2` 分支上修复：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| G1 | P1 | `ReadString('\n')` 对单行没有长度上限：上游发一个超长、又没有换行的行时，会先把整行读进内存，之后才检查 256KiB 的限制 | 新增 `readLine`，用 `ReadSlice` 逐块读，读到超过剩余预算就立即放行 |
| G2 | P1 | 顶层 error 分支多填了 Type 和 Param，代 429 和 h3 对不上 | 只取 code 和 message |
| G3 | P2 | 错误对象用固定类型的结构体解析：只要 type 或 message 不是字符串，整个对象就解析失败，被当成"没有错误" | 改成按 `map[string]any` 加类型断言解析，和 `GetOpenAIError` 一致 |
| G4 | P2 | 没有 h3 的 failed 兜底 | 补上 |
| G5 | P2 | code 原样保留：`429.0` 判定不中，输出也和 h3 不一样 | `canonical` 加 `codeText` |
| G6 | P3 | `response.status` 声明成 string：status 不是字符串时整帧解析失败 | 改成 `json.RawMessage`，再用 `stringValue` 取值 |

### 7.6 T3 审计（提交 `96203f1`，由架构侧直接审计、直接修复）

DSH 交付回报里的风险点和自行决定，架构侧的裁决：

| 事项 | 裁决 | 回写位置 |
|---|---|---|
| 存下来的 streak 不封顶（stats 会显示超过 streak_cap，累加到 2^31 会溢出） | 封顶：用 CAS 加 1，且不超过 streakCap。§1.5 和 §4 的写法统一 | 01 design §1.5；interface 2.3、§3 |
| Snapshot 是否列出从没被拦截过的 key | 全部列出。拦截期过后 streak 归零，但 until 和 base 还能说明最近发生过什么，便于运维 | — |
| `Wait` 的测试按墙钟计时，留 150ms 容差 | 接受，不为此注入时钟 | — |
| `Key` 的正则区分大小写，只认 `org-` 前缀 | 维持 SPEC 原样。认不出 org 时退化为 `model|-`，这是允许的降级 | — |
| `ResetModel` 依赖 `model|` 这个约定 | 维持。模型名本身带 `|` 时前缀可能误匹配，目前没有这样的模型名，记为已知限制 | — |
| 按 key 排序、`s<1` 的保护、`remain≤0` 时不看 ctx、`LoadOrStore` 写法 | 全部采纳 | — |

审计发现的问题，都在 `audit/T3` 分支上修复：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| J1 | P2 | `OnRateLimit` 存完 base 又从原子变量里读回来算窗口。并发时可能拿到另一个写者存的 base | 用本地值算窗口 |
| J2 | P2 | 存下来的 streak 不封顶 | 用 CAS 加 1，且不超过 streakCap |
| J3 | P2 | base 没有上限。`gate.base_floor_ms` 只校验了 ≥ 0，配置一个很大的值时 `base << (s-1)` 会溢出；elapsed 在 Caddy 不传断开时（K1）理论上也没有上限 | gate 里取 `min(…, 30m)`；config 校验 `base_floor_ms ≤ 1800000`。两处用的是 §6.4.1 同一个推导 |
| J4 | P1 | T1 审计定的 `streak_cap ≤ 23` 推导有误：只检查了窗口放不放得进 int64，漏了截止时间是"当前 UnixNano（约 1.8e18）+ 窗口"。30m<<22 ≈ 7.5e18，加上当前时间就超过 2⁶³，截止时间会翻成负数，冷却门也就失效了 | `maxStreakCap` 改为 22（design §6.4.1 已更正）；gate 对窗口和截止时间做饱和处理，绕过校验也不会翻转。§7.4 里"≤ 23"的裁决以这一条为准 |

### 7.7 T4 审计（提交 `15f355e`，由架构侧直接审计、直接修复）

DSH 交付回报里的风险点和自行决定，架构侧的裁决：

| 事项 | 裁决 | 回写位置 |
|---|---|---|
| tasks.md 写的"param 分别是 null 和字符串"和类型定义冲突 | 改成"空串和字符串" | 01 tasks T4 |
| 序列化失败时 429 已经提交、没有响应体 | 改成先序列化、后提交状态码；失败时把 code 换成 null 再序列化一次，保证响应体一定是合法 JSON | 01 design §1.4 |
| 不输出 metadata；黄金样例按完整字节固定；不断言 Content-Length | 全部采纳 | — |
| h3 的 429 还会带上 NewAPI 中间件加的其他响应头 | 按白名单复制：request id、`X-New-Api-Version`、`Vary`、`Access-Control-*`。`Write` 的第二个参数从 request id 改成上一次尝试的响应头 | 01 design §1.4、§1.6；interface 2.2 |

审计发现的问题，都在 `audit/T4` 分支上修复：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| K-a | P2 | 代 429 不带 CORS 头：浏览器端客户端带着 Origin 发请求时，h3 的 429 有 `Access-Control-Allow-Origin`，nahook 的没有，浏览器会因为跨域校验失败而读不到错误 | 按白名单从上一次尝试复制响应头 |
| K-b | P3 | 代 429 不带 `X-New-Api-Version` | 同上 |
| K-c | P3 | 状态码先提交、后序列化：序列化一旦失败，就是一个没有响应体的 429 | 先序列化；失败时把 code 换成 null 重试 |

修复提交 `ea75cf0`（`audit/T4`）。Win11 门禁：gofmt 无输出；vet 退出码 0；`go test -race ./...` 四个包全部 ok，顶层 55 个 PASS、0 个 FAIL；linux/amd64 交叉编译和 vet 退出码 0；go.mod 没有 require；行尾全部是 LF。

### 7.8 T5、T7 开工前的 SPEC 补漏（Q10–Q15）

| 改动 | 落点 |
|---|---|
| 新增 `internal/obs`：`NewLogger`、`Counter` 及 8 个常量、`NewStats`、`Inc`、`Snapshot`；不变量 `rate_limited = retries + gave_up` | design §6.7；interface 2.7；tasks T5 |
| `classify` 增加 error 返回值；`forward` 出错时先看 ctx：已取消就不写响应、计 `client_gone`，否则计 `bad_gateway` 并回 502 | design §1.2、§1.6；interface 2.5、§3 |
| §1.6 伪代码补上计数器和日志事件的位置；`BaseFloorMS` 换算成 `time.Duration` | design §1.6 |
| `forward`：URL 由 `newapi` 地址加路径和查询串拼成，不设 `RequestURI`；`Host = r.Host`；`*bytes.Reader` 请求体的 `ContentLength` 和 `GetBody` 由 `http.NewRequestWithContext` 自动设置（Go 源码 `net/http/request.go:942`），流式透传时手动设 `ContentLength = r.ContentLength` | design §6.1 |
| T6 前置加上 T5；T7 提到 T5 前面，先填 T0 结论出来之前的空档 | tasks §1、T5、T6 |
| mockrelay：`-listen`、`/__mode` 和 `/__log` 的格式、帧格式、固定内容；新增 `slow`、`hold_rate_limit`、`stall_rate_limit`、`hold_http429`；`http429`、`quota` 写明响应体；路径不匹配时回 400 | design §6.6；tasks T7；audit T-C3、T-F15 |
| NewAPI 自己的终态错误（消息末尾是本次 request id）原样放行，不重试 | design §1.6、§8；interface §3；tasks T5；audit T-F17、K7 |

### 7.9 T7 审计（提交 `91bf3aa`，由架构侧直接审计、直接修复）

DSH 交付回报里的自行决定和风险点，架构侧的裁决：

| 事项 | 裁决 | 回写位置 |
|---|---|---|
| `/__mode` 成功时回显 `{"script":[…]}` | 采纳 | 01 design §6.6 |
| 没设过脚本时回 503，并照样记账 | 采纳。官方默认不自动禁用渠道，打开后按状态码也只禁用 401，503 不会把测试渠道禁掉 | 01 design §6.6 |
| 帧里加了 OpenAI 帧的常规字段 | 采纳。官方 NewAPI 逐帧解析成 `dto.ResponsesStreamResponse`，这些字段都能解析；完整字节以测试里的黄金常量为准 | 01 design §6.6 |
| delta 文本 `Hello`、` world`、`!` 循环；`slow` 的发送时机；N=3 时和 `ok` 逐字节相同 | 采纳 | 01 design §6.6 |
| 秒数用 `strconv.Atoi` 校验，`01`、`+1` 也接受 | 接受：它们都是正整数 | — |
| 控制面统一用 `json.Marshal` 加一次 `Write` | 采纳 | — |
| 门禁 ④ 从 T6 起改用 `./cmd/...` | 改为日常门禁始终用 `./...`；`./cmd/...` 产出二进制，只在 T8 的构建脚本里执行。只编译 `./cmd/...` 会漏掉没被 `cmd` 引用的库包 | 01 audit §3 |

审计发现的问题，在 `audit/T7` 分支上修复：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| L-a | P2 | 业务请求的处理函数不读请求体。请求带请求体时，net/http 要等请求体读完才启动检测断开的后台读，而 NewAPI 发出的请求一定带请求体，所以 NewAPI 断开后 `hold`、`slow`、`stall` 仍会等满。原测试只传入已经取消的 ctx，测不到这条路径 | 等待之前先读完请求体；新增 `TestClosedConnectionStopsHold`，用真实 TCP 连接加请求体来测 |

修复提交 `5cc175c`。Win11 门禁：gofmt 无输出；vet 退出码 0；`go test -race ./...` 五个包全部 ok，顶层 80 个 PASS、0 个 FAIL；新测试连跑 3 次稳定；反证：把 `server.go` 换回 `91bf3aa` 的版本，新测试在 5.00s 时失败（"hold:5 kept waiting after the client closed its connection"）；linux/amd64 交叉编译和 vet 退出码 0；go.mod 没有 require；行尾全部是 LF。

排期调整：T5 要等 T0，原来的 T6 又依赖 T5，T7 合入后 DSH 就没有能开工的任务了。所以把 `obs` 从 T5 移到 T6，把 `cmd/nahook`（它要调用 `proxy.NewHandler`）从 T6 移到 T5。顺序改为 T7 → T6 → T5 → T8；T6 只依赖 T1、T3（01 tasks §1、T5、T6；`docs/任务书-DSH.md` §6）。

### 7.10 T6 审计（提交 `215077b`，由架构侧直接审计、直接修复）

DSH 交付回报里的自行决定和风险点，架构侧的裁决：

| 事项 | 裁决 | 回写位置 |
|---|---|---|
| control 的日志走 `slog.Default()`（因为 interface 2.6 的 `Serve` 签名里没有 logger） | 改为显式传入：`Serve` 增加 `log *slog.Logger` 参数，和 `proxy.NewHandler` 保持一致，不依赖全局状态。`cmd/nahook` 还没写，现在改接口成本最低 | 01 interface §1、2.6；design §6.4；tasks T5 |
| reload 的错误码按 §6.4 分类映射，§5 的"失败时回 500"只作兜底 | 采纳。§5 改为"按 §6.4 映射" | 01 design §5 |
| `PUT` 的 `n` 必填 | 采纳 | 01 design §5 |
| `DELETE` 日志里的 `new` 取回落后的全局值 | 采纳；同时写明 `old`、`new` 是在写锁外面读的，运维命令同时执行时日志可能对不上，配置本身不受影响 | 01 design §6.4 |
| `config.reloaded` 只带 `uid`；409 也记 `config.reload_failed`；错误体是 `{"error":…}` | 采纳 | 01 design §5、§6.4 |
| `Serve` 退出时删除 socket 文件；`nahookctl` 支持 `--x=y`、`--help`，负数交给服务端判断 | 采纳 | — |
| `ErrDirSync` 只在响应写出函数上测 | 接受：Windows 上造不出目录 fsync 失败 | — |
| `nahookctl` 不设超时 | 接受：短进程，控制面卡住时可以 Ctrl+C；没有出处的超时常数不加 | — |
| SO_PEERCRED 只做了交叉编译，没有在 Linux 上运行过 | 留给 T10 的 T-F11 在 LT 上实测 | — |

审计发现的问题，在 `audit/T6` 分支上修复：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| M-a | P3 | 从 `Listen` 到 `chmod 0600` 之间，socket 的权限由 umask 决定 | systemd 单元设 `UMask=0077`，放进 T8 的改动 | 
| M-b | P3 | control 依赖全局 `slog.Default`；测试要改全局 logger | 见上表第一行，提交 `5083dd8` |

临近模块的问题（T3 的 gate 测试，和本次交付相关：T6 的门禁里出现了一次失败）：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| M-c | P2 | `TestWaitReturnsOnContextCancel`、`TestWaitReturnsOnContextDeadline` 用 20ms 结束 ctx，而全抖动的等待时长取自 `[0, 2s)`，每次运行大约有 1% 的概率抽到更短的等待，计时器先到，测试失败，门禁时好时坏 | 随机取值改为包级变量 `jitter`（和 config 的 `syncDirFn` 同一做法），这两个测试把它固定为最长等待；生产代码仍用 `rand.N`，`TestWaitFullJitter` 仍用真随机。提交 `6d809c1` |

Win11 门禁（`6d809c1`）：gofmt 无输出；vet 退出码 0；`go test -race ./...` 八个包全部 ok，顶层 104 个 PASS、0 个 FAIL、1 个 SKIP（`TestSocketMode`，权限断言只在 Linux 上做）；修复后的两个测试连跑 200 轮共 400 次，全部通过；对照：修复前的版本连跑 300 轮共 600 次，失败 7 次（约 1.2%）；linux/amd64 交叉编译和 vet 退出码 0；go.mod 没有 require；行尾全部是 LF。

### 7.11 方案 A：LT 整机做测试，生产阶段改到生产主机（2026-10-21）

拍板记录：

| 时间 | 用户的决定 | 架构侧的执行 |
|---|---|---|
| 2026-10-21 | LT 已和实际生产隔离，只做测试；生产上有一套一模一样的环境 | 记入记忆；把 LT 上的改动和生产改动分开描述 |
| 2026-10-21 | 选方案 A；数据库快照已备份；只做卸载补丁的操作 | 见下面的"LT 卸载补丁" |
| 2026-10-21 | 按方案 A 改 SPEC | 见下面的"SPEC 落点" |

LT 卸载补丁（2026-10-21 13:19 CST，用户批准，执行前用户已完成 TDSQL-C 快照）：

| 步骤 | 结果 |
|---|---|
| 预检 | compose 的 sha256 前缀 `bbd7d4b9bcf07819`；容器 healthy；`/api/status` 显示 `main-20261020-38e69e3` |
| 拉取镜像 | `calciumion/new-api@sha256:6c63c7ee9cd5bf7880df3984e99dd3ef07992d7a97538cfb2719adf9d87bc3f9`，标签显示源码 `6cf0d90b5ff27f901697dddd61e0c28aba8efefc`、版本 `v1.0.0-rc.45` |
| 改 compose | 备份成 `compose.yaml.bak.20261021131926`；只改两行：`image` 换成上面的 digest，删掉 `RESPONSES_STREAM_STARTUP_GUARD_ENABLED`；`SQL_DSN`、`REDIS_CONN_STRING`、`SESSION_SECRET`、`REDIS_PASSWORD`、`GET_MEDIA_TOKEN` 这几行的哈希改前改后一致；`docker compose config -q` 通过 |
| 重建 | 只重建 `new-api`；35s 后 healthy，重启次数 0；Redis 没动 |
| 验证 | `/api/status` 显示 `v1.0.0-rc.45`；启动日志里 `already initialized at 2026-10-04 13:09:40`，渠道和系统设置都同步成功，ERR、panic、option 报错都是 0 |
| 回退 | 把备份文件拷回去再 `docker compose up -d new-api`；h3 镜像 `6249f475…` 还缓存在 LT 上 |

同一天的其他实测：
- LT 上 h3 除了护栏，在服务器端没有别的增加：没有 ntfy，也没有接任何外部监控站。Uptime Kuma 状态页的开关是开着的，但没有配置任何分组。
- NewAPI 容器能直接连到 `172.17.0.1`（`YJ-FIREWALL-INPUT` 链为空）。
- LT 的 Caddy 是 Debian 包 2.6.2。

SPEC 落点：

| 改动 | 落点 |
|---|---|
| 测试环境改成 LT 整机：经 `lt.xhub.chat` 的 nahook 路由测；撤掉原来单独的一套测试实例（3001 的 SQLite NewAPI、19081 的 nahook-test、19443 的测试站点、`test.json`、`nahook-test.service`） | 00 design §0、§2、§3.3、§4、§5、§6；01 tasks T8、T9、T10；01 audit §2 |
| 测试渠道和测试令牌放进独立分组，T9 验证隔离；Base URL `http://172.17.0.1:18080` | 00 design §6.2、§8 |
| h3 对照实例（T-F1）的启动方式写全：固定 digest，只绑定 127.0.0.1 | 00 design §6.3 |
| 生产阶段 T11–T13 改到生产主机上，开工前先查 K8 | 00 design §1.1、§1.2；00 tasks；01 tasks T11–T13；01 audit P-1–P-3、K8 |
| K3 定为 rc.45 的 digest；K4 记录正向已验证、回退方向在 T10 演练 | 01 audit §5；00 design §7、§8 |
| rc.45 的终态错误不带 `data:`，nahook 会放行并计 `released`；T-F17 按版本写预期 | 01 design §1.6、§8；01 audit T-F17 |
| `mockrelay.service` 写全，依赖 `docker.service` | 00 design §6.4 |

### 7.12 T0：Caddy 2.6.2 断开传递实验（2026-10-21）

执行：用户批准后，由执行子代理（DSH）按 `docs/T0/测试方案.md` 和 `测试要求.md`（文档提交 `1a1b60d`，脚本提交 `ea98b84`）在 LT 上执行，14:09:07–14:17:05 CST，没有重跑，没有偏离。架构侧随后在 LT 上只读复核了原始日志。

原始证据（`/root/nahook-t0/client.log`、`backend.log`；`delta_s` = 后端事件时间 − curl 断开时间）：

| 组合 | 3 次的后端事件 | delta_s | 后端写出的帧数 | 结论 |
|---|---|---|---|---|
| hold-default | `peer_closed` × 3 | −0.001 ～ −0.003 | — | 传递，立即 |
| hold-neg1 | `timeout_still_open` × 3 | +18.01 ～ +18.02（从收到请求起整 20s 没断） | — | 不传递 |
| stream-default | `write_failed` × 3 | +2.00 | 4 | 传递 |
| stream-neg1 | `write_failed` × 3 | +5.00 | 7 | 传递（慢） |

三次结果在毫秒级上一致，没有抖动。

解释（对照 caddy v2.6.2 源码）：

| 现象 | 机制 |
|---|---|
| hold-default 立即关闭 | 客户端断开 → Go 的 HTTP 服务器取消请求上下文 → transport 中止 RoundTrip、关闭到后端的连接 |
| hold-neg1 一直不关 | `reverseproxy.go:802-803`：`FlushInterval == -1` 时用 `ignoreClientGoneContext`（`:1423-1434`），它的 `Done()` 换成了 Caddy 模块自己的上下文。等待响应头时 Caddy 不往客户端写任何东西，也就没有写失败可以发现断开 |
| stream-default 2s | Caddy 立即关闭后端连接；后端下一次写进了内核缓冲区并引来 RST，再下一次写才失败：正好两个写周期 |
| stream-neg1 5s | 上下文被忽略；Caddy 把下一帧写给已断开的客户端时失败（同样要两个写周期），才返回并关闭后端响应体；后端再经过两个写周期才看到 |
| 去掉 `-1` 不影响流式 | `streaming.go:133-137`：上游是 `text/event-stream` 时一律立即 flush |

对 nahook 的结论：nahook 在等待冷却门和重试期间不向 Caddy 写任何字节，正好对应 hold 这一行。所以 nahook 路由必须不写 `flush_interval`，写了的话客户端走后 nahook 会继续重试到用完上限。不写时断开立即传递，不需要给 nahook 加单请求总时限。

复核：
- `t0_report.py` 在 LT 上重跑，结果和回报一致。
- 两次 `systemctl reload caddy` 在 14:09:55 和 14:16:50，都是 `load complete`。
- Caddyfile 已恢复，sha256 `c688d03e…` 和备份一致；19444、19082、19083 已释放；后端进程已退出。

局限：只测了 HTTP/1.1 回环。源码表明 `-1` 的行为只看配置值、和协议无关，T-C1、T-C2 在 `lt.xhub.chat` 上用 `curl --http2` 复测。

顺带观察（不在 NaHook 范围内，交给用户决定）：生产主站点的所有路径都写了 `-1`。没走 nahook 的请求（非流式请求、nahook 不可用时的回落）如果在 NewAPI 回出第一个字节之前客户端就断开了，NewAPI 会一直执行到结束，上游费用照样产生。

SPEC 落点：

| 改动 | 落点 |
|---|---|
| nahook 路由不写 `flush_interval`，写明原因和源码出处 | 00 design §6.1、§8 |
| 不设单请求总时限 | 01 design §4 |
| K1 销项；T-C1、T-C2 改用 HTTP/2，并写明两者的预期 | 01 audit §2、§5 |
| G1、T0 完成；T5 的前置满足 | 00 tasks G1；01 tasks T0、T5 |
| 补上 `cmd/nahook` 的规定（启动顺序、SIGHUP 日志、SIGTERM 时 Shutdown、退出码），T5 开工前补全 | 01 design §6.8；01 tasks T5 |

### 7.13 T5 审计（提交 `e84039e`，由架构侧直接审计、直接修复）

DSH 交付回报里的自行决定和风险点，架构侧的裁决：

| 事项 | 裁决 | 回写位置 |
|---|---|---|
| `classify` 的返回约定（整包读完时 `rest` 为 nil；超限时返回 `MultiReader`） | 采纳，和 design §1.2 一致 | — |
| `forward` 不复制 `Host`、`Content-Length` 两个头（由请求字段承载） | 采纳 | — |
| `joinPath`：`newapi` 带路径时不沿用客户端的 RawPath | 采纳；部署时 `newapi` 不带路径 | — |
| `isEventStream` 用 `mime.ParseMediaType` | 采纳 | — |
| 测试缝：`Handler.client` 可替换；`runWithSignals` 接收信号通道 | 采纳 | — |
| 启动失败的事件名 `startup_failed`；配置读取或校验失败写 stderr | 采纳 | 01 design §5、§6.8 |
| `NewHandler` 遇到解析不了的 `newapi` 时退化成空地址 | 采纳（`Validate` 已挡住） | — |
| 请求进行中热改上限只在代码路径上成立 | 接受，由 T-F3 在 LT 上验证 | — |
| SIGHUP 没有进程内测试 | 接受，由 T-F8 在 LT 上验证 | — |
| 读启动帧时客户端断开要不要计 `client_gone` | 要：见下表 N-c | 01 design §1.6、§6.7；interface §3 |
| `release` 先打日志再写 | 接受 | — |

审计发现的问题，在 `audit/T5` 分支上修复：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| N-a | P2 | 数据面的 `http.Client` 会自动跟随重定向：NewAPI 回 3xx 时，nahook 自己去请求新地址，碰到 301/302 还会把 POST 改成 GET，客户端拿不到原始的 3xx | `CheckRedirect` 返回 `http.ErrUseLastResponse` |
| N-b | P3 | 客户端没带 `User-Agent` 时，net/http 会补上 `Go-http-client/1.1`，NewAPI 看到的请求和直连时不一样 | 显式设成空值 |
| N-c | P3 | 读启动帧时客户端断开：`startup.Read` 因读错误返回放行，代码计 `released`、调 `ResetModel` 清零这个模型的冷却门，并往已断开的连接里写 | 放行分支先看 `r.Context()`：已取消就计 `client_gone`，不写，也不 `ResetModel` |

修复提交 `c6464a2`。Win11 门禁：gofmt 无输出；vet 退出码 0；`go test -race ./...` 十个包全部 ok，顶层 127 个 PASS、0 个 FAIL、1 个 SKIP（`TestSocketMode`，只在 Linux 上做）；`internal/proxy`、`cmd/nahook` 连跑 5 轮稳定；反证：把 `proxy.go`、`intercept.go` 换回 `e84039e` 的版本，三个新测试全部失败（写出了响应并把 streak 从 1 清成 0；客户端拿到 200 而不是 302；NewAPI 收到 `Go-http-client/1.1`）；linux/amd64 交叉编译和 vet 退出码 0；go.mod 没有 require；行尾全部是 LF。

### 7.14 T8 审计（提交 `3a387b5`，由架构侧直接审计、直接修复）

DSH 交付回报里的自行决定和风险点，架构侧的裁决：

| 事项 | 裁决 | 回写位置 |
|---|---|---|
| `BUILDINFO.txt`、`SHA256SUMS` 用 `[IO.File]::WriteAllText` 加 ASCII 编码写入，LF、不带 BOM | 采纳。架构侧从 GitHub 读回 `SHA256SUMS`，确认以 `\n` 结尾、没有 `\r` | — |
| `-o` 用正斜杠加结尾斜杠 | 采纳 | — |
| 构建失败时删除本次新建的 dist 目录 | 采纳这个意图，但实现有缺陷，见 O-a | — |
| 两个脚本都检查 HEAD 已推送；`release.ps1` 先用 `gh api` 探测仓库可达 | 采纳 | — |
| 样例的单测放在只有测试文件的 `deploy` 包里 | 接受 | — |
| 两个 unit 逐字照 00 §6.4 | 已核对，一致 | — |
| Release 附件不带可执行位 | 安装时用 `install -m 0755` | 01 design §7；tasks T9 |
| 没有签名，只有 sha256 | 接受：GitHub 记录的附件 digest 是第二个来源；另外 Release 可以复现，见下 | 01 design §7 |
| 脚本把路径写死 | 改为按脚本所在位置推导仓库，产物根目录通过参数传入（默认仍是 Win11 的布局） | — |

审计发现的问题，在 `audit/T8` 分支上修复：

| 编号 | 级别 | 问题 | 修复 |
|---|---|---|---|
| O-a | P2 | `build.ps1` 先把 `$script:dist` 设成目标目录（第 60 行），再检查它是否已存在（第 61–62 行）；而 `Stop-Build` 会删除 `$script:dist`（第 37–38 行）。所以对已经构建过的提交再跑一次，结果是把那个提交的 dist 目录删掉，而不是拒绝覆盖。附件在 GitHub 上还有一份，所以不会丢产物，但违背了"已存在就拒绝覆盖"的承诺 | 新增 `$script:created`，只在本次运行自己建了目录以后才赋值；`Stop-Build` 只删它 |
| O-b | P3 | `release.ps1` 上传前不核对产物：dist 目录被改动过，也会被原样发布 | 上传前核对 `BUILDINFO.txt` 的 commit 等于 HEAD，并按 `SHA256SUMS` 重算四个文件的哈希 |

修复提交 `22fcb96`，合并提交 `c1c9b5b`。只改了脚本，二进制不受影响，不需要重新发布，`build-3a387b58` 仍然是要部署的版本。

核验（Win11）：
- 门禁：gofmt 无输出；vet 退出码 0；`go test -race ./...` 十一个包全部 ok，顶层 128 个 PASS、0 个 FAIL、1 个 SKIP；linux/amd64 交叉编译和 vet 退出码 0；go.mod 没有 require；行尾全部是 LF；两个脚本用 PowerShell 解析器检查，没有语法错误。
- 复现：在一次性克隆里检出 `3a387b5`，执行 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath ./cmd/...`，`nahook`、`nahookctl`、`mockrelay` 三个二进制的 sha256 和 Release 附件逐字节一致。
- 修复后的 `build.ps1` 在临时 `-DistRoot` 下连跑两次：第一次退出码 0，产出五个文件；第二次退出码 1，输出 "refusing to overwrite"，第一次的目录和 `SHA256SUMS` 原样保留。原版因为路径写死在 `O:\NaHook` 和正式 dist 目录上，没有实际运行它来复现，缺陷证据是上面引用的代码行。
- 从 GitHub 读回的 Release：tag `build-3a387b58` 指向 `3a387b58141c48d0f0095e7de89ca3067e306a80`，prerelease，非草稿；五个附件的 digest 和 `SHA256SUMS` 一致。

过程说明：这次的 SPEC 提交落在合并提交 `c1c9b5b` 之后（以往是先提交 SPEC 再合并）。原因是第一次写 SPEC 的脚本报错退出，而合并命令在另一行照常执行了；合并内容是审计过的代码，不受影响。

### 7.15 T9：在 LT 上搭测试环境（2026-10-21，用户逐步批准）

| 步骤 | 做法 | 结果 |
|---|---|---|
| 测试渠道、测试令牌 | 用户在面板里创建 | 测试令牌的 `/v1/models` 只返回 `gpt-6-astra` |
| 下载 | `build-3a387b58` 的五个附件下载到 `/root/nahook-dist/3a387b58/` | `sha256sum -c` 全部 OK；`SHA256SUMS` 本身是 `14644afb…`，和 GitHub 记录一致 |
| 安装 | 三个二进制用 `install -m 0755` 装到 `/usr/local/bin`；两个 unit 和 `/etc/nahook/config.json` 取自 GitHub 上提交 `3a387b5` 的 `deploy/` 原文件 | 哈希和 Release、仓库一致；`systemctl enable --now` 后两个服务都是 active，nahook 监听 `127.0.0.1:19080`，mockrelay 监听 `172.17.0.1:18080`，控制口 `/run/nahook/ctl.sock` 是 0600 |
| Caddy 路由 | 架构侧在旁边生成新文件（00 §6.1 的路由，原 `reverse_proxy` 原样放进 `handle {}`），`caddy validate` 通过；替换和 reload 由用户执行（架构侧执行时被本机的权限分类器拦下，没有执行任何部分） | 新 Caddyfile 的 sha256 是 `d293d3f617c2ca13181f2ef3c9d59c8497a3ce4fe9b3bc26a85d98b66e9e530f`；备份 `Caddyfile.bak.20261021192639` 和 `Caddyfile.bak.t0` 逐字节相同；caddy active |

验收（T9 完成判据）：
- `https://lt.xhub.chat/api/status` 走 HTTP/2 返回 200；`nahookctl retry get` 返回 `max=3`，`stats` 正常返回。
- 用测试令牌经 `lt.xhub.chat`（HTTP/2）：
  - `/v1/responses`，mockrelay 为 `ok`：200，完整收到 `response.completed`；
  - `/v1/chat/completions`，`ok`：200，以 `data: [DONE]` 结束；
  - `/v1/responses`，脚本 `rate_limit, ok`：nahook 拦截，冷却门等待 46ms 后第二次尝试放行，客户端拿到 200 的完整流；mockrelay `/__log` 记了两条；
  - 三个请求前后，nahook 的 `intercepted` 从 3 变为 6，`released` 从 2 变为 5，说明这两条路径都经过了 nahook。
- 隔离：测试令牌只看得到 `gpt-6-astra`；所有响应都是 mockrelay 的内容（`resp_mock`、`chatcmpl-mock`），mockrelay 记录的条数等于发出的尝试次数，所以测试令牌的请求没有落到其他渠道。渠道报错时 NewAPI 换渠道重试的情况（mockrelay `http429`）由 T10 的相关项覆盖。

绕过 Caddy、直接请求 nahook 的冒烟（T10 之前的预检，不代替 T10）：
- 脚本一直是 `rate_limit`：上游共尝试 4 次（`retry.max=3`），返回 429，用时 0.14s；`Content-Type: application/json`，带 `X-Oneapi-Request-Id` 和 `X-New-Api-Version: v1.0.0-rc.45`，正文是上游的限流 error。
- 计数满足 01 design §6.7 的不变量：`rate_limited`(6) = `retries`(5) + `gave_up`(1)；`intercepted`(6) = `released`(5) + `gave_up`(1)。

观察：
- `passthrough` 计数包括 Caddy 每 10s 一次对 nahook 的 `/api/status` 健康检查，空闲 21s 加了 3。T10 读这个计数时要扣掉这部分；已写进 01 design §6.7 的计数表。
- reload 时 Caddy 提示 "Caddyfile input is not formatted"。原文件本来就不符合 `caddy fmt`（参数对齐用的空格、站点块开头的空行），不是这次引入的，不影响加载。
- 测试令牌的副本放在 LT 的 `/root/.nahook-test-token`（0600），供 T10 的 curl 读取，T10 结束后删除（00 design §5）。
- 主站点原有的 `reverse_proxy` 原样保留在 `handle {}` 里，包括 `flush_interval -1`；它和站点层 `encode` 的去留仍待用户决定，K9 在 T10 实测。

### 7.16 T10：在 LT 上跑审计清单（2026-10-21 起，进行中）

用户批准了四组：A 只调 mockrelay 和 nahookctl；B 改 Caddy、停启 nahook、写坏配置文件；C 改面板设置；D h3 对照实例和 K4 演练。替换 LT 的 Caddyfile 并 reload 的操作被本机的权限分类器拦下（T9 一次；T10 准备切换脚本时又一次，理由是绕过前一次拦截），这类操作改由用户在自己的终端里执行，架构侧只生成候选文件并 `caddy validate`。辅助脚本和中间文件在 LT 的 `/root/nahook-t10/`（0700）。

| 项 | 方法要点 | 结果 | 结论 |
|---|---|---|---|
| T-F2 | `retry set 2`，脚本一直是 `rate_limit` | mockrelay 3 次；客户端 429；`rate_limited` +3、`retries` +2、`gave_up` +1 | 通过 |
| T-F3 | 脚本 `rate_limit, hold_rate_limit:3, rate_limit`；第二次尝试被拖住时执行 `retry set 1 --model gpt-6-astra` | `config.retry_max_changed`（scope `gpt-6-astra`，3→1，uid 0）落在第一次 intercept 之后；第二次判定时 `max` 是 1，共 2 次尝试后 429（3.18s）；配置文件写入 `per_model: {"gpt-6-astra": 1}`；`unset` 后恢复 | 通过 |
| T-F4 | 16 个并发请求，脚本前 16 次 `rate_limit`、之后 `ok`，跑 4 轮。k 取进入冷却门的先后：nahook `wait` 日志的时间减去 `wait_ms` | 每轮 16 个都是 200，mockrelay 16 + 16；64 次重试全部落在 `[t0, until]` 内（窗口 246–275ms）；pearson(k, wait_ms) 四轮分别是 0.163、0.289、−0.119、0.210 | 通过：和 0 的差在 n=16 的随机波动（约 ±0.26）以内，排队的话会接近 1。略偏正，是因为 until 随后续的拦截往后推，先到的请求抽签区间更短 |
| T-F5 | 三类不拦截的请求（A `stream=false`、B 不命中的模型、C chat 不带 stream），各经 DIRECT、NH（19080）、SITE 发 100 次；响应体里的 request id 换成占位符后取哈希 | 每类 300 个响应只有一种响应体；状态码一致（A 500、B 503、C 200）；`intercepted` 不变。p50（A/B/C）：DIRECT 3.59/0.94/3.85ms，NH 3.53/0.98/3.86ms，SITE 16.51/13.77/16.84ms | 通过：nahook 本身的附加延迟在测量噪声以内；SITE 多出的约 13ms 是 Caddy 加上每次新建的 TLS 连接 |
| T-F6 | `ok`，经 SITE 和 DIRECT 各一次；用 `GET /api/log/token`（令牌鉴权、只读，rc.45 `router/api-router.go:359`）按 request id 取消费日志 | SSE 字节相同；两条日志都是 quota 125、prompt 10、completion 3、渠道 5 | 通过 |
| T-F8 | 把配置文件写成截断的 JSON 后 `systemctl reload nahook`，最后恢复原文件 | 日志 `config.reload_failed`（unexpected EOF）；pid 不变；`retry get` 仍是 3；`nahookctl reload` 退出码 1，返回同样的错误；期间拦截照常；恢复后 `config.reloaded`，文件哈希和原来相同 | 通过 |
| T-F9 | `rate_limit, rate_limit, ok` 经 SITE；用 nahook 日志里每次尝试的 request id 对日志 | 两次被拦截的尝试各 1 行消费日志，quota 0，内容 "Upstream returned no usage, so nothing was charged"；放行的那次 quota 125 | 已测：每次被拦截的尝试 $0、记 1 行；一个被拦截 n 次的请求共 n+1 行日志（Q6） |
| T-F10 | 同样的 `X-Forwarded-For`、`X-Real-IP`（203.0.113.7）经 NH 和 DIRECT 各发一次；再经 SITE 分别走 nahook 路由、默认路由，以及客户端自带 XFF。看 NewAPI 的 GIN 访问日志（消费日志的 ip 为空，测试账号没有开 IP 记录） | NH 和 DIRECT 都是 203.0.113.7；SITE 三种情况都是真实客户端 43.162.107.217 | 通过 |
| T-F11 | 看权限；用 `runuser -u nobody` 执行 `stats` 和 `retry set 9` | 目录 0700、socket 0600；nobody 两次都是 permission denied，上限仍是 3 | 通过 |
| T-F12 | `quota` | 200，原样放行；`rate_limited` 不变、`released` +1；mockrelay 1 次；和直连逐字节相同 | 通过 |
| T-F13 | `chat_rate_limit`，走 chat 路径 | mockrelay 4 次后 429 | 通过 |
| T-F14 | `rate_limit_failed_only` | mockrelay 4 次后 429 | 通过 |
| T-F16 | 67,109,936 字节的请求体经 SITE，再直连一次 | 200；`intercepted` 不变，只有 `passthrough` 增加；响应体和直连相同；两边都约 45s，耗时在 NewAPI 处理大请求体（直连 43.9s） | 通过 |
| T-C2 | 现行配置（不写 `-1`）；`hold:30`；`curl --http2 --max-time 2` 经 SITE | 客户端 2.0s 放弃；2.5s 时 `client_gone` +1，nahook 到 3000 的连接从 11 条变成 10 条；mockrelay 只收到 1 次 | 通过；加上 `fail_duration` 后和 T-C4 一起复测 |
| T-C3 | `slow:30`，客户端 3s 时断开 | NewAPI 消费日志的 `stream_status`：`end_reason=client_gone`、`end_error=context canceled`；容器日志显示 NewAPI 主动关掉了到上游的连接；按已收到的内容扣了 quota 55 | 通过 |
| T-C4 | `systemctl stop nahook` 后立即发请求，12s 后再发；然后启动 | 停掉后的第一个请求 **5.03s 后收到 502**（Caddy 日志 `dial tcp 127.0.0.1:19080: connect: connection refused`），没有回落；12s 时 200（走 3000）；启动后 1s 内流量回到 nahook | **不通过**，见 Q17。设计已改（00 §3.2、§6.1 加 `fail_duration 10s`），LT 上待用户装上后复测 |
| K9 | mockrelay `slow:4`，经 SITE 带和不带 `--compressed`，记录每行到达的时刻；对照 DIRECT、NH | 两种情况都是每秒到一帧，响应里没有 `Content-Encoding` | 用这个脚本测不出：首次写入约 320 字节，没有触发压缩，见 Q18 |

其他观察：
- 经过 nahook 时，`response.created` 要等到第一个非启动事件才一起发出（`slow` 下晚约 1s，直连 7ms 就到）。这是 01 design §1.3 的设计行为：启动事件只是"继续读"。客户端看到第一个 token 的时间不变。
- chat 路径上每个 chunk 都晚一拍才发出，直连也一样，这是 NewAPI 自己扣住上一块再发，和 nahook 无关。
- T-F3 里 `hold_rate_limit:3` 那次尝试让 base 变成 3004ms（`stats` 的 `base_ms`），说明 base 跟着单次尝试的耗时走（Q8 仍待 P-2 看真实分布）。
- 每组测试之后计数不变量都成立，例如 T-F4 之后 `rate_limited`(83) = `retries`(78) + `gave_up`(5)，`intercepted`(76) = `released`(71) + `gave_up`(5)。
- 在 LT 上准备好的 Caddy 候选文件（由 T9 的文件生成，`caddy validate` 通过）：`/root/nahook-t10/Caddyfile.final`（加 `fail_duration 10s`，sha256 `96e194dc9bd1ca721a796c397f192929493026d8e3214cb17e67ad43ff3c21a0`）和 `/root/nahook-t10/Caddyfile.tc1`（再加 `flush_interval -1`，只给 T-C1 用，sha256 `214e75233265f71cf88f8da5e1527e258f59e90ba2a6ff84deb288ecd3322eb2`）。

第二轮：用户先装上 `Caddyfile.tc1`，并把 LT 面板的失败重试次数改为 0、打开心跳并调到最小间隔；T-C1 之后再换成 `Caddyfile.final`。

| 项 | 方法要点 | 结果 | 结论 |
|---|---|---|---|
| T-C1 | nahook 路由临时写上 `flush_interval -1`（`Caddyfile.tc1`）；`hold:30`；`curl --http2 --max-time 2` 经 SITE | 客户端 2.0s 放弃；直到 35.5s，`client_gone` 一直是 0，nahook 到 3000 的连接一直开着；约 30s 时 mockrelay 回了 `ok`，nahook 记 `release attempt 0`，放行给了 28s 前就已经离开的客户端 | 符合预期：写了 `-1` 就不传递断开，HTTP/2 下和 T0 在 HTTP/1.1 下的结论一致。确认 nahook 路由不写 `-1`（00 §6.1） |
| T-C2（复测） | 装上 `Caddyfile.final`，做法同 T-C2 | 2.5s 时 `client_gone` +1，连接从 2 条变成 1 条，mockrelay 1 次 | 通过 |
| T-C4（第二次） | `Caddyfile.final`（加了 `fail_duration 10s`，仍有 `lb_try_duration 5s`）；停掉 nahook 后连发 3 个请求，12s 后再发一次 | 请求 1：0.26s 后 502，Caddy 日志 `readfrom tcp 127.0.0.1:59272->127.0.0.1:3000: body closed by handler`；请求 2、3：立即 503 `no upstreams available`；12s 时 200；启动后 6s 流量回到 nahook | **不通过**：重试时请求体已被关闭，3000 也被记了一次失败（Q17）。设计改为不写 `lb_try_duration`，候选文件 `Caddyfile.final2`，待用户装上后复测 |
| T-F7 | `http429`；`RetryTimes=0` | mockrelay 1 次；客户端 429，和直连逐字节相同；`passthrough` +1，`rate_limited` 不变 | 通过。rc.45 自己回 429 时，`Content-Type` 是 `text/event-stream`；nahook 只看状态码，不受影响 |
| T-F15 | 心跳打开；脚本 `hold_rate_limit:5, hold:3`（等响应头期间的 PING）和 `stall_rate_limit:5, hold:3`（流式阶段的 PING） | 两次都是 intercept → wait（3872ms、1880ms）→ `release attempt 1`；客户端 200，流里有 3 行 `: PING` | 通过。客户端在放行之前收不到任何字节（11.9s、9.9s），见 Q19 |
| T-F17 | 心跳打开、`RetryTimes=0`；`hold_http429:3` | mockrelay 1 次；客户端 200：3 行 `: PING`，然后是直接追加的 JSON 错误（没有 `data:`）；`rate_limited` 不变，`released` +1 | rc.45 部分通过。和原判据不一样的地方：rc.45 追加的错误消息**末尾没有 request id**。这不影响 rc.45（读到 EOF 就放行），但 request id 规则针对的是写 SSE 错误帧的版本（h3），所以改在 h3 对照实例上补测（audit T-F17 已改） |

新增的 Caddy 候选文件 `/root/nahook-t10/Caddyfile.final2`：由 `Caddyfile.final` 去掉 nahook 路由的 `lb_try_duration 5s`，`caddy validate` 通过，sha256 `40093312f74e4e2d5602fc8b99d66aef2da10a03abab5c73175af05ecc43fa9a`；`caddy adapt` 确认 nahook 路由只剩 `first` 策略和 `fail_duration 10s`，默认路由不变。

还没做：T-C4 第三次（装上 `Caddyfile.final2` 以后）；LT 面板改回原值（用户）；T-F1 和 h3 上的 T-F17（等用户初始化 3002）；K4（D 组）；K9 的大首帧实测（待用户决定 D2）。

第三轮：用户在 TDSQL-C 上新拍了快照，面板设为失败重试次数 0、心跳打开（间隔 1）；Caddy 已装上 `Caddyfile.final2`。

| 项 | 方法要点 | 结果 | 结论 |
|---|---|---|---|
| T-C4（第三次） | `Caddyfile.final2`（`fail_duration 10s`，不写 `lb_try_duration`）；停掉 nahook 后连发 5 个请求，12s 后再发；然后启动 | 请求 1：14ms 后 502（Caddy 只记了一条 `dial tcp 127.0.0.1:19080: connect: connection refused`）；请求 2–5：200，由 NewAPI 完成（mockrelay 4 次）；12s 时 200；启动后 4s 流量回到 nahook | 通过（Q17 关闭） |
| T-C2（final2 上复测） | 同 T-C2 | 2.5s 时 `client_gone` +1，mockrelay 1 次 | 通过 |
| K4 | 先备份 rc.45 的编排（`compose.yaml.bak.20261021232403.rc45`），再用上午的 h3 编排备份生成 `compose.h3.yaml`（镜像从 `new-api-h3:latest` 改成固定 digest `6249f475…`；其他行和当前文件相同，密钥 6 行的哈希一致），`docker compose up -d new-api`；之后改成护栏关闭再重建一次；最后恢复 rc.45 的编排 | rc.45 → h3：31s healthy，`main-20261020-38e69e3`，迁移正常、`already initialized`、渠道已同步，ERR/panic 0。h3 关护栏：31s healthy，ERR 0。h3 → rc.45：30s healthy，`v1.0.0-rc.45`，编排哈希回到 `8d3b40f3…`，ERR/panic/迁移报错 0；切回后 `ok`、`rate_limit, ok`、`rate_limit` 三个脚本经 nahook 都正常 | 通过：两个方向的切换都没有数据库问题（K4 关闭，Q4 的回退方向已演练） |
| T-F1 | nahook 一侧：`retry set 0`，`rate_limit` 直接请求 19080；h3 一侧：K4 期间 LT 主实例是 h3（开护栏、`RetryTimes=0`），`rate_limit` 直接请求 3000。没有用 3002 的对照实例 | 两边都是 `HTTP/1.1 429`、`Content-Type: application/json; charset=utf-8`、`Content-Length: 238`，响应体逐字节相同（sha256 `78e3993361b6c5d6…`），只有 `X-New-Api-Version` 和 request id 不同 | 通过。顺带确认：这个响应体和 rc.45 自己回 429 时（T-F7）也相同 |
| 阶段 1 预演 | h3 开护栏，经 nahook 发 `rate_limit` | h3 自己回 HTTP 429；nahook `passthrough` +1，`rate_limited` 不变；mockrelay 1 次 | 符合 P-1 的预期：护栏开着时 nahook 只透传 |
| T-F17（h3 开护栏） | 心跳打开、`RetryTimes=0`，`hold_http429:3`，经 nahook | 客户端直接拿到 HTTP 429（3.0s），没有 `: PING`；nahook 透传；mockrelay 1 次 | 护栏让心跳在启动阶段不发出，这条路径测不到；改在关护栏的 h3 上测 |
| T-F17（h3 关护栏） | 同上，h3 护栏关闭 | nahook 拦截 4 次、重试 3 次，mockrelay 4 次，客户端 35.9s 后拿到代 429；直连时 h3 回 200：`: PING` ×3、`data: {"error":…}`（没有 request id）、`data: [DONE]` | **不通过**，见 Q20。设计已改，待 T15 |
| 阶段 2 预演 | h3 关护栏，经 nahook：`rate_limit, ok` 和一直 `rate_limit` | 前者 200（拦截、等待、重试后放行）；后者 4 次尝试后代 429 | 符合 P-2 的预期 |

收尾：面板（K4 期间设成失败重试次数 0、心跳打开）待用户改回原值。3002 的 h3 对照实例没有用上，按 00 design §6.3 由用户删除（`docker rm -f nahook-h3-ref`，再删 `/root/container/nahook-h3-ref/data`）。

还没做：T15 实现、发布、在 LT 上替换 nahook 后用关护栏的 h3 复测 T-F17（切换方法同 K4，需要用户同意）；K9 的大首帧实测（待用户决定 D2）。

拍板（2026-10-21，用户）：

| 编号 | 用户的决定 | 架构侧的执行 |
|---|---|---|
| D1 | 同意建议；默认路由的 `flush_interval -1` 和站点层 `encode zstd gzip` 都直接去掉。这是测试时留下的配置，其他站点都没有这两行，也没有 gzip，不影响正常使用 | 由 `Caddyfile.final2` 删掉这两行生成 `Caddyfile.final3`（`caddy validate` 通过；`caddy adapt` 确认两个 `reverse_proxy` 都没有 `flush_interval`，站点没有 `encode`），用户执行替换和 reload；00 design §6.1、§8 改成现行设计 |
| D2 | 同意建议 | 去掉 `encode` 以后 K9 没有可测的对象，T16 不再新建；Q18 关闭 |
| K7 | LT 的心跳一直是开着的，从没关过（间隔 1s） | 01 audit K7 写明；Q20 的修复（T15）是生产阶段 2 之前的硬前置 |
| — | 面板已改回（失败重试次数 3、心跳间隔 1）；3002 对照实例已删除 | 已核对：`docker ps -a` 里没有 `nahook-h3-ref`，数据目录不存在 |

第四轮验证（`Caddyfile.final3`，sha256 `d596319d293a93156b780ebfcc773971c70bc1a8d362afb869cb42ebc3f1518d`）：
- reload 后没有新的错误日志；面板首页 200，带 `Content-Encoding: gzip`（NewAPI 自己的预压缩）；`/api/status`、`/v1/models` 200。
- nahook 路由：`ok` 200，没有 `Content-Encoding`；`rate_limit, ok` 照常拦截后放行。
- 默认路由的流式：`/v1/messages`（NewAPI 转成 chat 发给 mockrelay，`slow:3`）经站点是 2020ms、3020ms 两批到达，直连 NewAPI 是 2008ms、3008ms，只差 TLS 的约 12ms，Caddy 没有攒住事件。两批而不是三批，是 NewAPI 的 chat 处理扣住上一块再发，直连也一样。

T10 还剩：T15 部署到 LT 后，用关护栏的 h3 复测 T-F17。
