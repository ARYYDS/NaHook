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
| Q1 | R3 | 入口·Caddy 断开传递 | 不明 | P1 | 00、01 | 待 T0 / T-C1 / T-C2 实测 |
| Q2 | R3 | 判定·真实帧形态 | 不明 | P1 | 01 | 待 K2（阶段 2 确认）；message 兜底规则已覆盖大多数情况 |
| Q3 | R3 | 部署·3000 端口暴露在公网 | 脆弱 | P1 | 00 | 待 K5 |
| Q4 | R4 | 部署·换镜像后数据库回退 | 脆弱 | P1 | 00 | 待 K4 |
| Q5 | R3 | 测试·测试实例的模型定价 | 未细分 | P2 | 00 | 待 K6 |
| Q6 | R2 | 计费·被拦截的尝试是否为 $0 | 不明 | P2 | 01 | 待 T-F9 |
| Q7 | — | 工程·源码放在哪、怎么构建 | 未细分 | P2 | 01 | 已关闭：仓库 `ARYYDS/NaHook`，在 Win11 的 `O:\NaHook` 上开发和构建 |
| Q9 | — | 工程·产物怎么从 Win11 送到 LT | 未细分 | P2 | 01 | 已关闭：用 GitHub Release（仓库已公开） |
| Q8 | R2 | 冷却门·base 没有上限 | 脆弱 | P2 | 01 | 待实测（T-F4、P-2） |
| Q10 | R5 | 观测·`internal/obs` 没有任务、接口和计数器定义 | 未细分 | P1 | 01 | 已关闭：design §6.7、interface 2.7，并入 T5 |
| Q11 | R5 | 转发·失败路径（读请求体出错、ctx 取消）在 design 和 interface 里不一致 | 不明 | P2 | 01 | 已关闭：`classify` 增加 error 返回值；ctx 已取消时不写响应、计 `client_gone`（design §1.2、§1.6） |
| Q12 | R5 | 转发·`forward` 没写 Host、URL 和请求体长度的规则 | 未细分 | P2 | 01 | 已关闭：design §6.1 |
| Q13 | R5 | 排期·T6 漏了对 T5 的依赖；T5 要等 G1，但排在 T7 前面 | 耦合 | P2 | 01 | 已关闭：`obs` 放进 T6，`cmd/nahook` 放进 T5；顺序改为 T7 → T6 → T5，T6 不依赖 T0（§7.9） |
| Q14 | R5 | 测试·audit 用到的 mockrelay 行为（拉长输出、PING 之后再限流）在 §6.6 里没有定义；`/__log`、`http429`、`quota` 的格式没写 | 未细分 | P2 | 01 | 已关闭：design §6.6 重写，共 11 个脚本项 |
| Q15 | R2 | 重试·打开心跳后，NewAPI 的终态错误以 200 SSE 的形式出现，nahook 会再重试一轮 | 耦合 | P1 | 01 | 已关闭：消息末尾是本次 request id 时原样放行（design §1.6、§8；T-F17）；生产上的心跳设置待 K7 |

## 3. 逐项说明

### Q1 Caddy 2.6.2 + `flush_interval -1` 时客户端断开

- 证据：
  - Caddy 文档说负值时"不取消发往后端的请求"；
  - caddy#6666 记录了这个行为来自 #4952，2024 年 11 月回退；
  - LT 的生产站点配置了 `flush_interval -1`（2026-10-20 读过 Caddyfile）。
- 影响：如果断开传不到 nahook，客户端走了以后 nahook 还会按上限继续重试。
- 处理：T0 先按 `docs/runbook/T0-caddy-disconnect.md` 做最小实验（4 种组合各跑 3 次），T-C1/T-C2 再在完整链路上复测。
- 结论：待 T0 实测后回填到这里。

  > 推论：2.6.2 发布于 2022 年 10 月，#4952 在那之前合入，所以大概率受影响。

### Q2 真实中转站的流内限流帧

- 证据：`429-vs-quota.md` 只记录了 message 原文和 `type=tokens`、`code=rate_limit_exceeded`；h3 的日志只有 message。
- 影响：`upstreamError` 可能从帧里取不到结构化的错误。
- 处理：`IsRateLimit` 增加了"message 里含 Rate limit reached"的兜底规则；阶段 2 开始后，把前几次拦截的原始帧截断后记日志，确认帧形态。

### Q3 3000 端口暴露

- 证据：容器的端口映射是 `0.0.0.0:3000->3000`（2026-10-20 `docker ps`）。
- 影响：直连 3000 的客户端会绕过 nahook。
- 处理：K5，需要用户在云控制台确认安全组。

### Q4 换镜像之后数据库回退

- 证据：`git diff --stat 32e587406..206e0fa5c -- model/` 显示 `model/usedata.go` 和 `model/log.go` 有改动，其中 `usedata.go` 新增了带 gorm 标签的列。
- 影响：回退到 h3 时，库里会多出这些列。
- 处理：K4，在测试环境验证；阶段 3 之前备份 MySQL。

  > 推论：GORM 读数据时会忽略多出来的列，回退大概率可行，但必须实测。

### Q5 测试实例的模型定价

- 影响：测试 NewAPI 不配价格的话，可能会拒绝请求。
- 处理：K6，先验证"自用模式"能不能免去定价。

### Q6 被拦截那几次尝试的计费

- 证据：`NEWAPI/service/responses_usage.go:107` 的逻辑是：流失败且没有输出时，不估算输入 token。
- 影响：如果上游在 failed 帧里带了 usage，NewAPI 会按它计费。
- 处理：T-F9 实测并记录。

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
| — | 目前没有待拍板项 | — |

## 5. 开工前待查索引

| 编号 | 内容 | 来源 | 对应任务 |
|---|---|---|---|
| K1 | Caddy 是否传递客户端断开 | 01 audit §5 | 01:T0、T10 |
| K2 | 真实中转站的限流帧形态 | 01 audit §5 | 01:T12 |
| K3 | 官方镜像要固定的 digest | 01 audit §5 | 01:T9、T13 |
| K4 | 换镜像后能否回退数据库 | 01 audit §5 | 01:T10 |
| K5 | 3000 端口是否暴露在公网 | 01 audit §5 | 00:G3 |
| K6 | 自用模式能否免去定价 | 01 audit §5 | 01:T10 |
| K7 | 生产 NewAPI 的心跳设置 | 01 audit §5 | 01:T11 |

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
| T0 需要一份完整方案 | `docs/runbook/T0-caddy-disconnect.md` |
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
