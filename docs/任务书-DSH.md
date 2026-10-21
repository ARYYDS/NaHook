# NaHook 开发任务书（Win11 · DSH）

## 0. 一句话

在 Win11 的 `O:\NaHook` 上，用 Go 标准库实现 nahook：它串在 Caddy 和 NewAPI 之间，识别流式启动阶段的上游限流，用冷却门加全抖动等待后做同站重试，重试用完返回和 NewAPI 格式一致的 429，重试上限可以通过控制口热修改。你负责 `spec/01-nahook/tasks.md` 里 L-GO 车道的 **T1–T8**。

## 1. 环境与首次准备

### 1.1 机器与目录

| 项 | 值 |
|---|---|
| 机器 | Win11（DESKTOP-C4JG869），你在 pwsh 7 里输入命令 |
| 仓库 | `https://github.com/ARYYDS/NaHook.git`（公开），本地副本 `O:\NaHook` |
| 发布 | GitHub Release，由 `gh` 发布（design §7 发布与分发流程） |
| Go | `O:\WinCache\tools\go\bin\go.exe`（go1.27.2），`GOTOOLCHAIN=local`。不要用系统自带的 go1.27.1 |
| Go 缓存 | `GOCACHE=O:\WinCache\go-build`、`GOMODCACHE=O:\WinCache\go-mod`、`GOPATH=O:\WinCache\go`（已在用户环境变量里）；`GOTMPDIR=O:\WinCache\tmp` |
| `-race` 需要的 gcc | `O:\WinCache\tools\msys64\ucrt64\bin` |
| 构建产物 | `O:\WinCache\nahook\dist\<8位提交号>\`，**不进仓库** |
| 日志 | 长任务的日志放 `O:\WinCache\logs\nahook-*.log` |

### 1.2 首次准备（由用户完成，DSH 不做）

Win11 上现有的 `gh` 令牌已经失效（2026-10-20 实测）。重新登录之后，同一个凭据既能 `git push`，也能发 Release。

第 1 步，重新登录 GitHub：

```powershell
gh auth login --hostname github.com --git-protocol https --web
```

第 2 步，让 git 走 `gh` 的凭据：

```powershell
gh auth setup-git
```

第 3 步，克隆到 `O:\NaHook`（这个目录已存在且为空）：

```powershell
git clone https://github.com/ARYYDS/NaHook.git O:\NaHook
```

第 4 步，设置提交身份（用户自己定）：

```powershell
git -C O:\NaHook config user.name "<名字>"
```

```powershell
git -C O:\NaHook config user.email "<邮箱>"
```

### 1.3 每次开工前要设置的环境（pwsh 7，在当前会话里执行）

```powershell
Set-Location O:\NaHook; $env:PATH = 'O:\WinCache\tools\go\bin;O:\WinCache\tools\msys64\ucrt64\bin;' + $env:PATH; $env:GOTOOLCHAIN = 'local'; $env:GOTMPDIR = 'O:\WinCache\tmp'
```

### 1.4 自检（每次开工都跑一遍）

| 命令 | 期望输出 |
|---|---|
| `(Get-Command go).Source` | `O:\WinCache\tools\go\bin\go.exe` |
| `go version` | `go version go1.27.2 windows/amd64` |
| `go env GOCACHE GOMODCACHE` | `O:\WinCache\go-build` 和 `O:\WinCache\go-mod` |
| `gcc --version` | 第一行能看到版本号 |
| `git -C O:\NaHook remote -v` | `https://github.com/ARYYDS/NaHook.git` |
| `gh auth status` | 显示已登录 `ARYYDS`，没有 `token ... is invalid` |
| `git -C O:\NaHook status -sb` | 当前在 `dev` 分支，工作区干净。第一次开工时这里还是 `main`：先按 §4 第 1 步建好 `dev`，再复查这一项 |

有任何一项不符合，就停下来报告，不要自己去改系统环境。

## 2. 必读（按顺序，全文读）

