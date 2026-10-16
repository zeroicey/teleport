# 两边的 `schema_migrations` 不是同一张表

**Severity:** 🟡 low · **First hit:** 2026-10-16（P5）· **Hits since:** 0

## Symptom

P5 彩排脚本里这一行永远为假，于是刚跑完一次**正确**的导入就被判为失败：

```bash
tgt_migrations="$(psql_scoped "$SCHEMA" \
  "SELECT count(*) FROM schema_migrations WHERE version = '0001_init' AND checksum <> ''")"
# → 0
# ERROR: schema_migrations has no checksummed 0001_init row;
#        the schema was not built by the application's migrator
```

真库里的值是 **`0001_init.sql`**：

```text
version      | length(checksum)
0001_init.sql| 64
```

## Root cause

两边的「迁移记录」表取了同名的两个东西：

| | 旧（SQLite） | 新（PostgreSQL） |
| --- | --- | --- |
| 记录的值 | `0001_init` | `0001_init.sql` |
| 决定者 | 旧迁移器自己拼的版本名 | `MigratePG` 把**文件名**当作版本（`pgMigrationNames()` 保留 `.sql`，因为文件名序就是应用序） |
| 额外列 | 无 | `checksum`（sha256，P2.2b 加的） |

看代码时很容易以为「迁移版本」是同一个字符串：两边都叫 `schema_migrations`、
都有一列叫 `version`、都只记得住自己那一种拼法。我的检查就是这样写错的。

同一族的第二个陷阱：**`version` 的拼法差异不能被当成漂移**。搬运**不应该**比较这两张表，
也不应该把旧库的 `version` 拷进新库 —— 它们各自描述各自的迁移器，唯一的关系是
「旧库已到 `0002_agent_keys`」意味着「新基线是这两者的合并结果」。所以
`sqlitemigrate` 只校验**源**的版本集合（`KnownSourceMigrations`），对目标的
`schema_migrations` 只断言「有一行、且 `checksum` 非空」，不比对字符串。

## Fix

`scripts/verify-p5.sh` 里改成 `version = '0001_init.sql'`，并把「这个字符串是文件名、
不是旧库里的那个」写进注释——因为它不是一眼能看出来的东西。

## Guard

- 该行的语义是**「schema 是应用迁移器建的，不是手搓的」**：`checksum <> ''` 才说明
  `MigratePG` 写过这一行（`checksum` 列是 P2.2b 加的回填列，旧迁移器建的库会是空串）。
- `internal/store/pg_test.go` 的 `TestPGMigrationsApplyAndAreIdempotent` 已经钉住
  「记录的值必须看起来像文件名」（`strings.HasSuffix(version, ".sql")`）——这条测试本该让我
  先想到拼法问题。
- 规则（写下来以免再犯）：**跨旧/新两侧的检查，不要把同名的 bookkeeping 表当成同一种东西；
  先打印一次它的真实内容再写断言。**
