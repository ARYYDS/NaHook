# 换渠道时断开思考链

字段清单和社区做法的完整版本见上一级目录的 [strip-encrypted-content.md](../strip-encrypted-content.md) 和 [encrypted-content-issues-2026.md](../encrypted-content-issues-2026.md)。本文只记录与自动调度相关的部分。

## 1. 什么时候要断开

| 时机 | 原因 |
|---|---|
| 第一次切到新渠道 | 请求体里的 `rs_*`、`encrypted_content`、`previous_response_id` 都是原 org 签发的，新渠道会返回 400 |
| 路由过期、回到原渠道 | 这时请求体里带的是新渠道签发的思考链，原渠道同样会 400 |
| 目标被摘除、重新 pick 到别的渠道 | 同上 |
| 已经剥过了，新渠道仍然返回 `invalid_encrypted_content` | 被动再剥一次，只重发这一次 |

在同一条渠道上重试（v1 的冷却门路径）不需要断开：同一个 org，密文能正常解开。

## 2. 一次做完的步骤

1. 删掉 `previous_response_id`。
2. 从 `input` 里去掉所有 `type=reasoning` 的项，不要只清掉带密文字段的那些。
3. 去掉 `cmp_*` 压缩密文项。
4. 去掉指向 `rs_*` 的 `item_reference`。
5. 因此变成孤立的 `function_call` / `function_call_output`，整组一起丢掉。
6. 设 `store=false`。

`include: ["reasoning.encrypted_content"]` 保留：它的作用是让新渠道签发新密文，不是回放旧密文。

## 3. 断开之后要识别的报错

`invalid_encrypted_content`、`could not be verified`、`was not issued to this caller`、`organization_id did not match`、`Item rs_… not found`、`function_call without required reasoning item`。

出现这些说明剥漏了，按第 1 节最后一行被动再剥一次。

## 4. 代价

断开后会丢掉之前的思考链，新渠道上的第一轮质量会下降。所以路由 TTL 用滑动续期：活跃的会话尽量不在对话中途来回切。
