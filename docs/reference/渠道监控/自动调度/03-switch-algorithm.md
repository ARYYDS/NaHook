# 切流算法：按用户切换、全局共享探测结果

**状态：知识库，不在 hook v1 的范围内。** v1 只做冷却门、抖动重试和代 429。本文是以后接上"切渠道"时的设计依据。

## 1. 要满足的需求

| 需求 | 落到哪里 |
|---|---|
| 通过管理接口拿到同类型渠道（例如 OpenAI，type=1） | [02](02-admin-api.md) 的列渠道接口：只留已启用、模型里有 M、配了别名的渠道 |
| 先看启用了哪些模型，再在启用的渠道上逐个测试报错的模型 | `models_enabled` 加上原生探测，一次探测一条 |
| 正常返回的打标记，TTL 暂定 1800s | 全局探测缓存 `probe[M]` |
| 新渠道故障率过高就进入下一轮，探测下一个渠道 | 按真实流量统计故障率，超阈值就摘除，然后重新探测 |
| 按用户区分；单个用户的流式 TPM 超阈值就探测并切换 | 每个用户一张路由表；触发条件见第 3 节（LT 数据表明光看 TPM 不够） |
| 探测结果全局共享，下一个用户复用，TTL 不变 | `probe[M]` 所有用户共用；同一模型同一时刻只跑一轮探测 |
| 挂在原生端点上 | Caddy 接管 `POST /v1/responses`、`/v1/chat/completions`，客户端 URL 不变 |
| 按用户拆分 | 用令牌指纹（API Key 的哈希）作为 key |
| 按渠道拆分 | 每条渠道一个别名，各自的健康统计 |

## 2. 状态

```text
全局（所有用户共享）
  probe[M] = [ candidate{
      channel_id, alias,                 # 别名见 04
      probe_ok, probe_latency, probed_at # 探测结果，TTL 1800s
      fail_ewma, lat_ewma, samples       # 来自真实流量的被动统计
      ejected_until, ejections           # 摘除，按 Envoy 方式退避
  } ]
  probing[M]                             # 同一模型同时只跑一轮探测，并发触发的请求等这一轮结果

按用户（令牌指纹 u）
  route[u, M] = { channel_id, alias, until }   # 滑动 TTL，默认 1800s
  hits[u, M]  = 60s 滑动窗口内的限流拦截次数
  tpm[u, M]   = 60s 滑动窗口内的 token 数（从流里的 usage 被动累加）
```

## 3. 触发条件

LT 在 2026-10-20 的数据（[07](07-lt-measurements.md)）：

- 14:02 出现 429 时，用户 1 的流式只有 59 万 token/分钟，约占 org 上限 40M 的 1.5%。
- 13:48 他非流式跑了 126 万 token/分钟，一次 429 都没有。
- org 是被别的租户打满的，**用户自己的 TPM 和出不出 429 没有对应关系。**

所以：

- **主触发**：60s 内 `hits[u, M] ≥ N`。
- **TPM 只用来决定先迁谁**：`tpm[u, M] ≥ T` 的重度用户，只要拦截到 1 次就切。目的是先把占用大的用户迁走。
- 只看 TPM 的触发方式（`tpm ≥ T` 就切）在 LT 上会大量误切，不采用。

## 4. 算法

```text
请求到达 (u, M)
  route[u,M] 存在、没过期、目标渠道没被摘除
       → 把 model 改写成目标渠道的别名；续期 until（滑动）
  否则 → 原样交给 NewAPI，走它自己的选路（原渠道 A）

出现代 429 事件 (u, M, 当前渠道 A)
  hits[u,M] += 1
  满足触发条件（第 3 节）
     c := pick(M)
     c 为空 → 保持 hook v1 的逻辑：冷却门 + 同站重试 / 代 429（安全网）
     否则   → route[u,M] = c
              本次重试改写成 c 的别名，并先断开思考链（05）

pick(M)
  候选 := probe[M] 中满足：探测通过、距探测不到 1800s、没被摘除、不是 A
  候选为空 → probeRound(M)（同一模型只跑一轮；并发触发的请求等这一轮）
             跑完仍为空 → 返回空
  P2C：随机抽两个候选，取 score = fail_ewma × w1 + lat_ewma × w2 更小的那个
       → 避免所有被迁走的用户都挤到第一个候选上（sub2api #6992）

probeRound(M)
  渠道列表 := GET /api/channel/?type=<A 的类型>&status=1
  再按条件筛选：models 包含 M、分组覆盖到会用 M 的用户、hook 配置里有这条渠道的别名
  逐条探测：GET /api/channel/test/:id?model=M&endpoint_type=openai-response&stream=true
           一次只测一条，有总时限
  通过的：probe_ok=true，probed_at=now

每个经过 hook 的真实请求结束时（渠道 c）
  更新 c 的 fail_ewma、lat_ewma、samples
  成功时把 probed_at 顺带续期（被动健康），省去不必要的重探
  若 fail_ewma > F 且 samples ≥ S：
     摘除 c：ejected_until = now + base × 2^ejections（有上限）；ejections += 1
     同时摘除的渠道不超过候选的一半（Envoy max_ejection_percent）
     probe[M] 标记为待重探 → 下一次 pick 会去探测下一个渠道
     route 指向 c 的用户：下一个请求立刻重新 pick，并断开思考链
                          （不等 TTL 到期，避开 vm2api #163 的坑）
  若 c 持续健康：ejections 慢慢衰减
```

## 5. 不变量

1. 探测结果全局共享；用户的路由各自独立。
2. 同一模型同一时刻只跑一轮探测。
3. 不会把所有候选都摘掉。全部不可用时留在原渠道，继续走 v1 的同站重试，并照常给 NewAPI 打流量（LiteLLM 的 safety net 也是这么做的）。
4. 路由跟健康状态挂钩：目标被摘除，路由立即失效。
5. 每次换渠道（去新渠道、回原渠道），都要先断开思考链。
6. 所有阈值、TTL、别名表都能用 `nahookctl` 热修改，和 v1 的控制口是同一个。

## 6. 参数（暂定，待实测校准）

| 参数 | 暂定值 | 依据 |
|---|---|---|
| 探测 TTL | 1800s | 需求给定 |
| 路由 TTL | 1800s，滑动续期 | 需求给定；滑动是为了活跃会话不在对话中途被切回 |
| N（60s 内拦截次数） | 2 | LT 有过 1 分钟 9 次拦截；N=2 表示同一用户第二次被拦截才切 |
| T（重度用户 TPM） | 待定 | 需要多用户数据；LT 当天只有用户 1 |
| F / S（摘除阈值） | 故障率 0.3 / 至少 20 个样本 | 参考 LiteLLM 的 allowed_fails 和 Envoy 的连续失败 |
| 摘除基础时长 / 上限 | 30s / 10min | 参考 Envoy 的 base_ejection_time 和 max_ejection_time |

## 7. 和 v1 的关系

```text
v1：代 429 → 冷却门 + 抖动 → 同站重试，次数 ≤ retry.max → 代 429 返回
v2：代 429 → 满足触发条件且 pick 到候选 → 改写成别名 + 断开思考链 → 新渠道
            否则 → 回到 v1 的路径
```

v2 只在 v1 的代 429 事件上加一个分支，不改 v1 的冷却门和重试上限的语义。
