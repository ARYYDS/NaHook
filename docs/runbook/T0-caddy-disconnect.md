# T0 方案：Caddy 2.6.2 是否把"客户端断开"传给后端

对应 `spec/01-nahook/tasks.md` 的 T0、`spec/01-nahook/audit.md` §5 K1、`spec/specaudit.md` 的 Q1。

**执行前必须得到用户同意。** 一次同意覆盖第 2 步到第 9 步，其中包括两次 `systemctl reload caddy`。任何一步的检查没通过，都直接跳到第 9 步回退，然后上报。

## 1. 要回答的问题

LT 的生产站点配置了 `flush_interval -1`。Caddy 文档对负值的说明是：客户端提前断开时，**不取消**发往后端的请求。这个行为来自 #4952，2024 年 11 月回退（caddy#6666）。

nahook 的设计依赖"客户端断开 → nahook 的 `r.Context()` 被取消 → 停止重试"。所以要实测下面两件事，并得出结论：

| 阶段 | 要测的 |
|---|---|
| 等待响应头（nahook 正扣着响应、在等待或重试） | 写和不写 `-1`，客户端断开后，Caddy 会不会关掉到后端的连接 |
| 已经开始流式输出 | 同上。这一项还关系到现网：客户端断开后，NewAPI 会不会继续生成 |

## 2. 假设

| 编号 | 假设 |
|---|---|
| H1 | 写了 `-1`：等待响应头阶段客户端断开，Caddy **不**关后端连接 |
| H2 | 不写 `-1`：约 0–1s 内关掉后端连接 |
| H3 | 流式阶段的行为和 H1、H2 一致 |

## 3. 对生产的影响与风险

| 动作 | 影响 | 控制措施 |
|---|---|---|
| Caddyfile 末尾追加一个站点 `http://127.0.0.1:19444`（`bind 127.0.0.1`） | 只监听本机回环地址，不碰 `lt.xhub.chat` 站点 | 先生成一个候选文件，用 `caddy validate` 校验通过后才替换 |
| `systemctl reload caddy`（共两次） | Caddy 平滑重载；配置加载失败时继续使用旧配置。正在进行的长连接按 Caddy 的平滑机制排空，LT 没有配置 `grace_period` | 选在低峰时段执行；重载后马上检查生产健康 |
| 在 127.0.0.1:19082/19083 跑一个 Python 测试后端 | 只监听回环地址；20s 超时，占用很少 | 实验结束后 kill 掉 |
| 往 `/root/nahook-t0/` 写文件 | 只有脚本和日志，几 KB | 留作记录，不删 |

预计总用时约 12 分钟：准备 3 分钟、实验 5.4 分钟、恢复和汇报 3 分钟。

## 4. 前置检查（不改任何东西）

```bash
sha256sum /etc/caddy/Caddyfile
```

记下完整哈希。2026-10-20 读到的前缀是 `c688d03e06853104`；如果不一致，说明有人改过，停下来先问。

```bash
systemctl is-active caddy
```

```bash
ss -ltn | grep -E ':(19444|19082|19083)\b' || echo free
```

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://lt.xhub.chat/api/status
```

通过标准：哈希和记录一致；caddy 是 `active`；三个端口都空闲；生产健康检查返回 200。

## 5. 步骤

| 步骤 | 命令 | 通过标准 |
|---|---|---|
| 1 上传脚本 | 用 USGL 上传：`ops/t0/t0_backend.py`、`t0_client.sh`、`t0_report.py`、`t0-site.caddy` → `/root/nahook-t0/` | 四个文件都在 |
| 2 备份 | `cp -p /etc/caddy/Caddyfile /etc/caddy/Caddyfile.bak.t0` | 备份文件的 sha256 和第 4 节记下的一致 |
| 3 生成候选并校验 | `cat /etc/caddy/Caddyfile /root/nahook-t0/t0-site.caddy > /root/nahook-t0/Caddyfile.candidate && caddy validate --config /root/nahook-t0/Caddyfile.candidate --adapter caddyfile` | 输出 `Valid configuration` |
| 4 替换并重载 | `cp /root/nahook-t0/Caddyfile.candidate /etc/caddy/Caddyfile && systemctl reload caddy` | 重载退出码为 0 |
| 5 检查 | `systemctl is-active caddy`；`ss -ltn \| grep 19444`；`curl -sS -o /dev/null -w '%{http_code}\n' https://lt.xhub.chat/api/status` | caddy 是 `active`；19444 在监听；生产返回 200 |
| 6 启动后端 | `cd /root/nahook-t0 && nohup python3 t0_backend.py > backend.out 2>&1 & echo $! > backend.pid` | `backend.log` 里出现两行 `listening` |
| 7 跑客户端 | `cd /root/nahook-t0 && nohup bash t0_client.sh > client.out 2>&1 &` | 约 5.4 分钟后，`client.log` 最后一行是 `{"done":true}`。中途不要反复查看，等 6 分钟再看一次 |
| 8 出报告 | `python3 /root/nahook-t0/t0_report.py` | 12 行，每行都有结果 |
| 9 恢复 | `kill "$(cat /root/nahook-t0/backend.pid)"`；`cp -p /etc/caddy/Caddyfile.bak.t0 /etc/caddy/Caddyfile`；`caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile`；`systemctl reload caddy` | 见第 7 节 |

