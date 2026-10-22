# LT 实测（2026-10-20，全部是只读查看）

主机：`lt`（`「LT 站点域名」`）。方法：用 `docker logs` 看容器 `luna-task-master` 的日志，读 Caddyfile，调用不需要鉴权的接口。

## 1. 部署形态

| 项 | 值 |
|---|---|
| 入口 | 宿主机上的 Caddy 2.6.2（systemd）：`reverse_proxy localhost:3000`，`lb_policy least_conn`，`lb_try_duration 5s`，`health_uri /api/status`，**`flush_interval -1`**，`response_header_timeout 30m`，`dial_timeout 5s`，`header_up X-Real-IP {remote_host}`，`encode zstd gzip`，请求体上限 512MB |
| NewAPI | `ghcr.io/alterem/new-api-h3:latest`，`/api/status` 返回版本 `main-20261020-38e69e3`，端口 `0.0.0.0:3000` |
| 存储 | 外部 MySQL（`SQL_DSN` 是 MySQL 格式，我没读里面的连接信息）；同机有 Redis 容器 `redis_luna-task`，端口没对宿主机开放 |
| 挂载 | `/root/container/luna-task/data → /data`，`/root/container/luna-task/logs → /app/logs` |
| `GET /api/channel/ops` | 不带令牌返回 401，说明接口存在 |
| 当天用到的渠道 | 只有 `#4 gpt-openai-api21` |

注意：3000 端口对公网发布了。如果云厂商的安全组没挡，客户端可以绕过 Caddy，也就绕过了 hook。

## 2. 限流事件（当天全量）

| 指标 | 值 |
|---|---|
| `/v1/responses` 返回 200 | 133 |
| 返回给客户端 HTTP 429 | 4 |
| 渠道 #4 上游 429 事件 | 13，全部在 14:02（9 次）和 14:03（4 次） |
| 第二次尝试救回的请求 | 5（13 = 4×2 + 5×1） |
| 首发成功率 / 重试成功率 | 57/66 = 86% / 5/9 = 56% |
| 非流式出现限流 | 0 |
| 输出内容之后才限流 | 0（全部是首字"不适用"、0 token、$0） |
| 命中会话亲和规则 | 0（当天 147 行日志全是"测试"令牌） |

被救回的请求，其 `request_policy` 节选：

```text
attempt 1  channel_selected      13ms
attempt 1  429 rate_limit_exceeded  upstream  1288ms
attempt 1  retry retry_status_matched
attempt 2  channel_selected      1290ms     ← 和上一次只隔 2ms
attempt 2  success               3296ms  frt=3089ms
```

上游报错文案里给的等待提示只有 3–28ms：

```text
Limit 40000000, Used 39995307, Requested 19224 → try again in 21ms
21ms ≈ (19224 − (40000000 − 39995307)) / (40000000 / 60000ms)
```

这个提示是上游在约 1.3s 之前算出来的，拿到时就已经过期了，而且没有算进其他租户同时在消耗的量。所以冷却门的等待窗口用失败尝试本身的耗时作基准（约 1.3s），不用这个提示。

## 3. 用户 1 每分钟用量

| 时间 | 流式 | 请求数 | token/分钟 | 429 次数 |
|---|---|---|---|---|
| 13:48 | 否 | 66 | 1,259,679 | 0 |
| 13:49 | 是 | 2 | 32,408 | 0 |
| 14:02 | 是 | 31 | 591,669 | 9 |
| 14:03 | 是 | 31 | 591,666 | 4 |

由此得到两点：

- 用户 1 在出 429 时只占 org 上限 40M 的 1.5%，桶是被别的租户打满的。用户自己的 TPM 预测不了 429，所以[切流](03-switch-algorithm.md)的主触发用拦截事件。
- 这一分钟非流式 126 万 token 没出 429，下一阶段流式 59 万出了 429。原因可能是不同时段其他租户的负载不同，也可能是中转站对流式请求单独限流。现有数据区分不了，需要做对照压测。

## 4. 没能实测的项目

| 项目 | 原因 | 怎么补 |
|---|---|---|
| 列出其他渠道和启用的模型 | 需要管理员令牌 | 你建好 `channel:read` 令牌后，跑 [02](02-admin-api.md) 第 4 节的命令 |
| 原生探测 | 会产生上游费用，而且需要 `channel:operate` 令牌 | 同上 |
| 切换和摘除 | LT 上没有第二条同类型、能服务 gpt-6-astra 的渠道 | 先加一条备用渠道，配好别名（[04](04-alias-billing.md)） |
| Caddy 是否吞掉客户端断开 | 属于 hook v1 的测试项 | T-C1 到 T-C4 |
| 渠道表的完整配置 | 需要 MySQL 凭据，不应经手 | 通过管理接口获取 |
