# 守卫把数量写死：加了第二个迁移，守卫照样通过

**Severity:** 🟠 medium · **First hit:** 2026-10-10 · **Hits since:** 0

## Symptom

无报错、无警告，CI 全绿，检查器还打印了一行让人安心的成功信息：

```
✔ backend/internal/store/migrations/0001_init.sql and schema.sql are in sync
```

而真相是：`schema.sql` 与**实际会执行的 schema** 已经不一致了 —— 新加的
`0002_agent_keys.sql` 根本没有被任何人比对。

## Root cause

`scripts/check-schema-sync.mjs` 里写死了单个文件名：

```js
const MIGRATION = 'backend/internal/store/migrations/0001_init.sql';
```

这在只有**一个**迁移时是完全正确的。它的注释也写着「the migration embedded in
the Go backend」（单数）。问题是：

- 后端用 `//go:embed migrations/*.sql` **全部**迁移并发执行（`store.Migrate` 按文件名
  排序逐个应用）；
- 而守卫只看第一个。

所以加入 `0002` 之后，守卫从「守住一致性」退化成「守住 0001 的一致性」——
**新增迁移完全不受保护**。一个忘记同步 `schema.sql` 的未来迁移会静默通过，
直到有人拿 `schema.sql` 去建库、发现缺表。

**为什么容易漏**：脚本没坏，输出还是那句 ✔。守卫的失效不产生任何信号 ——
它只是**少守了一部分**。这类「覆盖范围悄悄缩小」的 bug 比「直接报错」危险得多，
因为没有任何东西提示你去看它。

**附带的反直觉现象**：真正把这个问题暴露出来的，是我**做对了**动作（把 0002 的 DDL
追加到 `schema.sql`）之后守卫反而**红了** —— 因为它拿 0001 去比一个含 0001+0002 的快照。
也就是说：在修正它之前，「正确地同步快照」会被守卫判为漂移。这提示了根因
（守卫的知识过时了），而不是快照错了。

## Fix

改为读取**整个目录**、按文件名排序后拼接，再与快照比对 —— 与 Go 端
`store.Migrate` 的应用顺序完全一致：

```js
migrationFiles = readdirSync(MIGRATIONS_DIR).filter((n) => n.endsWith('.sql')).sort();
migration = normalize(migrationFiles.map((n) => readFileSync(join(MIGRATIONS_DIR, n), 'utf8')).join('\n'));
```

并在成功信息里打印**它实际比对的文件列表与数量**，这样「守卫覆盖了哪些东西」
是看得见的，而不是要靠人去读脚本：

```
✔ 2 migration(s) (0001_init.sql, 0002_agent_keys.sql) are in sync with schema.sql
```

`scripts/check-schema-sync.mjs`

## Guard

修复后**做了反向验证**：临时把 `schema.sql` 恢复成只含 0001，守卫立即失败
（`✖ Schema drift detected`），再恢复。一个没有被证明「会失败」的守卫等于没有守卫。

同类修复：`TestMigrateIsIdempotent` 原本硬编码「恰好 1 条迁移记录」，加迁移即红。
改为与 `migrationFS` 里的文件数量比较 —— 同样是「别把数量写死」。

## 推广

> **凡是用「硬编码数量/单个名字」来表达「全部」的检查，都要问一句：加一个成员之后
> 它还会检查吗？**
>
> 具体到本仓：
> - 守卫要遍历目录，不要点名文件；
> - 断言要与**事实来源**比较（`migrationFS` 的内容、目录里的文件数），不要与常量比较；
> - 成功输出要列出它实际检查了什么，让覆盖范围可审计；
> - 每次新增成员后，**故意破坏一次**验证守卫仍会报警。
