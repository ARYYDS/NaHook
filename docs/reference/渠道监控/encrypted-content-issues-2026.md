# 近一个月 issue + OpenAI 加固后的新文案

窗口：约 2026-09-13 ～ 2026-10-20。加固不是一次发布，文案分几代。

## 加固时间线（文案一代比一代紧）

| 大约时间 | 校验绑什么 | 典型 Reason |
|---|---|---|
| 2025-12 | 服务端事故，旧会话坏 | `could not be verified`（无 org 细节） |
| 2026-03 | **organization_id** | `Encrypted content organization_id did not match the target organization` [Codex #13724](https://github.com/openai/codex/issues/13724) |
| 2026-08 | 论文后堵跨模型/跨账号预言机；compaction 也加密 | `could not be decrypted or parsed`；`cmp_*` [Codex #36704](https://github.com/openai/codex/issues/36704) |
| **2026-09** | **caller / 续写身份 / TTL / item_id** | 见下表，这才是近一个月主力 |

2026-09 起同一把 Key、同一模型也会挂：blob 过期、compaction 另一条 identity、网关改了 `rs_*` 后缀。

## 近一个月（9 月中旬以后）

| 日期 | 仓库 | 文案 | 场景 |
|---|---|---|---|
| 09-13 | [oh-my-pi #11928](https://github.com/can1357/oh-my-pi/issues/11928) | `was not issued to this caller` | Muse Spark 经 Zen `/v1/responses`，tool 续写回放密文 |
| 09-13 | [opencode #48773](https://github.com/anomalyco/opencode/issues/48773) [#48805](https://github.com/anomalyco/opencode/issues/48805) | 同上 | 同会话换模型；新会话正常 |
| 09-14 | [opencode #48973](https://github.com/anomalyco/opencode/issues/48973) | 同上；`isRetryable False` | 长会话突然毒死，短会话还活 |
| 09-14 | [opencode #48942](https://github.com/anomalyco/opencode/issues/48942) | Azure：`Conflicting authenticated continuation identities` | compaction 那一轮的密文混进主链 |
| 09-14 | [opencode #48908](https://github.com/anomalyco/opencode/pull/48908) | 分类这三条再剥一次：`not issued to this caller` / `invalid_encrypted_content` / `Referenced reasoning item '…' was not found or has expired` | 官方 TTL/过期已进错误码 |
| 09-20 | Codex #36704 跟帖 | `cmp_*` 密文 | 旧线程永久砖 |
| 09-22 | [codex-proxy #7](https://github.com/thezillo/codex-proxy/pull/7) | 400 后剥密文再打 | 账号池 failover |
| 09-23～24 | [CLIProxyAPI #6108](https://github.com/router-for-me/CLIProxyAPI/issues/6108) | `item_id did not match` | **无改配置从 09-22 成功变成 09-23 失败**；带 tool 的续会话；粘性只能降频 |
| 09-28 | [paseo #4542](https://github.com/getpaseo/paseo/issues/4542) | `could not be decrypted or parsed` | rewind/fork 丢了 `model_provider`，密文打到另一 org |
| 10-03 | [CodexPlusPlus #1766](https://github.com/BigPizzaV3/CodexPlusPlus/issues/1766) | `item_id did not match` | 原会话还能聊（增量）；**派生/fork 必挂**（整段历史重放） |

配套修复（同期）：opencode #48908 剥 `reasoningEncryptedContent`+`itemId` 重试一次；oh-my-pi 对 Muse 关掉 `include-encrypted-reasoning`。

## 加固后分类器要认的字符串

剥完再换站时，400 不只 `invalid_encrypted_content`：

```text
invalid_encrypted_content
organization_id did not match the target organization
was not issued to this caller
Referenced reasoning item '…' was not found or has expired
Encrypted content item_id did not match the target item id
Conflicting authenticated continuation identities
Could not decrypt the provided encrypted_content   (xAI 无 code)
could not be decrypted or parsed
```

OpenClaw 已踩过：只匹配 `invalid_encrypted_content` 时，xAI / Azure 文案救不起来。

## 和 429 换站

近一个月的 issue 说明：加固后密文绑 **caller + item_id + 续写身份 + 过期**，不只绑 org。

- 同渠道秒重试（LT `4->4`）仍可能过
- 换中转站 / 换 Key / compaction 混 identity / 网关改 `rs_*` → 秒 400
- CLIProxyAPI #6108：09-23 起 **item_id 绑定变严**，粘性会话也挡不住 tool 续写

换站前按 `strip-encrypted-content.md` 清 `encrypted_content`、`rs_*`、`previous_response_id`、`cmp_*`，并把上表文案从 h3 的 400-skipRetry 里拿出来做「剥一次再打」。
