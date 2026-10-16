# teleport — P5 完成（SQLite → PostgreSQL 数据搬运 + 行为核对）

**Date:** 2026-10-16 · **Branch/Commit:** `main`（基于 `de22443`，**P5 尚未提交**）· **Duration:** 一个会话
· **State:** P0 ✅ / P1 ✅ / P2 ✅ / P3 ✅ / P4 ✅ / **P5 ✅**；**P6 ⛔（且必须先问人类）**；线上旧版仍 `disabled`，公网 502
· **Gate:** 四档 —— `scripts/check.sh`（无库，**152 SKIP**）、`scripts/test-pg.sh`（强制真库）、
`scripts/verify-p4.sh`（未重跑，P4 之后 Go/前端未改）、**`scripts/verify-p5.sh`（新，15/15 + 负向对照）**

---

## 一、先读什么

1. `.ai/CURRENT_TASK.md` 的 **P5 交付物 / P5 的核对 / P5 已实测** 三节。
2. `.ai/decisions/2026-10-16-p5-schema-ownership.md` —— **本会话最重要的裁决**：
   schema 由应用自己的迁移器建、搬运工具**从不发 DDL**、**P5 不建生产库 schema**、
   读旧库用 `sqlite3` shell（零新增依赖）。P6 的 `migrate → import → verify` 顺序就来自这里。
3. **🔴 最重要的一个**：`.ai/pitfalls/cases/2026-10-16-verifier-shares-the-conversion-it-checks.md`
   —— 校验器复用了被测代码的 `convertValue`，于是**超范围整数被静默回绕（2^63 → int64 min）
   而校验报绿**。已修两层（范围拒绝 + 让两个数据库各自渲染再比字符串）。**如果只读一条，读这条。**
4. 另两个新 pitfall：
   - `2026-10-16-controls-that-tamper-the-reference.md` —— 负向对照改错了侧（把 A 和 A 比），
     这是「验证脚本必须先被反证」那条教训的**新形状**；
   - `2026-10-16-migration-bookkeepings-do-not-match.md` —— 旧库记 `0001_init`、
     新库记 `0001_init.sql`，同名表不是同一个东西。
5. 旧 pitfall `2026-10-16-merged-baseline-hides-a-stale-test-db.md` 的**追加段**（第二次同族现象）。

## 二、怎么跑 P5

```bash
cd /data/dev/teleport
bash scripts/verify-p5.sh                      # 用 bin/import/teleport.db（已取回的副本）
P5_FETCH=1 bash scripts/verify-p5.sh           # 先从 hcyj 只读取回再跑
P5_EXPECT_SHA256=0b3855d7... bash scripts/verify-p5.sh   # 钉住源（P6 建议这么用）
P5_SKIP_BUILD=1 ...                            # 复用已有的 bin/import/sqliteimport
P5_SKIP_NEGATIVE=1 ...                         # 只跳过负向对照（**别这么用**）
```

它拒绝非 `*test*` 库名；在 `teleport_test` 建私有 schema `p5`（用完即拆）；
**不碰**生产意图库 `teleport`、**不碰** hcyj 的 Caddy/frpc/8788。
日志：`bin/import/p5-import.log`、`p5-verify.log`、`p5-negative.log`。

## 三、P5 证明了什么

| 档 | 结果 |
| --- | --- |
| 真源 → 真 schema 搬运 | 17 行 / 5 张表，单事务 |
| 行为核对（`sqlitemigrate verify`） | **20 / 20**（含 5 项 `db_rendered_text[...]`：两个数据库各自渲染、共 157 个格子） |
| 独立计数（`sqlite3` vs `psql`，对着人给的 5/6/2/4/0） | 全对上 |
| 负向对照（篡改目标） | 按名字变红：`revoked_links_stay_404` + `permanent_links_stay_permanent` |
| Go 侧篡改用例 | 15 个（含「多一列」「改列名」「删视图」「root 归属被改」这类**静默丢数据**形状）——**20 项检查每一项都有反证** |
| 对抗性夹具（自建） | 文本逐字节 / BIGINT 极值 / DOUBLE 位级 / Inf 拒绝 —— 见 CURRENT_TASK |

用户的四条要求逐条落地：**报告数 5 / 密钥数 2 / 全部分享链接 6**（源侧 `sqlite3` 数、目标侧 `psql` 数、
两边都对着人给的数字）；**`expires_at=0` 仍永久**（直接断言目标 `expires_at` 仍是 0，
而不只是「能打开」——因为「能解析」不蕴含「永久」）；**已吊销仍 404**（源里两个被吊销的 token
中有一个**同时**是永久链接，它必须同时满足两个性质）；另外补了 ADR 点名的 root 归属边界
（`owner_key_id=''` 的报告仍归 root）。

## 四、几处值得知道的判断

- **不建生产库 schema**：合并式基线按文件名记录、永不重放 → 提前建库 = 静默变旧的窗口（P1.1 的原样重演）。
  所以生产 `teleport` 保持 0 张表到 P6，P6 在同一窗口里 `migrate → import → verify`。
- **读旧库用 `sqlite3` shell，不加 Go driver**：`go.mod`/`go.sum` **零变化**；
  `go list -deps .` 证明服务器二进制**不含** `sqlitemigrate`、也不含 `os/exec`。
  `scripts/build.sh` 只 build `.`，所以新命令不进发布产物。
- **`immutable=1` + sidecar 守卫**：`-wal` 存在时拒绝（否则 WAL 里的已提交事务会被静默丢掉）。
  取源时先 `ssh sha256sum` 再 `scp` 再本地比对。
