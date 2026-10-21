# 01-nahook tasks：排期

## 0. 基础前置

| 前置项 | 状态 | 缺了它能做到哪一步 |
|---|---|---|
| 仓库 `ARYYDS/NaHook`（公开），在 Win11 的 `O:\NaHook` 上开发和构建（design §7） | 已定；还需要用户在 Win11 上重新执行 `gh auth login`（见 `docs/任务书-DSH.md` §1.2） | 能克隆，但推送不了，也发不了 Release |
| 产物通过 GitHub Release 分发（design §7） | 已定 | — |
| L-GO 车道由 Win11 上的 DSH 执行；任务书和开工提示词见 `docs/` | 已定 | — |
| LT 上的每一次替换或安装 | 都要先请示用户 | 请示之前停在那一步 |
| 用户在测试 NewAPI 上完成初始化：root 账号、自用模式、测试渠道、测试令牌（00 §6.3） | 待用户操作 | 能做到 T9 起容器；T10 跑不了 |
| K1（Caddy 是否吞掉客户端断开） | 由 T0 实测 | 不影响写代码；影响 T11 的生产路由写法 |
| K3（官方镜像 digest） | 部署当天确定 | 影响 T9 和 T13 |
| 每个生产阶段都要用户明确同意 | — | T11、T12、T13 前都要停下来等 |

## 1. 阶段与车道

```text
          阶段 A：风险验证     阶段 B：开发                       阶段 C：测试环境     阶段 D：生产
L-OPS  │ T0 Caddy 断开实验 │                                  │ T9 搭测试环境      │ T11 → T12 → T13
       │                  │                                  │ T10 跑审计清单      │
L-GO   │                  │ T1 → T2 → T3 → T4 → T7 → T5 → T6 → T8 │                  │
                            （T5 开工前要等 00 G1，也就是 T0 的结论）
                            （Win11 上的 DSH 串行执行；每个任务交付后，由开发机另派审计）
L-DOC                                                                               │ T14 运维手册
```

- L-OPS 只改 LT 上的配置和 Caddyfile，不碰 Go 代码；L-GO 只改 nahook 仓库。两条车道可以并行。
- T7 只依赖 T1，排在 T5 前面，用来填 T0 结论出来之前的空档；T5 要等 G1，T6 依赖 T5。
- T9 依赖 T8 的产物；T11 依赖 T10 全部通过。

## 2. 任务

- [ ] **T0 Caddy 2.6.2 断开传播实验（开工第一步，风险验证）**（L-OPS）
  - 前置：无
  - 改动：完整方案见 `docs/runbook/T0-caddy-disconnect.md`。在 LT 的 Caddyfile 里临时加一个只监听本机回环的站点 `http://127.0.0.1:19444`，下面四个路径分别对应"不回响应头 / 已开始流式输出"和"写 / 不写 `flush_interval -1`"的组合。后端是 `ops/t0/t0_backend.py`，监听 19082 和 19083。执行前要请示用户
  - 依据：audit K1、T-C1、T-C2
  - 完成判据：
    - [ ] 四种组合各跑 3 次，`backend.log` 里都有每一次的 `peer_closed`、`timeout_still_open`、`write_failed` 或 `completed`
    - [ ] 按 runbook §6 的判定表得出结论，写进 specaudit §3 的 Q1，并回写到 00 §6.1 和 audit K1
    - [ ] 临时站点已删除，Caddyfile 的 sha256 和实验前一致，生产站点健康检查返回 200
  - 冲突：`/etc/caddy/Caddyfile`（见 §3）

- [ ] **T1 仓库骨架和 config 包**（L-GO）
  - 前置：用户已在 Win11 上完成 `gh auth login`，并把仓库克隆到 `O:\NaHook`
  - 改动：`go.mod`（`go 1.26`，不加 require）；`internal/config`：`Config`、`Load`、`Validate`、`Persist`、`Store`（`Current`、`RetryMax`、`SetRetryMax`、`UnsetRetryMax`、`Reload`）
  - 依据：design §1.7、§5、§6；interface 2.4 ｜ audit T-F3、T-F8
  - 完成判据：
    - [ ] 表驱动测试覆盖：全局值和模型级配置的查找、`n<0` 被拒、写盘失败时快照不变、Reload 失败时保留旧快照
    - [ ] `go test -race` 通过
  - 冲突：无

