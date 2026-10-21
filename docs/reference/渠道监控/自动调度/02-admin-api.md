# 管理接口（"x-api"）

用个人访问令牌（PAT，前缀 `nap_`）调用，请求头 `Authorization: Bearer nap_…`。hook 直连 `127.0.0.1:3000`，不经过 Caddy。

行号按官方 `206e0fa5c`，路径相对 `NEWAPI/`。

## 1. 接口清单

| 用途 | 接口 | 令牌权限 | 位置 / 说明 |
|---|---|---|---|
| 列出渠道 | `GET /api/channel/?type=1&status=1&p=1&page_size=100` | `channel:read` | `router/channel-router.go:60`。返回 `data.items[]`，含 id、name、type、status、priority、group、models。**不返回密钥**（`controller/channel.go:250` 的 `Omit("key")`） |
| 已启用的模型 | `GET /api/channel/models_enabled` | `channel:read` | `router/channel-router.go:64` |
| 重试次数 | `GET /api/channel/ops` → `data.retry_times` | `channel:read` | `router/channel-router.go:65`、`controller/channel.go:176`。读的是请求策略快照，面板保存后立即生效 |
| 原生探测 | `GET /api/channel/test/:id?model=M&endpoint_type=openai-response&stream=true` | `channel:operate` | `router/channel-router.go:68`；参数在 `controller/channel-test.go:865-867` |
| 整个请求策略 | `GET /api/option/request_policy` | `option:read`（只能 root） | 权限太宽：同一个 scope 还能读 `GET /api/option/` 下几乎所有配置，不推荐 |

鉴权流程：

1. `AdminAuth`：令牌所有者必须是管理员或 root。
2. 校验令牌的 scope。
3. Casbin 的 `RequirePermission`。

令牌在 个人设置 → 安全与访问 → 访问令牌 里创建：

- 必须在浏览器里登录，并通过安全验证；
- 可以设成永不过期，界面会给出警告；
- 每个用户最多 20 个。

## 2. 副作用（决定能调多频繁）

| 动作 | 副作用 | 位置 |
|---|---|---|
| 任何一次用 PAT 调用 | 写一条访问令牌审计日志，更新令牌的最后使用时间，并计入全局 API 限流 | `middleware/audit.go:307` |
| 探测 | 真实调用上游，会产生费用 | — |
| 探测 | 以令牌所有者的名义记一行 "Model test" 消费日志 | `controller/channel-test.go:511` |
| 探测 | 更新渠道的响应时间 | `controller/channel-test.go:893` |
| 探测失败 | 按自动禁用规则处理，默认只有 401 会禁用渠道 | — |
| 探测 | 流式响应里的错误帧也能识别出来 | `controller/channel-test.go:659` |

## 3. 探测测不出 TPM 余量

探测发的是一个很小的请求，而 TPM 型限流取决于 org 桶还剩多少。LT 实测（[07](07-lt-measurements.md)）：`Requested 19224`，桶里只剩 4693，于是被拒；换成一个小请求，在桶快满时照样能通过。

所以探测只能说明三件事：渠道是通的、密钥是对的、模型存在。它**说明不了还剩多少 TPM**。余量只能从真实流量的结果里看出来，对应 [03](03-switch-algorithm.md) 里"故障率过高就摘除"那一环。

## 4. 命令样例

要先用你自己的令牌，勾选 `channel:read` 和 `channel:operate`。

```bash
curl -s -H "Authorization: Bearer $NAP" "http://127.0.0.1:3000/api/channel/?type=1&status=1&p=1&page_size=100" | jq '.data.items[] | {id,name,status,priority,group,models}'
```

```bash
curl -s -H "Authorization: Bearer $NAP" "http://127.0.0.1:3000/api/channel/models_enabled" | jq '.data'
```

```bash
curl -s -H "Authorization: Bearer $NAP" "http://127.0.0.1:3000/api/channel/test/4?model=gpt-6-astra&endpoint_type=openai-response&stream=true"
```

```bash
curl -s -H "Authorization: Bearer $NAP" "http://127.0.0.1:3000/api/channel/ops" | jq '.data.retry_times'
```
