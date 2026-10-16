# teleport —— P2 中途交接（store 换 pgx 尚未开始）

**Date:** 2026-10-16 · **Branch/Commit:** `master @ 1315b08` · **Duration:** 一个会话
· **State:** P0 ✅ / P1 ✅（附更正）/ **P2.1 ✅ / P2.2 ✅ / P2.3 ⛔ 未开工**
· **Gate:** `scripts/check.sh`（无库可跑）与 `scripts/test-pg.sh`（强制真库）**两档都绿**

---

## 一、先读什么（顺序）

1. `.ai/CURRENT_TASK.md` —— P2 已拆成 P2.1/P2.2/P2.3，看那张表的「下一步」。
2. `.ai/decisions/2026-10-16-p2-store-port-technique.md` —— **P2.3 的做法已裁决为 B
   （一次性大爆炸重写）**，文件里列了 B 必须做的 5 项缓解。这是本会话最该先读的文件。
3. `.ai/decisions/2026-10-16-p2-test-strategy.md` —— 测试夹具怎么用，以及上限锁的实测证据。
4. `.ai/pitfalls/cases/2026-10-16-*.md` —— 本会话真踩出来的 3 条。

## 二、本会话交付了什么

| 交付 | 位置 |
| --- | --- |
| 每测一个 schema 的 PG 夹具 | `backend/internal/store/storetest/` |
| 两档闸门 + 自检 | `scripts/test-pg.sh`、`scripts/check.sh`、`package.json` |
| 上限锁设计验证（3 个测试） | `backend/internal/store/cap_lock_pg_test.go` |
| 迁移内容哈希守卫（4 个测试） | `backend/internal/store/pg.go`、`migration_checksum_pg_test.go` |
| 夹具自身的守卫（3 个测试） | `backend/internal/store/storetest_harness_test.go` |

测试数：247 → **257**。`pg_test.go` 的 8 个真库测试改用夹具，**断言一字未改**。

### 怎么跑真库测试

```bash
cd /data/dev/teleport
export TEST_PG_DSN="postgres://teleport:$(sudo cat /data/services/teleport/.pgpassword.tmp)@127.0.0.1:5432/teleport_test?sslmode=disable"
bash scripts/test-pg.sh          # 强制：无 DSN 直接退出；并自检夹具仍拒绝非 test 库
env -u TEST_PG_DSN bash scripts/check.sh   # 无库档：PG 测试 SKIP，但会大声提醒
```

`teleport` 角色口令在 `/data/services/teleport/.pgpassword.tmp`（600 root:root，需 `sudo`）。
**不要把它写进仓库**。Postgres 是 docker 容器 `postgres`，宿主 `127.0.0.1:5432`。

## 二点五、本会话新增的裁决（P2.3 不必做，但 P6 前必须知道）

`.ai/decisions/2026-10-16-teleport-own-hostname.md`（✅ ACCEPTED，用户裁决）：

- teleport 将拥有 `https://teleport.hcyj.xyz/`，**根路径**（`ROUTE_PREFIX=""`）。
- **P6 与切换一起做**，不提前。（纲领里「重写期间不要改 hcyj 的 Caddy」继续有效。）
- **旧前缀 `api.hcyj.xyz/yeciorez/teleport` 永久保留**：`expires_at=0` 的分享链接
  永不过期，删前缀就是永久碎掉它们。两个入口长期并存。
- 已核实：`agentkey.DeriveToken` 只吃 `SESSION_SECRET`+两个标签+applicationID+claimSecret，
  **不吃 URL**，所以域名变更**不失效**已发出的 agent 密钥。
- P6 之前**不要**碰 `ROUTE_PREFIX`/`PUBLIC_BASE_URL`/Vite `base`/`/ai.md` 里的地址，
  否则会做出一个只有新域名能开、旧链接全碎、且未验证过的中间态。
- 线上继续 **502**（用户裁决：不临时恢复旧版），所以 P2.3/P3/P4 期间**没有线上参系可比**；
  P5 的对账靠 SQLite 文件本身（仍在 hcyj `/data/services/teleport/`）。

## 三、P2.3 具体怎么做（已备好清单，别重新推导）

做法已定：**B，一次性把 `queries.go` + `keys.go` 改成 pgx，一个提交**。下面是本会话
已经查清的、执行时需要的东西。