1. `spec/README.md`：路径前缀的含义（`NEWAPI/`、`H3/`、`渠道监控/` 分别指什么）。
2. `spec/01-nahook/design.md`：§0–§8 全文。**§1 的伪代码就是实现依据。**
3. `spec/01-nahook/interface.md`：每个函数的签名、对照的现有函数、不变量。
4. `spec/01-nahook/audit.md`：§1 要点、§2 里和单测有关的 T-F 项、§3 门禁、§4 预警。
5. `spec/01-nahook/tasks.md`：T1–T8 的前置条件和完成判据。
6. `spec/00-总体架构/design.md`：§5 存储位置、§6.4 systemd 单元（T8 要用）。
7. 参照资料：
   - `docs/reference/渠道监控/429-vs-quota.md`：限流报错的原文，mockrelay 和单测要原样使用；
   - `docs/reference/渠道监控/4-4-tpm-retry.md` §7：等待必须能取消，下限 50ms 的出处。
8. 参照源码（只读，按固定的提交号）：
   - h3 的启动帧护栏（T2 要移植它）：`https://github.com/alterem/new-api-h3/blob/38e69e3001606d8cdf7de6c4b872d8329b405c25/relay/channel/openai/responses_startup_error.go`
   - NewAPI 官方源码：`https://github.com/QuantumNous/new-api/blob/206e0fa5c600127a314a97f88592c067f784f4a6/<路径>`，`<路径>` 用 SPEC 里 `NEWAPI/` 后面的部分。

## 3. 范围与边界