- **`-replace` 需要两个开关**：`-replace` 单独出现只打印「将要删掉什么」并以码 3 退出。
- **`updated_at` 触发器不会在搬运时触发**：导入是 **INSERT-only**，而唯一的 UPDATE 路径
  （`UpdateReport`）显式写 `GREATEST(now, updated_at+1)`，所以 `BEFORE UPDATE` 触发器的
  `NEW IS NOT DISTINCT FROM OLD` 条件在该路径上永不成立。

## 五、已验证 / 未验证

- [x] 四档闸门全绿（`check.sh` 无库档打印 **152 SKIP** 并大声提示；`test-pg.sh` 真库 0 失败）。
- [x] `gofmt -l` 空、`go vet ./...` 干净。
- [x] 源未被修改：hcyj 远端与本地副本 sha256 一致（`0b3855d7…beb24d`），**只读获取**。
- [x] 环境干净：`teleport_test` 现在 **0 张表、只剩 `public`**（清掉了 P4 会话留在 `public` 里的整套陈旧 schema
      和我手工实验留下的 `p5_scratch`）；生产意图库 `teleport` 仍 **0 张表**；无残留进程。
- [ ] **假定、未测：真往生产库灌数据。** 那是 P6，而且**必须先问人类**。
- [ ] **假定、未测：旧版与新版对账。** 旧版已停（502），ADR 早已记下这个代价。
- [ ] **假定、未测：搬运后的大陆观测点 / 真实浏览器。** 属 P6。
- [ ] **假定、未测：`sqlite3` shell 的其它版本**（本机 3.53.4）；`content`/`metadata` 里没有 NULL/BLOB
      （真源实测如此，代码里对 NULL 是「交给 PG 报约束错」而不是静默）。
- [ ] **假定、未测：实时钟边界。** `live` vs `expired` 靠 `time.Now()`，理论上恰好在这一毫秒过期的链接
      可能被两侧分类不同（真源里当前 `expired=0`，所以没被实际踩到）。

## 六、Loose ends

- [ ] **P5 尚未提交**（7 个新文件 + 4 个 `.ai` 文件）。
- [ ] `bin/import/` 与 `bin/e2e/` 都在 gitignore 里（生成物），但 `scripts/verify-p5.sh` **要提交**。
- [ ] P5 的复审：两个后台只读子代理（数据路径正确性 / 夹具可证伪性）的结果见本会话；第三个
      （架构 + P6 安全性）**provider 侧 abort**，已改用 `fusion_validate` 复核。

## 七、不要重复

1. **负向对照必须篡改「两者中的一侧」，且要断言篡改真的落地了**（改动行数 `>= 1`）。
   我第一版篡改了源、又拿它当源去比对 → 「A vs A」永远相等。（新 pitfall 有完整记录。）
2. **同名 bookkeeping 表不等于同一个东西**：旧 `schema_migrations.version` = `0001_init`，
   新 = `0001_init.sql`。写跨侧断言前先把真实内容打印一次。
3. **「能解析」不蕴含「永久」**：验证一个性质时要断言**承载该性质的那一列**，不是它的间接后果。
4. **`metadata` 摘要两侧形态不同**（源：原始文本；store：解码后的 map 再编码）。
   真数据恰好是字母序，所以第一版「靠运气绿了」。两侧都过 `canonicalJSON`。
5. **`json.Number` 必须显式处理**：`sqlite3 -json` 的整数/实数都是 `json.Number`；
   用 `float64` 接会把 64 位毫秒时间戳四舍五入，用 `Sscanf("%d")` 接会把 `720.0` 静默截断。
5b. **`int64(float64)` 超范围时结果未定义**（本平台回绕成 `MinInt64`），必须显式查范围，
   不能只查有没有小数部分。SQLite 的动态类型让超范围整数以 REAL 形式出现，
   所以这条在「INTEGER → BIGINT」的搬运里是**可达**的。
5c. **校验器不许复用被测代码的转换函数**。若两侧都要「把 X 变成 Y」，就必须有一个独立实现
   （这里是让 SQLite 与 PostgreSQL 各自渲染文本）。这是我这份工作里**第二次**踩「两个操作数
   不独立」的形状 —— 第一次是负向对照（见第 1 条）。
6. **视图也在 `information_schema.columns` 里**：目标自省要限定 `BASE TABLE`。

## 八、下一步

**P6：切换，且必须先问人类。** 顺序（全部在同一切换窗口内）：

```bash
# 0. 问人类 → 确认窗口（会改 hcyj 的 Caddy、开 frpc 8788、加 teleport.hcyj.xyz 证书）
# 1. 取源（只读），钉住 sha256
P5_FETCH=1 P5_EXPECT_SHA256=<pinned> bash scripts/verify-p5.sh   # 先在临时 schema 上再绿一次
# 2. 生产库：建 schema（应用自己的迁移器）→ 灌数据 → 核对
./bin/teleport-linux-amd64 migrate            # 需要 teleport.env 里的 DATABASE_URL（含口令，必填）
./bin/import/sqliteimport import -source bin/import/teleport.db -dsn "$DATABASE_URL"   # 不带 -replace
./bin/import/sqliteimport verify -source bin/import/teleport.db -dsn "$DATABASE_URL"
# 3. 起服务 + frpc 8788 + hcyj Caddy（新域名根路径 + 旧前缀都留着）
# 4. 两个入口 + 大陆观测点 + 真实浏览器
```

不要轮换 `SESSION_SECRET`（会让所有派生密钥失效）。不要用 `-replace` 打生产（除非真的打算清空它）。
