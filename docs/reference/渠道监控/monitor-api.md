# 监测接口

鉴权：标了 Admin/Root 的接口走管理端登录或具备对应权限的 Access Token。Uptime 与性能摘要给控制台状态页用。

## 1. 渠道探测

| 方法 | 路径 | 权限 | 作用 |
|---|---|---|---|
| GET | `/api/channel/test/:id` | ChannelOperate | 立即测一条渠道。Query：`model`、`endpoint_type`、`stream` |
| GET | `/api/channel/test` | ChannelOperate | 入队「测全部」系统任务（mode=`scheduled_all`，完成后通知）。已有同类型任务则 **409** |
| GET | `/api/channel/ops` | ChannelRead | 当前 `retry_times` 与自动禁用是否生效 |
| POST | `/api/channel/:id/status` | ChannelOperate | 启用/禁用单渠道 |
| POST | `/api/channel/status/batch` | ChannelOperate | 批量改状态 |
| POST | `/api/channel/tag/disabled` | ChannelOperate | 按标签禁用 |
| POST | `/api/channel/tag/enabled` | ChannelOperate | 按标签启用 |

单渠道测试成功响应：

```json
{ "success": true, "message": "", "time": 1.23 }
```

失败仍 HTTP 200，body 里 `success: false`，带 `error_code` 与耗时。这是探测接口约定，和运行时把 429 回给终端用户不是同一条路径。

测全部入队成功：

```json
{ "success": true, "data": { "task_id": "...", "status": "..." } }
```

## 2. 系统任务（定时探测的执行面）

Root。定时渠道测试、上游模型检测、MJ/异步任务轮询都走这里。

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/system-task/list` | 历史任务 |
| GET | `/api/system-task/current` | 当前正在跑的任务 |
| GET | `/api/system-task/:task_id` | 单条（含 `channel_test` 的 tested/succeeded/failed/disabled/enabled） |
| DELETE | `/api/system-task/history` | 清历史 |
| GET | `/api/system-info/instances` | 多实例租约 |

`channel_test` 的 payload：

```json
{ "mode": "scheduled_all|auto_ban_only|passive_recovery", "notify": true }
```

空 payload = 定时跑，用控制台 `monitor_setting.channel_test_mode`，不通知。

设置项（请求策略 / 选项）：

- `monitor_setting.auto_test_channel_enabled`
- `monitor_setting.auto_test_channel_minutes`
- `monitor_setting.channel_test_mode`
- `monitor_setting.channel_test_concurrency`
- 环境变量 `CHANNEL_TEST_ENABLED`、`CHANNEL_TEST_FREQUENCY`

## 3. 余额与上游模型

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/channel/update_balance` | 刷新全部渠道余额 |
| GET | `/api/channel/update_balance/:id` | 刷新一条 |
| GET | `/api/channel/:id/vllm/status` | vLLM 运行状态 |
| GET | `/api/channel/:id/sglang/status` | SGLang 运行状态 |
| GET | `/api/channel/:id/codex/usage` | Codex 用量 |
| POST | `/api/channel/upstream_updates/detect` | 检测单渠道上游模型变更 |
| POST | `/api/channel/upstream_updates/detect_all` | 检测全部 |
| POST | `/api/channel/upstream_updates/apply` | 应用变更 |
| POST | `/api/channel/upstream_updates/apply_all` | 应用全部 |

上游模型检测另有定时任务，环境变量：

- `CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED`（默认 true）
- `CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_INTERVAL_MINUTES`

## 4. 控制台状态页（Uptime Kuma）

| 方法 | 路径 | 鉴权 | 作用 |
|---|---|---|---|
| GET | `/api/uptime/status` | 公开（随 `/api/status` 模块） | 聚合各 Kuma 公共状态页 |
| GET | `/api/status` | 公开 | 含 `uptime_kuma_enabled` |

实现：`controller/uptime_kuma.go` 对每组配置并发请求：

- `{url}/api/status-page/{slug}`
- `{url}/api/status-page/heartbeat/{slug}`

返回每个 monitor 的 `name`、`uptime`（24h 键 `id_24`）、`status`（最近一次心跳）。**只展示，不改变选路。**

选项：`console_setting.uptime_kuma_enabled`、`console_setting.uptime_kuma_groups`（JSON 数组，字段 `url` / `slug` / `categoryName`）。

## 5. 运行时观测（是否切到了别的渠道）

消费日志 `admin_info.use_channel`：`["12","34"]` 表示本请求打过渠道 12 再打 34。服务端日志有 `retry: 12->34`。

请求策略决策写在日志详情里，429 换渠道时 reason 为 `retry_status_matched`。预算用尽为 `attempt_budget_exhausted`。绑定渠道为 `pinned_channel`。亲和 strict 为 `strict_session`。

性能：

| 方法 | 路径 | Query | 作用 |
|---|---|---|---|
| GET | `/api/perf-metrics/summary` | `hours`、`recent_model` | 分组汇总 |
| GET | `/api/perf-metrics` | **必填** `model`，可选 `group`、`hours` | 单模型耗时/成功率 |

429 在性能口径里记为失败（`rate_limit_exceeded`）。切渠道成功的那一次会记在最终成功的渠道上。

## 6. 健康检查

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/status/test` | Admin，进程自检 |

## 7. 和 429 补丁的关系

监测接口 **看不到**「200 SSE 里的限流」是否被护栏拦住。要确认补丁在干活：

1. 进程环境 `RESPONSES_STREAM_STARTUP_GUARD_ENABLED=true`
2. 客户端对 `/v1/responses` `stream=true` 在限流时仍可能最终 200（换到备份站成功）或最终 429 JSON（所有站都限流）
3. 日志 `use_channel` 长度 > 1
4. 客户端 **不应** 在第一次尝试就收到 `event: error` + `rate_limit_exceeded` 然后结束；护栏打开时这一帧不会出站

护栏关闭时：同一请求会 HTTP 200、SSE 里直接出现 `rate_limit_exceeded`，`use_channel` 只有一个 ID。这就是官方最新的行为。