### 3.1 迁移到 PG 的 SQL 方言差异（逐条）

| 现在（SQLite） | 改成（PG） | 注意 |
| --- | --- | --- |
| `?` 占位符（`queries.go` 31 处、`keys.go` 41 处） | `$1, $2, …` | 逐个手改，**同一个值重复出现要重新编号** |
| `sql.ErrNoRows` | `pgx.ErrNoRows` | 漏掉会把「不存在」变成 500 |
| `MAX(?, updated_at + 1)`（`queries.go:524`） | `GREATEST($n, updated_at + 1)` | PG 的 `max()` 是聚合函数，两参数版必须用 `GREATEST` |
| `res.RowsAffected()` | 同名同形 | `pgconn.CommandTag` 有同样方法，调用点不用改 |
| `boolToInt` / `is_active = 1` | **保持不变** | schema 刻意用 INTEGER + CHECK 而不是 BOOLEAN，见 P1 的 4 号转换 |
| `unixepoch('subsec')*1000`（`store.go:152`，SQLite 迁移器） | PG 侧已经是 `time.Now().UnixMilli()` | 随 `store.go` 一起删 |

### 3.2 方法签名冻结（否则要改所有测试断言）

`Store` 的方法**不要加 `ctx` 参数**。加了就要动每一个调用点和每一处断言，而 ADR 里
「断言冻结不许重写」这条缓解正是靠签名不变才成立。内部用 `context.Background()`。
（P3 移植 Gin 时如果想把 ctx 串起来，那是另一次改动。）

### 3.3 要删的 SQLite 残留

- `backend/internal/store/store.go`（`Open` + `Migrate` + `migrationFS` + 全部 pragma 注释）
- `backend/internal/store/migrations/`（0001、0002 两个 SQLite 迁移）
- 仓库根 `schema.sql`、`scripts/check-schema-sync.mjs`
- `package.json` 的 `db:check-schema` 脚本、`scripts/check.sh` 里对应的那一步
- `backend/go.mod` 的 `modernc.org/sqlite`（及 `mattn/go-isatty`、`ncruces/go-strftime`、
  `modernc.org/libc|mathutil|memory|bigfft`、`dustin/go-humanize`、`remyoudompheng/bigfft`、
  `google/uuid` 中只被 sqlite 依赖的那部分 —— 用 `go mod tidy` 收尾并逐个确认）
- `backend/internal/store/migration_upgrade_test.go` —— 它的主题是 SQLite 的
  0001→0002 升级路径，PG 侧只有合并式基线，这个测试**失去意义**，删掉而不是硬改

### 3.4 测试侧要动的地方（断言不许改，只改取数方式）

- `newTestStore(t)` → 用 `storetest` 建 PG store。所有 store 测试从此**需要真库**
  （无 DSN 时 SKIP —— 这正是两档闸门的意义）。
- 测试里直接摸原始句柄的地方要改，共 **13 处**：
  - `s.db.QueryRow/Exec`（`handoff_test.go` 2、`reports_test.go` 1、`store_test.go` 4、
    `keys_test.go` 3）
  - `s.DB().QueryRow/Query`（`keys_test.go` 5，`DB()` 现在返回 `*sql.DB`）
  - 改成 `s.Pool().QueryRow(ctx, …)`，占位符同样要 `$n`。
- **`keys_test.go:106` 的 `SELECT name FROM sqlite_master WHERE type='table'` 是特例**：
  它在断言「库里没有明文密钥」，靠遍历 sqlite 元数据表。PG 对应物是
  `information_schema.tables` + `information_schema.columns`（`table_schema = current_schema()`）。
  这是唯一一处**测试机制**必须重写的地方 —— 但它断言的**性质**（明文永不入库）必须原样保留，
  而且这是冻结契约之一。改的时候把这一点写进提交信息。
- `internal/api/*_test.go` 四处 `store.Open(cfg.DBPath)` → 换成夹具。
  `config.Config.DBPath` 之后应换成 `DatabaseURL`（P3 一起做也可以，但要记着）。
- `store_test.go:37` 的 `TestMigrateIsIdempotent` 读 `migrationFS`：PG 侧已有
  `TestPGMigrationsApplyAndAreIdempotent` 覆盖同一性质，这个删掉。

### 3.5 sqlc

