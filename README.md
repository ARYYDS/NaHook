# NaHook

NaHook 是一个独立的常驻进程，串在 Caddy 和 NewAPI（官方镜像）之间，只处理一件事：流式 `/v1/responses`、`/v1/chat/completions` 在启动阶段被上游限流时，NewAPI 会把错误当作 200 SSE 直接透传给客户端。NaHook 在客户端收到任何字节之前识别出这种限流，用冷却门加全抖动等待一段时间后同站重试；重试用完，返回一个和 NewAPI 格式一致的 HTTP 429。重试上限可以用 `nahookctl` 一行命令热修改。

## 目录

| 目录 | 内容 | 谁维护 |
|---|---|---|
| `spec/` | 工程 SPEC，以这里为准。入口是 `spec/README.md` | 架构侧（开发机） |
| `docs/任务书-DSH.md`、`docs/开工提示词.md` | Win11 上 DSH 的任务书和开工提示词 | 架构侧 |
| `docs/runbook/` | LT 上的操作方案（例如 T0） | 架构侧 |
| `docs/reference/` | 知识库快照，只读 | 不改 |
| `ops/` | LT 上实验和运维用的脚本 | 架构侧 |
| `cmd/`、`internal/`、`deploy/` | 代码、systemd 单元、配置样例 | Win11 上的 DSH |

## 规则

- 只用 Go 标准库。
- 二进制不进仓库。产物放在 Win11 的 `O:\WinCache\nahook\dist\<8位提交号>\`。
- LT 上的每一次替换、安装、重载，都要先请示用户。
