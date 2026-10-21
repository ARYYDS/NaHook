# NaHook SPEC

| 目录 / 文件 | 内容 |
|---|---|
| `00-总体架构/` | LT 上 Caddy + nahook + NewAPI 的整体蓝图、分阶段上线、全局排期和同文件锁表 |
| `01-nahook/` | nahook 本体：design、interface、audit、tasks |
| `specaudit.md` | SPEC 阶段的问题、待拍板、决策溯源 |

## 路径前缀对照

SPEC 里引用的路径用下面这些前缀。行号都按表里的提交。

| 前缀 | 指向 | 在线地址 |
|---|---|---|
| `NEWAPI/` | 官方 `QuantumNous/new-api` `206e0fa5c600127a314a97f88592c067f784f4a6` | `https://github.com/QuantumNous/new-api/blob/206e0fa5c600127a314a97f88592c067f784f4a6/<路径>` |
| `H3/` | `alterem/new-api-h3` `38e69e3001606d8cdf7de6c4b872d8329b405c25`（公开仓库） | `https://github.com/alterem/new-api-h3/blob/38e69e3001606d8cdf7de6c4b872d8329b405c25/<路径>` |
| `渠道监控/` | 本仓库的 `docs/reference/渠道监控/`（知识库在 2026-10-20 的快照） | — |
| `自动调度/` | 本仓库的 `docs/reference/渠道监控/自动调度/` | — |

## 谁改什么

| 内容 | 谁来改 |
|---|---|
| `spec/` | 只有架构侧（开发机）。DSH 发现 SPEC 和实现冲突时停下来报告，不自己改 |
| `docs/reference/` | 只读快照，任何人都不改 |
| 代码（`cmd/`、`internal/`、`deploy/`） | DSH 在 Win11 的 `O:\NaHook` 上改 |
