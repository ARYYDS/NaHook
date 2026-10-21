# 换站时如何把思考链断干净

社区共识：跨 org / 跨中转站 **不要解密**，把 org 绑定状态从请求里拿掉再打。剥不干净会立刻再 400。

## 必须一起清掉的字段

只删 `encrypted_content`、留下 `id: rs_…` / `previous_response_id` / 成对的 `function_call`，新站仍会拒。

| 字段 | 不剥会怎样 |
|---|---|
| `input[].encrypted_content`（reasoning / compaction `cmp_`） | `invalid_encrypted_content` / org 不匹配 |
| `input[].id` 为 `rs_*` 且无可用密文 | `Item rs_… not found` 或 `item_id did not match` |
| `previous_response_id` | 新 org 没有这条 `resp_` |
| 整条 `type=reasoning`（含只有 summary、没有密文字段的陈旧项） | sub2api #3138：没密文字段也会 400 |
| 失去 reasoning 父项后的 `function_call` / `function_call_output` | `function_call without required reasoning item` |
| 引用 `rs_*` 的 `item_reference` | 同上 |
| 密文还在时非空 `content[]` | `array_above_max_length`（有密文则 content 必须空） |
| compaction 密文 | 和 reasoning 同类，每轮都打进来 |

`include: ["reasoning.encrypted_content"]` 可以留：那是让 **新站签发新密文**，不是回放旧密文。旧密文在 `input` 里。

## 社区解法清单（近期）

### 1. 先钉死，尽量不断

- LiteLLM `encrypted_content_affinity`：带密文就钉原 deployment，连 TPM 也绕过  
  https://docs.litellm.ai/blog/responses-api-encrypted-content-incident
- CLIProxyAPI：有 `previous_response_id` / `encrypted_content` 时会话粘同一把 auth  
  https://github.com/router-for-me/CLIProxyAPI/issues/1793

适合：还能打回原渠道。429 窗口短（如 28ms）时优先这条。

### 2. 主动剥（换站前改 body）

| 力度 | 做法 | 来源 |
|---|---|---|
| 最轻 | 只删 `encrypted_content`，保留 summary | OpenClaw Copilot [#95493](https://github.com/openclaw/openclaw/pull/95493) |
| 常用 | 删密文 + 剥 `rs_*` id，保留 reasoning 壳 / summary | [sub2api #3588 同类](https://github.com/Wei-Shaw/sub2api/pull/3588)（store=false 时 id 会 404） |
| 更干净 | 整条 `type=reasoning` 从 `input` 拿掉 | OpenClaw `dropReasoningFromHistory` [#92857](https://github.com/openclaw/openclaw/pull/92857)；sub2api [#3139](https://github.com/Wei-Shaw/sub2api/pull/3139) |
| 网关默认关签发 | 配置里关掉 `include encrypted_content`，之后不再产生密文 | [pydantic-ai #4608](https://github.com/pydantic/pydantic-ai/issues/4608) |
| 按年龄剥 | blob 超过 TTL 或进程重启后丢掉 | [opencode-sanitize-reasoning](https://github.com/unurgunite/opencode-sanitize-reasoning)（默认约 3 分钟） |
| 换模型/换路由 | 检测 model snapshot 变化，`dropReplayableReasoning` | OpenClaw [#70654](https://github.com/openclaw/openclaw/issues/70654) |
| Anthropic 对照 | 跨账号重试清 thinking block | sub2api `FilterThinkingBlocksForRetry`（#658 期望行为） |

换站时建议一次做完：

1. 删 `previous_response_id`
2. 从 `input` 去掉所有 `type=reasoning`（不要只清有密文字段的）
3. 去掉 `cmp_*` compaction 密文项
4. 去掉指向 `rs_*` 的 `item_reference`
5. 若因此出现孤立 `function_call`：整组 tool 回合一起丢，或留下 call+output、不要留「半截 reasoning」
6. `store=false`（新站不要去查旧 `resp_`）

### 3. 被动剥（先打、400 再清、只重试一次）

社区最稳的换站姿势：

1. 原样打新渠道
2. 认出 `invalid_encrypted_content` / `could not be verified` / `was not issued to this caller`
3. 按上面清单改 body，**同一条新渠道再打一次**
4. 本会话关掉密文回放，避免毒 body 循环

实现：

- sub2api [#999](https://github.com/Wei-Shaw/sub2api/pull/999) `trimOpenAIEncryptedReasoningItems`，HTTP + WS
- sub2api [#3139](https://github.com/Wei-Shaw/sub2api/pull/3139)：没有 `encrypted_content` 字段的陈旧 reasoning 也要丢
- CLIProxyAPI [#1796](https://github.com/router-for-me/CLIProxyAPI/issues/1793) / [#3537](https://github.com/router-for-me/CLIProxyAPI/pull/3537)：剥 reasoning + 嵌套密文，流式在首个 assistant 内容前还能救
- hermes-agent：整段会话禁用 replay，从 history 弹出 `codex_reasoning_items`

h3 把 400 标成 `skipRetry`。要「断干净再换站」，得在进官方循环前改 body，或把 `invalid_encrypted_content` 从 skip 里拿出来、剥完再 retry。

### 4. 客户端断会话

rewind / `/clear` / 新开 thread。opencode 文档：回到没有毒 blob 的那一步就恢复。适合会话已经每轮都 400。

## 不要做的

- 只删密文、保留 `rs_*` 且 `store=false` → 404
- 改写带密文的 item id → `item_id did not match`
- 密文还在时带 `reasoning_text` content → `array_above_max_length`
- 只丢 reasoning、留下 function_call
- 把 400 当普通失败继续换下一站、body 原样 → 每条新渠道都秒 400

## 和 NewAPI 429 护栏怎么接

```text
429（护栏）→ 原生换渠道
  若 body 含密文 / previous_response_id / rs_*
    → 换站前按清单剥干净
    → 或 400 后剥一次再打同一条新渠道
  无密文的干净首轮
    → 直接换站即可
```
