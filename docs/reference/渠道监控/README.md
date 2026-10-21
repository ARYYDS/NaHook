# 渠道监控索引

本目录是 `new-api-h3` / LT / 官方 NewAPI 的 429 与渠道监测笔记。行号按当前文件，打开对应 md 跳到该行。

路径：

- 本机：`D:\NewAPI_Dev\new-api-h3\docs\渠道监控\`
- win-vps：`C:\code\NewAPI\docs\渠道监控\`

## 文档索引（标题行号）

| 文件 | 行 | 节 |
|---|---|---|
| [4-4-tpm-retry.md](4-4-tpm-retry.md) | 1 | LT `4→4`：同渠道 TPM 重试、RetryTimes、等待锁 |
| | 5 | `4→4` 是什么 |
| | 17 | 这条 429 在说什么（40M TPM / 28ms） |
| | 28 | 失败详情 `eRqlLRZV` |
| | 41 | 同一秒部分成功、部分失败 |
| | 53 | RetryTimes 在哪设（面板，不是护栏） |
| | 61 | 护栏挂点 |
| | 80 | 等待锁该加在哪 |
| [architecture.md](architecture.md) | 1 | 渠道监控架构与 429 实现 |
| | 5 | 三层结构 |
| | 28 | 官方：429 怎么到客户端 |
| | 60 | 补丁：启动护栏把 200 SSE 收成 429 |
| | 112 | 换到其他中转站 |
| | 127 | 探测层 |
| | 152 | 配置清单 |
| [trigger-and-gaps.md](trigger-and-gaps.md) | 1 | 监测频率、触发方式、官方缺陷 |
| | 5 | 具体频率 |
| | 31 | 轮询为主，事件只走禁用 |
| | 55 | 为什么要额外监测程序 |
| | 72 | 官方缺陷 |
| [429-vs-quota.md](429-vs-quota.md) | 1 | TPM 429 还是额度耗尽 |
| | 5 | 实验室报错原文 |
| | 21 | 事件驱动能不能分开判 |
| | 37 | 建议的事件判定 |
| | 49 | 现网缺口 |
| [hook-without-fork.md](hook-without-fork.md) | 1 | LT 补丁与不改官方源码的 hook |
| | 3 | LT 实际部署 |
| | 30 | 官方有没有挂钩面 |
| | 42 | 出站 hook（渠道 Base URL） |
| | 83 | 入站 sidecar |
| | 87 | 和 LT 现状对照 |
| [compare-h3-vs-official.md](compare-h3-vs-official.md) | 1 | h3 与官方差异 |
| | 12 | 源码逻辑差（仅 `38e69e300`） |
| | 50 | 不改官方源码怎么接 |
| | 74 | 对照表 |
| [encrypted-reasoning-failover.md](encrypted-reasoning-failover.md) | 1 | 原渠道可用、切渠道秒报错 |
| | 5 | 思考链加密是什么 |
| | 27 | 和 429 护栏怎么叠 |
| | 45 | 官方循环做不到的事 |
| | 51 | 处理方式 |
| | 62 | 社区有没有人解密 |
| [strip-encrypted-content.md](strip-encrypted-content.md) | 1 | 换站时如何把思考链断干净 |
| | 5 | 必须一起清掉的字段 |
| | 22 | 社区解法清单 |
| | 24 | 先钉死 |
| | 33 | 主动剥 |
| | 54 | 被动剥（400 再清一次） |
| | 72 | 客户端断会话 |
| | 76 | 不要做的 |
| | 84 | 和护栏怎么接 |
| [encrypted-content-issues-2026.md](encrypted-content-issues-2026.md) | 1 | 近一个月 issue + 加固文案 |
| | 5 | 加固时间线 |
| | 16 | 近一个月 issue |
| | 33 | 分类器要认的字符串 |
| | 50 | 和 429 换站 |
| [monitor-api.md](monitor-api.md) | 1 | 监测接口 |
| | 5 | 渠道探测 |
| | 31 | 系统任务 |
| | 59 | 余额与上游模型 |
| | 78 | Uptime Kuma |
| | 94 | 运行时观测 `use_channel` |
| | 109 | 健康检查 |
| | 115 | 和 429 补丁的关系 |

## 源码行号（h3 / 官方共用处已标明）

| 文件 | 行 | 符号 |
|---|---|---|
| `controller/relay.go` | 72 | `Relay` |
| | 177 | 重试 for：`retry <= RetryTimes` |
| | 226 | `DecideRelayRetry` |
| | 230 | `action != retry` 则 break；**等待应加在此后、下一圈之前** |
| `service/relay_error.go` | 20 | `DecideRelayRetry` |
| | 40 | `attempt_budget_exhausted` |
| | 53 | `retry_status_matched`（含 429） |
| `relay/helper/stream_scanner.go` | 47 | `ResponsesStartupGuardActive` |
| `relay/channel/api_request.go` | 441 | `DoRequest` |
| | 471 | 护栏开则 **不** `SetEventStreamHeaders` |
| `relay/channel/openai/relay_responses.go` | 70 | `OaiResponsesStreamHandler` |
| | 78 | 调用 `guardResponsesStartupStream` |
| `relay/channel/openai/responses_startup_error.go` | 51 | `guardResponsesStartupStream` |
| | 247 | `classifyResponsesStartupError` |
| | 259 | TPM / `rate_limit_exceeded` → HTTP 429，`skipRetry=false` |
| `common/constants.go` | 143 | `var RetryTimes = 0` |
| `model/option.go` | 656 | 面板写入 `RetryTimes` |
| `web/.../retry-section.tsx` | 44 | 「最多重试次数」表单项 |
| `controller/channel.go` | 176 | `GET /api/channel/ops` 的 `retry_times` |
| `service/channel_select.go` | 388 | `AppendUsedChannel`（日志轨迹 `4->4`） |
| `setting/operation_setting/monitor_setting.go` | — | 定时探测默认 10 分钟、默认关 |
| `controller/uptime_kuma.go` | 1 起 | 状态页，不切流量 |

## 阅读顺序

1. [architecture.md](architecture.md) 总图  
2. [4-4-tpm-retry.md](4-4-tpm-retry.md) LT 实况与等待锁  
3. [compare-h3-vs-official.md](compare-h3-vs-official.md) / [hook-without-fork.md](hook-without-fork.md) 改源码 vs hook  
4. [encrypted-reasoning-failover.md](encrypted-reasoning-failover.md) 切站秒 400  
5. [monitor-api.md](monitor-api.md) / [trigger-and-gaps.md](trigger-and-gaps.md) 监测  

HEAD：`alterem/new-api-h3` `38e69e300`。这些 md 不进 git（`.git/info/exclude` 未排除 docs，提交前自行决定）。
