# A merged baseline migration hides a stale database, and the drift guards cannot see it

**Severity:** 🔴 high · **First hit:** 2026-10-16 · **Hits since:** 0

## Symptom

P1 was recorded as "✅ 8 个真库测试在真库上验证". A day later, running the same suite
against the same database:

```text
--- FAIL: TestPGMetadataCheckRejectsInvalidJSON (0.03s)
    pg_test.go:155: invalid JSON produced ERROR: invalid input syntax for type json
    (SQLSTATE 22P02) (SQLSTATE 22P02), want a CHECK violation 23514.
```

Both drift guards (`TestPGSnapshotMatchesMigrations`, the shell snapshot comparison in
`scripts/check.sh`) stayed **green** while this failed.

## Root cause

Three facts compose:

1. `migrations_pg/0001_init.sql` is a **merged baseline**: one file that creates the whole
   schema. `MigratePG` records it by **filename** in `schema_migrations` and never re-applies it.
2. The test database `teleport_test` is **long-lived**. It was created while the baseline was
   being authored, from an *uncommitted* intermediate revision whose CHECK read
   `(metadata)::json IS NOT NULL`.
3. The file was then corrected to `CHECK (metadata IS JSON)` before commit, and
   `CREATE TABLE` (no `IF NOT EXISTS`) would have applied that correctly — but only to a
   database that had never recorded `0001_init.sql`. `teleport_test` had.

So the live database kept DDL that **no commit ever contained**:

| | metadata CHECK |
| --- | --- |
| live `teleport_test` | `CHECK (((metadata)::json IS NOT NULL))` |
| committed migration | `CHECK (metadata IS JSON)` |

Both committed revisions (`ab229ec`, `a58ae2d`) already say `IS JSON`, which is the proof
that the drift came from the working tree, not from git.

**The guards could not catch it because they compare files to files.**
`schema.pg.sql` vs `migrations_pg/*.sql` is a pure text comparison; a live database is not a
participant. A green guard therefore means "the snapshot matches the migrations", never
"the database matches the migrations".

Note the failure mode was *inverted* from the usual one: the stale schema is the **stricter**
one (a cast raises 22P02 instead of rejecting the row), so nothing silently accepted bad data.
The damage is to the **claim of verification** — "verified against the real database" was
true of a database that no longer existed.

## Fix

Rebuild the test database from the committed files and re-verify:

```bash
docker exec -e PGPASSWORD="$PW" postgres psql -U postgres \
  -c "DROP DATABASE IF EXISTS teleport_test;" \
  -c "CREATE DATABASE teleport_test OWNER teleport;"
TEST_PG_DSN='postgres://teleport:$PW@127.0.0.1:5432/teleport_test?sslmode=disable' \
  go test ./internal/store/ -count=1 -run TestPG
# → 8/8 PASS
```

The production-intent database `teleport` was still **empty** (no `schema_migrations`, no
tables), so nothing in P5/P6 is affected — but the same edit would have poisoned it
permanently once it had been migrated.

## Guard

**Structural, not a new assertion:** the P2 test harness creates a **fresh schema per test
from the embedded migrations** (see `.ai/decisions/2026-10-16-p2-test-strategy.md`). A schema
that is created and dropped inside one test cannot drift from the files that created it, so
this failure mode stops existing rather than being detected.

Until that harness lands, the rule is: **after editing `migrations_pg/*.sql`, drop and
recreate every database that has already recorded it** — including `teleport_test`. Editing a
merged baseline is not a migration.

Deliberately *not* the guard: a test that compares the live `reports_metadata_check` text to
the migration. It would pin one constraint and miss the next one.

## 追加（2026-10-16，P5 期间第二次遇到同族现象）

P4 交接文档把「环境干净」写成「`teleport_test` 只剩 `public`；生产意图库 `teleport` 仍 0 张表」。
P5 开始搬运时发现 **`teleport_test.public` 里躺着一整套完整的 schema**
（`reports`/`share_tokens`/`agent_keys`/`key_applications`/`key_renewals`/`schema_migrations`/`v_live_shares`，
`applied_at = 1792145288078` ≈ 当天 18:08，即 P4 会话期间由某次没带 `search_path` 的迁移跑出来的）。

所以那句话是**真的但读错了**：「只剩 `public`」说的是**只剩这一个 schema**，不是**这个 schema 是空的**。
这正好是「绿必须真的对应当前状态」的同一个教训——**判据要指向对象，不能指向对象的容器**。

已处理：把 `public` 里的那套对象逐一下掉（`DROP VIEW … CASCADE; DROP TABLE … CASCADE; DROP FUNCTION …`），
`teleport_test` 现在是 0 张表、只剩 `public`；同时删掉了 P5 手工跑留下的 `p5_scratch`。

**规则（追加）：**

- 说「环境干净」时必须给出**计数**（几张表、几个 schema），不能只给名字。
- 每次彩排/手工实验后用**自己的 schema**（`p5`、`p4`……）并确保退出时拆掉；
  带 `search_path` 的 DSN 是唯一安全的写法，不带就是把对象写进 `public`。
- 这不会影响 P5/P6：生产意图库 `teleport` 当时与现在都是 0 张表，而 P5 的彩排用的是
  `teleport_test` 里的临时 schema（用完即拆）。合并式基线的那条守卫（内容哈希 + 拒绝启动）
  仍然只在**真正建过库**的地方起作用——`public` 那套对象之所以无害，只是因为它没被任何人使用。
