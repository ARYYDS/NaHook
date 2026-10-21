# 429 改造成功后：原渠道可用，切渠道秒报错

护栏把 200 SSE 的 `rate_limit_exceeded` 收成内部 429，并接入官方 `DecideRelayRetry`。原渠道再打能成功（LT `4->4`）。换到 **另一条中转站/另一把 Key** 立刻失败，主因不是 TPM，是 **思考链密文绑在原 organization 上**。

## 思考链加密是什么

Responses 推理模型（o3 / gpt-5.x / gpt-6-* / Codex）会产出 `type=reasoning` 的 item（id 形如 `rs_…`）。明文思维链不给客户端。ZDR / `store=false` 时走 **encrypted reasoning items**：

1. 请求带 `include=["reasoning.encrypted_content"]`（Codex、Agent 默认常开）。
2. 上游用该 **organization 的密钥** 做 AEAD，把思维链封进 `encrypted_content`。
3. 关联数据里带 `organization_id`（以及 item id）。
4. 下一轮客户端把 `rs_…` + `encrypted_content`（或 `previous_response_id`）原样塞回 `input`。
5. 上游在内存里解密，用完即丢，磁盘不落明文。

官方说明：[Encrypted Reasoning Items](https://developers.openai.com/cookbook/examples/responses_api/reasoning_items)。  
LiteLLM 事故：[跨 Key 负载均衡无法校验密文](https://docs.litellm.ai/blog/responses-api-encrypted-content-incident)。报错：

```text
invalid_encrypted_content
Encrypted content organization_id did not match the target organization.
```

Codex 账号池切号是同一类 400：`Encrypted content could not be decrypted or parsed.`

密文只能给 **签发它的 org/Key** 解。另一家中转站是另一个 org，校验立刻失败。

## 和 429 护栏怎么叠在一起

```text
请求已带 rs_* / encrypted_content / previous_response_id（上一轮在渠道 A 的 org 里生成）
  → 渠道 A TPM：200 SSE rate_limit_exceeded
  → 护栏 → 内部 429 → 官方换渠道 B
  → 同一份 body 打到 B（另一个 org）
  → B：400 invalid_encrypted_content / previous_response 不存在
  → classify：400 → skipRetry=true
  → 客户端立刻看到错误，不再重试
```

原渠道再打：同一 org，密文能解，TPM 窗口过了就 200。所以「原渠道正常、切渠道秒挂」。

首轮、body 里 **没有** 密文/`previous_response_id` 时，换渠道一般不会走这条 400。Agent / Codex / 多轮 tool 几乎总会带回 reasoning item，429 一换站就炸。

h3 对启动错误的分类：`invalid_request_error` / 400 为 **停止重试**。切过去的 400 不会再退回 A。

## 官方循环做不到的事

`DecideRelayRetry` 只看 HTTP 码。429 一律换渠道，**不看** body 里有没有 `encrypted_content`。NewAPI 也没有 LiteLLM 那种 `encrypted_content_affinity`。

LiteLLM 的修法：密文请求 **钉死原 deployment**，连 TPM 也绕过，因为换 Key 必 400。

## 处理方式

| 场景 | 做法 |
|---|---|
| 请求含 `encrypted_content` / `rs_` / `previous_response_id` | 429 **只重试同一渠道/同一 org 的 Key**，不要换中转站 |
| 干净首轮、无密文 | 可以换站（护栏 + 原生 retry） |
| 必须换站 | 从重试 body 里去掉密文和 `previous_response_id`（丢思维链，质量下降） |
| 观测 | 切站失败看 `invalid_encrypted_content`、`organization_id did not match`，不是又一次 TPM |

护栏解决的是「200 SSE 进不了 429 循环」。切站秒报错是 **429 循环不该把 org 绑定的密文打到另一家站**。

## 社区有没有人解密

有人试过，**没有**公开的离线破密钥工具。走的是「让同一家厂商的模型当解密预言机」，不是把 `gAAAA…` Fernet 包在本地解开。

| 来源 | 做法 | 结果 |
|---|---|---|
| Matthew Green，2026-05，[cryptographyengineering](https://blog.cryptographyengineering.com/category/ai) | 原样重放密文到别的 session / 账号 / 模型 | 能重放，说明当时是厂商级全局密钥；篡改会被拒 |
| Panfilov 等，arXiv [2608.09867](https://arxiv.org/html/2608.09867v1) *Stealing Reasoning Traces from Proprietary LLM APIs* | 把强模型密文喂给同厂弱模型（GPT 侧用 **GPT-5.6 Luna**），再让弱模型逐字转写 | 解码公开日志里 315,320 条；论文 **不发布密钥** |
| Johann Rehberger，[Embrace The Red](https://embracethered.com/blog/posts/2026/recovering-encrypted-llm-thoughts/) | 复现 Luna 转写 | 跨模型、跨账号当时可行 |
| OpenAI 2026-10 对 Moonshot 相关活动的说明 | 把密文拷到另一场对话里让模型「解密」 | 官方称没破加密，是滥用 API；随后堵了跨对话解密路径 |
| 网关社区 | 不解密 | 切 Key 就 **丢掉密文** 或 **钉死原 org**：[LiteLLM affinity](https://docs.litellm.ai/blog/responses-api-encrypted-content-incident)、[opencode-sanitize-reasoning](https://github.com/unurgunite/opencode-sanitize-reasoning)、[pydantic-ai #4608](https://github.com/pydantic/pydantic-ai/issues/4608) |

厂商打过补丁之后，校验里带上 `organization_id` / caller。跨中转站重放现在是 400 `invalid_encrypted_content`，不是解出明文。切渠道这条路上 **不能靠解密让 B 站读懂 A 站的密文**；只能同 org 重试，或剥掉 `encrypted_content` 再换站。