- [ ] **T2 startup 包（移植 h3 的启动帧读取和判定）**（L-GO）
  - 前置：T1
  - 改动：`internal/startup`：`Read`、`inspect`、`upstreamError`、`isStartupEvent`、`IsRateLimit`、`containsAny`、常量
  - 依据：design §1.3；interface 2.1 ｜ audit T-F12、T-F13、T-F14、T-F15
  - 完成判据：
    - [ ] 固定样例覆盖：LT 原文的 error 帧、只有 response.failed、Chat 的 error chunk、`: PING`、超过 256KiB、超过 128 个事件、坏 JSON、`[DONE]`、开头就 EOF、`insufficient_quota`
    - [ ] 每个样例都满足 `Prefix + Rest == 原始字节`
    - [ ] 同一组输入下，`IsRateLimit` 和 h3 `classifyResponsesStartupError` 的 429 分支结果一致（`insufficient_quota` 除外）
  - 冲突：无

- [ ] **T3 gate 包**（L-GO）
  - 前置：T1
  - 改动：`internal/gate`：`Gate`、`Registry`、`Key`、`ResetModel`、`Snapshot`
  - 依据：design §1.5、§6.3；interface 2.3 ｜ audit T-F4
  - 完成判据：
    - [ ] 并发调用 `OnRateLimit` 时 `until` 只增不减
    - [ ] 1000 个并发的 `Wait` 都不超过 `until`，到达时间的分布没有和序号线性相关
    - [ ] ctx 取消后 `Wait` 立即返回
    - [ ] `Key` 能从 LT 原文里取出 org
  - 冲突：无

- [ ] **T4 proxy429 包**（L-GO）
  - 前置：T2
  - 改动：`internal/proxy429.Write`
  - 依据：design §1.4；interface 2.2 ｜ audit T-F1
  - 完成判据：
    - [ ] 黄金样例：`code` 分别是字符串、数字和 null；`param` 分别是空串和字符串（两边的类型都是 string，线上不会出现 null）；字段顺序正确，末尾没有换行，HTML 字符按 encoding/json 默认规则转义
    - [ ] 响应头只按白名单从上一次尝试复制（design §1.4），SSE 专用的头不会被带过来
  - 冲突：无

- [ ] **T5 obs 包和 proxy 包（分类、转发、放行、透传、拦截主循环）**（L-GO）
  - 前置：T2、T3、T4；00 G1（T0 的结论。需要单请求总时限时，先改 design §4 和 §1.6，再开工）
  - 改动：
    - `internal/obs`：`NewLogger`、`Counter` 及其常量、`NewStats`、`(*Stats).Inc`、`(*Stats).Snapshot`
    - `internal/proxy`：`NewHandler`、`ServeHTTP`、`classify`、`forward`、`serveIntercept`、`release`、`passthrough`、`copyFlush`、`writeBadGateway`
  - 依据：design §1.2、§1.6、§1.8、§5（日志）、§6.1、§6.7；interface 2.5、2.7 ｜ audit T-F2、T-F5、T-F6、T-F7、T-F10、T-F16
  - 完成判据：
    - [ ] 用 httptest 起一个假 NewAPI，覆盖：重试到上限后返回代 429、重试中途成功、HTTP 429 透传、非流式透传、超大请求体透传、ctx 取消
    - [ ] 断言：判定之前对 `w` 没有任何写入
    - [ ] 断言：逐跳头被去掉，XFF 和 X-Real-IP 原样转发；NewAPI 收到的 Host 等于客户端请求的 Host；每次重试的请求体和原始请求体逐字节一致
    - [ ] 断言：ctx 取消（在 `forward` 期间和 `Wait` 期间各一次）时不写响应，计 `client_gone`，不计 `bad_gateway`
    - [ ] 断言：代 429 带上了假 NewAPI 最后一次响应里的 `X-Oneapi-Request-Id` 和 `Access-Control-Allow-Origin`
    - [ ] 断言：每个场景结束后的 `Snapshot` 与 design §6.7 一致，且 `rate_limited = retries + gave_up`
    - [ ] 断言：日志是 JSON，一行一条；`intercept`、`wait`、`release`、`give_up` 的字段与 design §5 一致
  - 冲突：无