`sqlc v1.31.1` 已装（`/home/oicey/go/bin/sqlc`）。照抄 clip 的 `sqlc.yaml`
（`/data/dev/clip/sqlc.yaml`）：`schema` 指 `internal/store/migrations_pg`，
`sql_package: "pgx/v5"`，`emit_empty_slices: true`。两个动态 `SET` 列表
（`UpdateShareToken`、`UpdateAgentKey`）用 `COALESCE($n::type, column)` 表达成**单条静态语句**
（`revoked_at` 用 `CASE WHEN $n::boolean IS NULL THEN revoked_at WHEN $n THEN $now ELSE 0 END`），
这样 sqlc 能生成，PATCH 的「字段缺失 vs 显式设值」语义也保住。防漂移用 `sqlc diff`（照抄 clip）。

### 3.6 三处上限必须用已验证的锁

`pg.go` 里的 `LockPendingApplications` / `LockActiveKeys` / `LockPendingRenewals`，
在事务**开头** `SELECT pg_advisory_xact_lock($1)` 再计数。
`cap_lock_pg_test.go` 已经证明：不加锁时 cap=1 能落 2 行；加了锁第二个事务会被真的挡住。
`KEY_MAX_ACTIVE` 的两个入口（`ClaimApplication`、`CreateManualKey`）**必须用同一把锁**。

## 四、已验证 / 未验证

- [x] 两档闸门都绿（本会话最后一次：`check.sh` + `test-pg.sh`）
- [x] 夹具隔离：两个 Pool 互不可见；schema 随测试拆掉；DSN 守卫被反证（`/teleport` 被拒）
- [x] 上限锁：确定性实验 + 24×5 统计实验
- [x] 迁移哈希守卫端到端反证（改文件→拒绝，撤销→通过，文件与 HEAD 逐字节相同）
- [x] 连接预算：整套 store 跑下来峰值 14 个连接（基线 9）
- [x] 大陆链路健康：`https://api.hcyj.xyz/` = 200、证书 `verify=0`；
      `8788` 无服务（预期，teleport 已停）
- [ ] **未验证：store 本体一行都没改**（P2.3 全部未做）
- [ ] 未验证：sqlc 能否表达那两个动态 SET（预计能，未实测）

## 五、不要重复

1. **别在循环里开连接池。** `t.Cleanup` 在**测试**结束时才跑，不是每轮。20 轮 × 2 池
   = 200 个连接，而 `max_connections=100` 是与 clip/registry/serenique **共用**的 ——
   失败形态是「别人的服务连不上」，不是「这个测试失败」。见 pitfall。
2. **别用「让子测试故意失败」来反证。** 子测试失败会连父测试一起判失败，套件永远绿不了；
   而且子测试被 **skip 时 `t.Run` 返回 true**，父测试会把 skip 读成成功。要断言就把判断
   抽成纯函数（`storetest.CheckDSN` 就是这么来的）。
3. **夹具不能 import `store`。** store 自己的测试要 import 夹具，`storetest → store` 会成环
   （实测报 `import cycle not allowed in test`）。迁移器作为参数传进去。
4. **`search_path` 只指那一个 schema，绝不带 `public`。** 带了就会在对象缺失时静默回落到
   共享 schema，而测试照样「通过」。夹具里 `assertObjectsLandedIn` 专门盯这个。
5. **schema 必须在开 scoped pool 之前创建。** 未限定名的 DDL 会落到 search_path 里第一个
   **已存在**的 schema；先连池再建 schema 会让迁移把表建到别处去。
6. **合并式基线改了文件，必须重建库。** 现在会被哈希守卫挡住并明确报错，别再手动绕。
7. **gofmt / shellcheck 都吃撇号**：注释里 `store's` 这种会让 shellcheck 的解析炸掉，
   写散文别用引号（本仓库既有 pitfall 记过同一类事）。

## 六、下一步（第一个动作）

按 3.1–3.6 执行 P2.3：先 `queries.go`（reports/share_tokens），再 `keys.go`（keys/renewals），
改完立刻 `bash scripts/test-pg.sh`。**断言不许为了变绿而改**；每条必须改的断言都要在提交
信息里说明它属于哪条冻结契约、以及为什么原来是错的。

若中途发现 B 的批量失败无法区分「真 bug」与「方言写错」，ADR 的 Revisit 条款允许局部退回 A：
**只把那一组方法**做成双实现对照，而不是继续盲修。
