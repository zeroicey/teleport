# P5 数据搬运：schema 由谁建，以及工具怎么读旧库

**Status:** ✅ ACCEPTED · **Date:** 2026-10-16 · **Deciders:** agent（用户在 P5 简报里把这一项交给我裁决：
「先想清楚 schema 由谁建、要不要在 P5 建」）· **Supersedes:** none
· **Related:** `2026-10-16-rewrite-on-gin-and-postgres.md`（纲领，第 5 条验收）、
`2026-10-16-p2-test-strategy.md`（隔离夹具）、pitfall `2026-10-16-merged-baseline-hides-a-stale-test-db.md`

## 背景（只有事实和约束）

- 旧数据在 **hcyj** 的 `/data/services/teleport/teleport.db`（SQLite，5 报告 / 6 分享链接 /
  2 密钥 / 4 申请 / 0 续期）。约束：**只读获取，不改 hcyj 上任何东西**。
- 生产意图库 `teleport` 在 PostgreSQL 里**是空的（0 张表）**；schema 的唯一真源是
  `backend/internal/store/migrations_pg/0001_init.sql`（**合并式基线**）。
- P1.1 的事故：合并式基线**按文件名记录、永不重放**，所以「用某个旧版本的基线建出来的库」
  会永久留在那个版本，而文件对文件的漂移守卫看不见它。那次坏的是 `teleport_test`。
- 旧 SQLite 的 schema 与新 PG 的 schema **列名/列序完全一致**（已实测比对
  `pragma_table_info` 与 `information_schema.columns`），所以搬运是纯行搬运，不需要列映射表。
- 时间列两侧都是 epoch 毫秒整数；`expires_at=0` = 永不过期。
- PostgreSQL 实例与 clip / registry / serenique 共用，`max_connections=100` 是大家共用的预算。

## 选项

| 选项 | 好处 | 坏处 | 可逆性 |
| --- | --- | --- | --- |
| A. 搬运脚本自己发 DDL（建表） | 一条命令搞定 | schema 出现第二个真源；与嵌入迁移器的校验和守卫并行存在，二者迟早不一致。**这正是 P1.1 那类事故的温床** | 难（错误一旦落库就是「看起来对的库」） |
| B. 搬运脚本调 `store.OpenPG`（应用自己的迁移器）建 schema，自己只 INSERT | schema 真源只有一处；与运行期启动路径逐字节相同；校验和守卫自动生效 | 导入命令会顺带迁移（对「只读核对」不适用，所以 `verify` 不迁移） | 易 |
| C. **P5 就把生产库 `teleport` 的 schema 建起来**，P6 只灌数据 | P6 少一步 | 从 P5 到 P6 之间基线一旦改动，生产库就**静默变旧**（P1.1 的原样重演），而校验和守卫要到 P6 启动时才报警 | 中（要 DROP 重建） |
| D. P5 只在 `teleport_test` 的**临时 schema** 里彩排，生产 schema 留给 P6 在切换窗口内现建现灌 | 没有「半成品生产库」的窗口；彩排跑的是**完全相同的代码路径**；P6 的建库与灌数据在同一窗口，不存在静默变旧的间隔 | P6 当天第一次在真库上建 schema（但迁移器本身已由 P1/P2/P4 反复验证） | 易 |

## 决定

**采用 B + D。**

1. **schema 由应用自己的迁移器建，谁也不许再写第二份 DDL。** 搬运工具通过 `store.OpenPG` →
   `store.MigratePG` 打开目标，所以它写进去的任何一行都对应一个**内容哈希已被记录**的基线。
   工具本身**从不发 DDL**。`verify` 子命令**不迁移**：它必须能回答「现在这里对不对」，
   而不能先改变被问的对象。
2. **P5 不建生产库的 schema。** 生产 `teleport` 保持 0 张表；P5 只在 `teleport_test` 里
   建一个用完即删的 schema 做彩排。P6 在切换窗口内按顺序：`migrate` → `import` → `verify`。
   理由：合并式基线 + 提前建库 = 静默变旧的窗口（选项 C 的坏处），而这个窗口换来的只是
   P6 少敲一条命令。
3. **读旧库用 `sqlite3` 命令行 shell（`-json` 模式，`mode=ro&immutable=1` URI），不引入
   任何 Go SQLite 驱动。** 服务器二进制不该为一次性迁移背一个 SQLite 客户端或第二个驱动；
   `go.mod` 因此零新增依赖，P4 验过的发布产物一个字节都不变。附带收益：写侧（pgx）与读侧
   （sqlite3 C 实现）是**两个独立实现**，同一处误解不会同时骗过两边。
4. **搬运是「全有或全无」的单事务，且默认拒绝非空目标。** 只有显式 `-replace` **且** `-yes`
   才会 TRUNCATE；`-replace` 单独出现时只打印「它将要删掉什么」然后退出（码 3）。

## 后果

- P6 的搬运命令是 `sqliteimport import -dsn "$DATABASE_URL"`（不带 `-replace`），后面跟
  `sqliteimport verify`。两者都是本仓库的代码，都用 `store` 的读写路径，所以「搬运后行为没变」
  这句话的判据与线上运行期是同一套。
- 「schema 由谁建」此后只有一种答案：`migrations_pg/*.sql`。任何想给搬运工具加 DDL 的改动
  都应当被这条决策挡下。
- 代价：迁移工具**需要 `sqlite3` 可执行文件**。它已经装在 hpcore 上（3.53.4），且脚本会先
  检查；缺了会明确报错而不是静默跳过。
- 代价：`-replace` 在线上是**危险**的（会清空一个正在服役的库）。两个开关 + 先打印将删内容，
  是刻意选的最低成本护栏；它不能替代「不要在生产上随手跑它」。
- 已实测（P5 彩排，`scripts/verify-p5.sh`）：真源 → 真 schema，15 项检查全绿，
  且负向对照能让其中两项**按名字变红**。

## Revisit when

- 基线不再合并（改成增量迁移、旧库可以重放）→ 选项 C 的坏处消失，可以重新考虑提前建库。
- 迁移工具需要跑第二次（例如 P6 之后又要灌入新数据）→ 那时需要一个新的决策，因为
  `-replace` 的语义（谁赢、源里没有的行怎么办）从未被规定过。
- 旧主机上出现 `teleport.db-wal` 且无法 checkpoint → 获取方式要改成 `sqlite3 .backup` 并重新评审
  （`immutable=1` 读法会静默丢掉 WAL 里的已提交事务）。
