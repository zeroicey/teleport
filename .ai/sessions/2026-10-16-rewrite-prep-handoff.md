# teleport —— 重写交接（新会话从这里开始）

**Date:** 2026-10-16 · **Repo:** `teleport`（历史连续，37 个提交，最新 `a58ae2d`）
· **State:** 🚧 P0 ✅ / P1 ✅ / **P2 未开工**

---

## 一、人类要执行的命令

```bash
# 1.（可选但建议）改文件夹名：旧名字里的 "-gin" 在重写完成后就没意义了
mv /data/dev/teleport-gin /data/dev/teleport

# 2. 在这个文件夹里启动 pi
cd /data/dev/teleport && pi
```

GitHub 那边 `zeroicey/teleport` **保持原名**（它就是 teleport），remote 已设置，已推送。

**注意**：旧版服务已在 hcyj 上**停掉并禁用**（2026-10-16，用户确认无人使用）。
数据、二进制、`teleport.env`（含三个密钥）全部原样留在 hcyj 的 `/data/services/teleport/`，
P5 的数据搬运与回滚都要用。想临时恢复：`ssh hcyj 'systemctl enable --now teleport'`。

---

## 二、给下一个会话（AI）的话

**你的验证职责只有一个：保证这台电脑（hpcore）能连上 hcyj 的服务器。** 别的都不要管。

重写期间这个职责的具体含义：

```bash
# a) 本机 → hcyj 的公网入口还能通（既有服务，未受影响）
curl -s -o /dev/null -w '%{http_code}\n' https://api.hcyj.xyz/

# b) hcyj 的公网入口 + 证书
curl -s -o /dev/null -w 'verify=%{ssl_verify_result} http=%{http_code}\n' https://api.hcyj.xyz/

# c) 本机 → hcyj 的隧道那一跳（用 curl，**不要用 ping**）
ssh hcyj 'curl -s -o /dev/null -w "%{http_code}\n" http://172.17.0.1:8788 || echo "8788 无服务（预期：teleport 已停）"'
```

`api.hcyj.xyz/` 为 200、证书 `verify=0` = 链路健康。**8788 现在是空的**（teleport 已停），
这是预期的；P6 切换时 frps 会在那个端口上转发到本机。

> 参照：同一台机器上的 clip 服务（`https://clip.hcyj.xyz`）是这条链路的活样例 ——
> 它的 `frpc-relay-clip.service` + Caddy 站点块就是 P6 要给 teleport 照做的东西。

---

## 三、先读什么（顺序）

1. **`.ai/CURRENT_TASK.md`** —— 目标、P0–P6 每阶段的完成判据、以及「SQLite 单写者保证消失后
   必须显式加锁的三处」。
2. **`.ai/decisions/2026-10-16-rewrite-on-gin-and-postgres.md`** —— **第一张表是 14 项冻结契约**，
   改任意一项都会让已发出的 agent 密钥或在野分享链接失效。
3. `.ai/pitfalls/cases/` —— 6 条，其中 3 条是这次 P0/P1 真踩出来的。

**这次重写真正的风险不是写新代码，而是在换实现的过程中悄悄丢掉旧代码里已经证明过的性质。**
所以 P0 的做法是先把契约钉住并对旧实现跑绿，之后每次「绿」都是同一个判据下的绿。

---

## 四、当前进度