## 6. 判定表与设计落点

"传递了"的判定：

- 等待响应头：后端事件是 `peer_closed`，并且 `delta_s ≤ 1.0`。
- 流式阶段：后端事件是 `write_failed`，并且 `delta_s ≤ 2.0`（后端每秒写一次，再加上缓冲）。

**等待响应头阶段（决定 nahook 的设计）：**

| `hold-neg1` | `hold-default` | 结论 | 落点 |
|---|---|---|---|
| 不传递 | 传递 | H1、H2 成立：`-1` 吞掉了断开 | 生产 nahook 路由**不写** `flush_interval`（`spec/00-总体架构/design.md` §6.1），测试站点 T-C2 也这样配；nahook 不需要另加总时限 |
| 传递 | 传递 | 2.6.2 不受 #4952 影响 | 同样不写，保持一致；把结论记下来 |
| 不传递 | 不传递 | 2.6.2 在等待响应头阶段都不传递 | nahook 的重试本来就受 `retry.max=3` 限制，每个被放弃的请求最多多打 3 次上游，浪费有上限。另把"升级 Caddy"或"给 nahook 加单请求总时限"作为待拍板，写进 `spec/01-nahook/design.md` §4 |
| 传递 | 不传递 | 结果反常 | 整组重跑一次；还是反常就上报 |

同一组合的 3 次结果不一致时，按"不传递"处理，并上报。

**流式阶段（关系到现网，单独上报）：**

| `stream-neg1` | 含义 | 落点 |
|---|---|---|
| 不传递 | 现网主站点有 `-1`：客户端断开后，NewAPI 会一直生成到结束，上游费用照样产生 | 单独请示用户：是否去掉主站点的 `-1`。这件事不在 NaHook 范围内 |
| 传递 | 现网没有这个问题 | 记录即可 |

`stream-default` 传递，就说明 T-C3 的预期成立。

## 7. 回退和恢复的检查

无论实验成功与否，最后都执行第 5 节第 9 步，然后检查：

```bash
sha256sum /etc/caddy/Caddyfile
```

必须和第 4 节记下的完全一致。

```bash
ss -ltn | grep -E ':(19444|19082|19083)\b' || echo free
```

必须输出 `free`。

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://lt.xhub.chat/api/status
```

必须是 200。

如果第 3 步校验没过：不替换文件、不重载，直接结束。如果第 4 步重载失败：Caddy 还在用旧配置，执行第 9 步的恢复命令（cp 回备份 + validate + reload）。

## 8. 记录和上报

- 把 `t0_report.py` 的输出原样贴到 `spec/specaudit.md` 的 Q1，并回写 `spec/00-总体架构/design.md` §6.1 和 `spec/01-nahook/audit.md` K1。
- `/root/nahook-t0/` 下的日志留在 LT 上，不拷回来。

## 9. 局限

- 本实验走的是 HTTP/1.1 回环。生产客户端经 HTTPS 连接，可能用 HTTP/2，取消的机制不同（RST_STREAM）。
- 为了不在生产机上启用 `tls internal`（会动系统信任库），这里不测 HTTP/2。HTTP/2 下的表现，留到 P-1 阶段看生产 nahook 日志里客户端断开后 ctx 有没有被取消，再做确认。