- [ ] **T6 control 包和 nahookctl**（L-GO）
  - 前置：T1、T3、T5（`control.Serve` 要用 `obs.Stats`；`cmd/nahook` 要调用 `proxy.NewHandler`）
  - 改动：`internal/control.Serve`；`cmd/nahookctl`；`cmd/nahook`（main、信号处理）
  - 依据：design §5、§6.4、§6.5；interface 2.6 ｜ audit T-F3、T-F8、T-F11
  - 完成判据：
    - [ ] 测试用临时 unix socket，覆盖四个路由和各个错误码
    - [ ] 退出码符合 design §6.5
  - 冲突：无

- [ ] **T7 mockrelay**（L-GO）
  - 前置：T1
  - 改动：`cmd/mockrelay`：`/__mode` 脚本，以及 design §6.6 里的所有脚本项；按请求计数，记录到达时间（`GET /__log`）
  - 依据：design §6.6 ｜ audit 全部 T-F 项
  - 完成判据：
    - [ ] 每个脚本项都有单测；`rate_limit` 的文案和 `429-vs-quota.md` 的原文一致
  - 冲突：无

- [ ] **T8 构建、打包和第一次发布**（L-GO）
  - 前置：T5、T6、T7；Win11 上的 `gh auth login` 已完成
  - 改动：构建脚本 `scripts/build.ps1`（构建 → 写 `BUILDINFO.txt` 和 `SHA256SUMS`）；`scripts/release.ps1`（`gh release create`）；`deploy/nahook.service`、`deploy/nahook-test.service`、`deploy/mockrelay.service`、`deploy/config.example.json`、`deploy/test.example.json`
  - 依据：design §7（构建、产物、发布与分发流程）；00 §6.4 ｜ audit §3
  - 完成判据：
    - [ ] audit §3 的门禁都通过（在 Win11 的 `O:\NaHook` 上执行）
    - [ ] `O:\WinCache\nahook\dist\<8hex>\` 下有三个 linux/amd64 静态二进制，以及 `BUILDINFO.txt` 和 `SHA256SUMS`
    - [ ] GitHub 上出现了 prerelease `build-<8hex>`，带五个附件；在 Win11 上把附件下载到一个新的空目录，`sha256sum -c` 能通过（用 Git Bash 执行）
    - [ ] 产物不进 git
  - 冲突：无

- [ ] **T9 搭测试环境（甲）**（L-OPS，**每一步在 LT 上的操作都要请示用户**）
  - 前置：T8 已发布 Release；K3；用户完成测试 NewAPI 的初始化
  - 改动：在 LT 上：
    - 从 Release `build-<8hex>` 下载到 `/root/nahook-dist/<8hex>/`，`sha256sum -c SHA256SUMS` 通过后，再安装到 `/usr/local/bin`；
    - 安装 `nahook-test.service`、`mockrelay.service`；
    - 起测试 NewAPI 容器（00 §6.3）；
    - Caddyfile 加测试站点 19443（00 §6.2），reload
  - 依据：00 §2、§6 ｜ audit §2
  - 完成判据：
    - [ ] `curl 127.0.0.1:19443/api/status` 返回 200
    - [ ] mockrelay 设为 `ok` 时，经 19443 能拿到完整的流
  - 冲突：`/etc/caddy/Caddyfile`（见 §3）

- [ ] **T10 在测试环境跑审计清单**（L-OPS）
  - 前置：T9
  - 改动：不改代码；只记录结果
  - 依据：audit §2 的 T-C1–T-C4、T-F1–T-F16；audit K4、K6
  - 完成判据：
    - [ ] 每一项都有实测记录（命令、输出摘要、结论），写进 specaudit §3
    - [ ] T-C1/T-C2 的结论回写到 00 §6.1，决定生产路由要不要 `flush_interval -1`
  - 冲突：`/etc/caddy/Caddyfile`（切换 T-C1/T-C2 时）

- [ ] **T11 生产阶段 1：部署 nahook + Caddy 生产路由（护栏仍开着）**（L-OPS，**需要用户同意**）
  - 前置：T10 全部通过
  - 改动：安装 `nahook.service` 和 `/etc/nahook/config.json`（`retry.max=3`）；备份 Caddyfile 后加生产路由（00 §6.1），reload
  - 依据：00 §1.1 阶段 1 ｜ audit P-1
  - 完成判据：
    - [ ] P-1 通过
    - [ ] 回退演练：去掉路由后 reload，请求正常
  - 冲突：`/etc/caddy/Caddyfile`

- [ ] **T12 生产阶段 2：在 h3 上关护栏**（L-OPS，**需要用户同意**）
  - 前置：T11
  - 改动：备份 `compose.yaml`；把 `RESPONSES_STREAM_STARTUP_GUARD_ENABLED` 设为 false；重建容器
  - 依据：00 §1.1 阶段 2 ｜ audit P-2、K2
  - 完成判据：
    - [ ] P-2 通过
    - [ ] K2 确认了真实帧的形态
  - 冲突：`/root/container/luna-task/compose.yaml`

- [ ] **T13 生产阶段 3：换成官方镜像**（L-OPS，**需要用户同意**）
  - 前置：T12 稳定运行（观察时长由用户定）；K4 通过；MySQL 已备份（用户操作）
  - 改动：`compose.yaml` 改用固定 digest 的官方镜像，重建容器
  - 依据：00 §1.1 阶段 3 ｜ audit P-3
  - 完成判据：
    - [ ] P-3 通过
    - [ ] 回退演练方案写进 T14
  - 冲突：`/root/container/luna-task/compose.yaml`

- [ ] **T14 运维手册**（L-DOC）
  - 前置：T11
  - 改动：`渠道监控/自动调度/` 不动；在 `渠道监控/` 下新增 `nahook-runbook.md`，内容包括 `nahookctl` 用法、三个阶段的回退步骤、日志怎么看
  - 依据：design §5、§6.5；00 §1.1
  - 完成判据：
    - [ ] 每个回退步骤都写到可以直接执行的命令
  - 冲突：无

## 3. 同文件冲突与合并顺序

| 文件 | 本 SPEC 的任务 | 合并顺序 |
|---|---|---|
| LT `/etc/caddy/Caddyfile` | T0（临时站点）、T9（测试站点）、T10（切换 `-1`）、T11（生产路由） | T0 → T9 → T10 → T11；每次改之前先备份，改完用 `caddy validate` 检查后再 reload |
| LT `/root/container/luna-task/compose.yaml` | T12、T13 | T12 → T13 |
| LT `/etc/nahook/*.json` | T9、T11；运行期间由 nahookctl 写入 | 部署之后只允许通过 nahookctl 修改 |
| nahook 仓库 | T1–T8 | 车道内串行 |

全局锁表以 `00-总体架构/tasks.md` §3 为准。

## 4. 完成定义

- [ ] audit §2 的每一项都有实测记录
- [ ] audit §3 的 CI 门禁已经接入构建脚本
- [ ] audit §5 的 K1–K6 全部查清，并已回写 design 和 interface，在 specaudit 里销项
- [ ] 生产三个阶段都完成，并且都做过回退演练（或者用户明确免掉）