| 阶段 | 状态 | 产出 |
| --- | --- | --- |
| **P0** 契约清点 | ✅ | 14 项契约逐条 grep「是否被测试引用」→ 发现 **agentkey 三个派生标签零覆盖**，补了 4 个守卫（标签字面量 + 黄金字节 + 独立重算 + 续期域分离）。测试 243 → 247。两次反证确认会变红。 |
| **P1** PG 基线 | ✅ | 库/角色 `teleport`、`migrations_pg/0001_init.sql`、生成的 `schema.pg.sql`、`MigratePG` 运行器、8 个真库测试（在独立的 `teleport_test` 上）、两道漂移守卫。 |
| **P2** store 换 pgx | ⛔ 未开工 | 见下 |
| P3 | ⛔ | `internal/httpx` → Gin 中间件；`internal/api` 移植 |
| P4 | ⛔ | 全链路起新版 + 243 测试 + 真实浏览器 |
| P5 | ⛔ | SQLite → PG 数据搬运（5 报告 / 2 密钥 / 全部分享链接）+ 行数核对 |
| P6 | ⛔ | 切到 hpcore（frps :8788 + Caddy **不用改**，它本来就指 `172.17.0.1:8788`） |

**闸门现在是**：`scripts/check.sh` = 前端 typecheck + schema 同步 + gofmt + go vet + go test `-count=1`。
（旧仓库的闸门缺 gofmt 与 vet，P0 时补上的 —— 第一次跑就报出 4 个文件。）

---

## 五、P2 开工前必须先定的一件事

**测试策略。** 243 个测试现在跑在**内存 SQLite** 上（隔离、可并行、快）。换成 PG 后必须有真库：

| 方案 | 代价 |
| --- | --- |
| 共享测试库 + 每测 TRUNCATE | 简单，但**串行**；套件里有并发测试，TRUNCATE 会把它们拖慢 |
| **每测一个 schema**（建议） | 稍多几行脚手架，可并行、可隔离 |
| testcontainers | 最干净，但每次跑测试起容器，重 |

建议**每测一个 schema**，并沿用 clip 那道守卫（DSN 的库名必须含 `test`，否则拒绝运行）。
**这个决定要先写进 ADR 再动手** —— 策略定错了，P2 的每一次迭代都会变慢。

**P2 的核心风险**：SQLite 的单写者保证在 Postgres 里消失了。
现在免费拿到原子性的三处 —— **待批准上限（`KEY_MAX_PENDING`）、密钥上限（`KEY_MAX_ACTIVE`）、
分享链接计数** —— 必须显式加锁（`SELECT … FOR UPDATE` 或 advisory lock），
否则会变成「看起来在限制，实际能超」。建议先给这三处写并发测试当靶子，再改实现。

---

## 六、不要重复的坑（详情见 `.ai/pitfalls/cases/`）

- **别假设 `.hcyj.xyz` 泛解析指向 hcyj**：它曾经指向 `81.148.233.134`（**英国电信 BT 的地址**），
  而 `ping` 通。判断要用**权威 DNS 回答 + 延迟量级 + 端口是否真在服务**。
- **`gofmt` 会重排 doc comment**，并把注释里的两个 ASCII 撇号渲染成弯引号 `”`。
  技术注释想说「空字符串」就写散文，别用引号。
- **否定型断言必须断言「因为什么失败」**：`json_valid` → `IS JSON` 与 `IS NOT NULL` 的 CAST
  都会「报错」，只断言 `err != nil` 的测试两种实现都通过。要断言 SQLSTATE（23514 = check_violation）。
- **`go test` 缓存会跳过读包外文件的守卫**：会返回缓存的 PASS。主守卫放 shell、加 `-count=1`。
- **反证脚本自己也会骗人**：先断言「变异真的生效」再解读结果，否则「守卫没抓住」这个结论可能是假的。

---

## 七、这条链路怎么长这样（一句话背景）

后端要跑在**本机**（hpcore，家庭电信），公网入口在 **hcyj**（阿里云广州，有公网 IP 与证书）。
**hcyj → 本机的入向 TCP 是死的**（实测：6 个端口全 timeout，而 `hpcore → hcyj` 通、
`tailscale ping` 有 pong 却是 DERP 中继），所以隧道必须由**本机主动拨出**（frp）。
Caddy 在 hcyj 终止 TLS、只做转发。clip 已经用这条链路跑通了，teleport 的 P6 照抄即可。
