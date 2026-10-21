# 监测频率、触发方式、官方缺陷

结论先说：**内置渠道监测是周期轮询，不是 429 事件触发。** 用户请求撞到限流时，不会因此立刻测渠道、也不会默认把渠道禁用。所以要观察「流式 200 SSE 限流 / 能否换站」，必须另加打真实协议的监测程序。

## 1. 具体频率

| 机制 | 默认 | 谁改 | 实际周期 |
|---|---|---|---|
| 渠道自动测试 | **关** | `monitor_setting.auto_test_channel_enabled` 或 `CHANNEL_TEST_ENABLED` | 打开后才跑 |
| 测试间隔 | **10 分钟** | `monitor_setting.auto_test_channel_minutes` 或 `CHANNEL_TEST_FREQUENCY`（分钟，整数；该环境变量还会强制打开测试并改成 `scheduled_all`） | `now - 上次任务 UpdatedAt >= interval` |
| 调度器扫描 | 15 秒 | 写死 `systemTaskSchedulerInterval` | 最多每 15 秒看一次「该不该建下一轮任务」 |
| Runner 空闲轮询 | 15 秒 | 写死 `systemTaskRunnerIdleInterval` | 认领 pending 任务；从节点不跑（仅 master） |
| 租约心跳 | 20 秒（TTL 60s 的 1/3） | 写死 | 防多实例抢跑 |
| 渠道间探测间隔 | 0 | `POLLING_INTERVAL` 秒 → `RequestInterval` | 测完一条再 sleep，默认 0 |
| 探测并发 | 1 | `monitor_setting.channel_test_concurrency`（1–32） | 一轮里并行测几条 |
| 响应过慢禁用阈值 | 5 秒 | `ChannelDisableThreshold` | **只在定时/批量探测里生效**，实时代理请求不按这个切 |
| 上游模型检测 | 开 | `CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED` | 默认间隔见该任务常量（分钟级） |
| MJ / 异步任务轮询 | 有未完成任务才调度 | — | **15 秒** |
| Uptime Kuma | 控制台打开才请求 | `console_setting.uptime_kuma_*` | **没有后台频率**：每次前端打 `GET /api/uptime/status` 才去拉 Kuma |

渠道测试到期算法（`runSystemTaskScheduler`）：

```text
若该类型已有 pending/running → 不建
否则若 now - latest.UpdatedAt < Interval 秒 → 不建
否则 CreateSystemTask(channel_test)
```

打开自动测试后，第一次会在 15 秒内入队；之后大约每 `auto_test_channel_minutes` 一轮。一轮要测完全部入选渠道才结束，渠道多、并发 1 时，一轮本身可能超过 10 分钟。

## 2. 触发逻辑：轮询为主，事件只走禁用

```text
A. 周期（默认关）
   master ticker 15s → 到期 → 入队 channel_test → 按 mode 选渠道 → testChannel()

B. 人工
   GET /api/channel/test      → 立刻入队（测全部，并通知）
   GET /api/channel/test/:id  → 同步测一条，不入队

C. 运行时错误（用户请求）
   ProcessChannelError
     → 记错误日志
     → 若 ShouldDisableChannel：异步 DisableChannel
     → 不入队 channel_test
     → 不根据 429 禁用（默认 AutomaticDisableStatusCodes = 401）
```

**429 不会触发探测。** `DecideRelayRetry` 只决定这一次用户请求要不要换渠道，和监测任务无关。

自动测试用的协议：`shouldUseStreamForAutomaticChannelTest` **只有 Codex 渠道走流式**。OpenAI/Azure Responses 的定时探测是非流 JSON。实验室里打出 429 的路径是 `POST /v1/responses` + `stream:true`，内置探测打不出同一种故障。

运行时 `UpdateResponseTime` 只在探测函数里调用，用户流量的耗时不更新渠道响应时间，因此「过慢 5 秒禁用」也看不到线上 TPM 排队。

## 3. 为什么要额外监测程序

内置三样东西覆盖不了「流式 429 切站」：

1. **Uptime Kuma 面板**只拉第三方状态页 HTTP，不知道 NewAPI 渠道、模型、TPM、SSE。
2. **定时 testChannel** 默认关、10 分钟、非流、429 不禁用，限流窗口（常 1 分钟级）过完才测到，测到也还是「渠道仍启用」。
3. **运行时日志**要人翻 `use_channel` / `retry_status_matched`，没有对外的「当前是否在限流、是否已切到 B 站」探针。

额外程序要做的是生产同款请求：

- `POST /v1/responses`，`stream: true`
- 固定模型、固定分组、可打出 TPM 的前缀
- 看 HTTP 状态、SSE 是否出现 `rate_limit_exceeded`、`admin_info.use_channel` 是否多于一个 ID
- 护栏打开时：客户端不应在第一次尝试就收到 200 SSE 错误帧

这不是替代 NewAPI 的选路，是验证选路和补丁有没有在真实限流下工作。

## 4. 官方 NewAPI 的缺陷（相对这件事）

1. **Responses 流式限流形态是 200 SSE。** `DoRequest` 先写 event-stream 头。上游 `rate_limit_exceeded` 进客户端后，重试循环无法整单换渠道。非流 JSON 的 HTTP 429 可以重试，流式这条路断了。
2. **`RetryTimes` 默认 0。** 即使是 HTTP 429 JSON，出厂也不换站。
3. **自动测试默认关，间隔 10 分钟。** 不是事件驱动。
4. **自动禁用默认关，禁用码默认只有 401。** 429 渠道保持启用，下一次仍可能抽到它（同 Priority 权重随机尤其如此）。
5. **定时探测协议与线上不一致。** 除 Codex 外非流；打不出「200 SSE 限流」。
6. **过慢禁用只看探测 RTT。** 用户请求的排队/TPM 等待不进入 `ChannelDisableThreshold`。
7. **Uptime Kuma 与渠道健康解耦。** 状态页绿不能代表网关能出 token。
8. **启动护栏默认关**（本树补丁）。官方行为保持 200 SSE 直出。要切站必须显式 `RESPONSES_STREAM_STARTUP_GUARD_ENABLED=true`，再配 Priority 阶梯和 `RetryTimes`。

事件驱动只存在于「401 等禁用码 → 异步 DisableChannel」。限流、流内错误、TPM 墙都不走这条事件。