| 你负责 | 你不碰 |
|---|---|
| T1–T8：Go 代码、单测、构建脚本、`deploy/` 下的单元文件和配置样例 | LT 服务器上的任何操作，包括 ssh、scp、curl 生产地址 |
| 在 `O:\NaHook` 和 `O:\WinCache\` 下读写 | T0、T9–T14 |
| 推送 `dev` 分支 | 修改 `spec/` 和 `docs/reference/`。发现冲突时停下来报告 |
| 收到明确指令时发布 GitHub Release（T8 交付时发第一次） | 没有指令就发 Release；覆盖或删除已经发出去的 Release 或 tag |
| | 合并到 `main`：架构侧审计通过后才合并 |
| | 引入任何第三方模块 |
| | 把任何二进制放进仓库 |

## 4. 工作循环（每个任务都一样）

1. `git -C O:\NaHook fetch origin`，然后 `git switch dev` 并 `git pull --ff-only origin dev`。第一次开工时 `dev` 还不存在，从 `origin/main` 新建：`git switch -c dev origin/main`。
   之后每个任务开工前，再执行 `git merge --no-edit origin/main`，把架构侧在 `main` 上的 SPEC 更新和审计修复合进 `dev`。你不改 `spec/` 和 `docs/`，所以这一步不会冲突；如果真的冲突了，停下来报告。
2. 只做当前这一个任务（Tn），不提前做后面的任务。
3. 写代码和单测。表驱动；断言用标准库 `testing`，不引入 testify，原因是不允许第三方依赖。
4. 按 `spec/01-nahook/audit.md` §3 的顺序，**原样**执行门禁命令。全部通过才能提交。
5. `git add` 只加这次改的具体文件。提交信息的格式是 `Tn: <一句话>`，正文写要点。
6. `git push origin dev`。
7. 按 §7 的模板写交付回报，然后**停下**，等待下一条指令。

交付之后，架构侧会派审计。审计期间你不要回头改已经交付的部分；如果收到审计修复的指令，按指令处理。

## 5. Git 规则

- 禁止 `git add .` 和 `git add -A`。
- 禁止 force push，禁止改写已经推送的历史。
- 仓库里有 `.gitattributes`（eol=lf）。不要把行尾改成 CRLF。
- `O:\WinCache\nahook\dist\` 在仓库外面，不要把它挪进仓库。
- 构建只针对已提交的代码：构建前 `git status --porcelain` 必须为空。

## 6. 任务清单

完成判据以 `spec/01-nahook/tasks.md` 为准，下表只列出每个任务最容易出错的地方。

| 任务 | 内容 | 特别注意 |
|---|---|---|
| T1 | `go.mod`（`module github.com/ARYYDS/NaHook`，`go 1.26`，不加 require）；`internal/config` | 先写盘再切换快照；数据面只读原子指针，写锁只给控制面用（design §1.7、§4） |
| T2 | `internal/startup`：移植 h3 的启动帧读取和判定 | 遇到异常一律放行，不要照搬 h3 回 502；`IsRateLimit` 不包含 `insufficient_quota`（design §1.3） |
| T3 | `internal/gate` | 不排队、不加锁，每个等待者自己的计时器，全抖动 `rand.N`；`until` 只能用 CAS 往后推（design §1.5） |
| T4 | `internal/proxy429` | 用 `json.Marshal`，不要用 Encoder；字段顺序是 message、type、param、code；`code` 原样保留上游的 JSON 值（design §1.4） |
| T5 | `internal/proxy`、`cmd/nahook` | 判定之前不能写 `w`；不要用 `httputil.ReverseProxy`；XFF 和 X-Real-IP 原样转发；非 200 一律透传，不重试；NewAPI 自己的终态错误（消息末尾是本次 request id）也不重试（design §1.6、§6.1、§8）。要等 T0 的结论（00 G1） |
| T6 | `internal/obs`、`internal/control`、`cmd/nahookctl` | 计数器只用原子操作，`Snapshot` 列出全部 8 个键（design §6.7）；socket 权限 0600；退出码 0/1/2；支持 `--socket` 参数（design §6.4、§6.5） |
| T7 | `cmd/mockrelay` | 脚本项要齐全（design §6.6）；`rate_limit` 的文案和 `429-vs-quota.md` 的原文一致；要能记录每次请求到达的时间 |
| T8 | `scripts/build.ps1`、`scripts/release.ps1`、`deploy/*.service`、配置样例；构建后发布第一个 Release | 输出目录 `O:\WinCache\nahook\dist\<8位提交号>\`，里面有三个二进制、`BUILDINFO.txt`（提交号、`go version`）和 `SHA256SUMS`。Release 的 tag 是 `build-<8位提交号>`，标为 prerelease，`--target` 用完整提交号；发完后把附件下载到一个新的空目录，用 Git Bash 跑 `sha256sum -c SHA256SUMS` 验证 |

## 7. 交付回报模板

```text
## Tn 交付回报
- 依据的 SPEC 章节：
- 架构设计：动了哪些结构、锁、状态和数据流
- 实现逻辑：关键函数和调用路径（文件:行号）
- 自己做的决定和优化，以及理由：
- 提交号：
- 执行的命令和实际输出（门禁命令逐条贴输出摘要，失败时贴完整输出）：
- 没做完的部分 / 风险 / 需要架构侧决定的点：
```

## 8. 必须停下来问的情况

- SPEC 和实现冲突，或者 SPEC 没写清楚。不要自己补设计。
- 必须定一个 SPEC 里没写出处的数值。
- 需要第三方依赖。
- 门禁没通过，而且原因在 SPEC 本身。
- 任何需要访问 LT 或生产环境的操作。
- 第 1.4 节的自检没通过。

## 9. 常见陷阱

摘自 `spec/01-nahook/audit.md` §4，开工前再读一遍：

- 用 `httputil.ReverseProxy`。
- 把 HTTP 429 也拿来重试。
- 照搬 h3 在异常时回 502 的逻辑。
- 给冷却门加锁或队列。
- 用 `time.Sleep` 实现等待。
- 自己发明一个平滑系数。
- 以为 NewAPI 的 `RetryTimes` 会限制 nahook 的重试。
- 引入 YAML 库。
- 把 `insufficient_quota` 算成限流。
- 在代 429 里加 `Retry-After` 或改写 message。
- mockrelay 只监听 127.0.0.1（要监听 172.17.0.1）。
