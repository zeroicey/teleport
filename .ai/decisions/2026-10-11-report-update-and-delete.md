# 报告的更新与删除

**Status:** ✅ ACCEPTED · **Date:** 2026-10-11 · **Deciders:** 用户（"继续做报告更新删除这些操作吧"） · **Supersedes:** none

## 背景

平台此前只有「发」和「撤销链接」，没有「改」和「删」。后果：

- 报告发出去之后**改不了一个字**。标题写错、正文有个笔误、分类选错，
  唯一办法是**重发一篇 + 撤销旧链接** —— 但旧链接可能已经发给别人了，
  撤销会让对方看到 404，而重发会得到**新的 report id 和新链接**。
- 报告**删不掉**。发错的、临时性的、含敏感信息的报告永久留在面板列表里，
  而且它那条分享链接只要没过期就一直有效。

用户诉求原话：「报告更新删除这些操作」。

## 关键发现：schema 早就为这两件事留好了位置

这不是新设计，而是**把已经声明的意图接通**（和 2026-10-10 的续期宽限同一类问题）。

### 1. 更新：`updated_at` + 触发器

```sql
created_at  INTEGER NOT NULL,
updated_at  INTEGER NOT NULL
...
--  Trigger — keep reports.updated_at honest without trusting the caller.
CREATE TRIGGER trg_reports_touch_updated_at
AFTER UPDATE ON reports
FOR EACH ROW
WHEN NEW.updated_at = OLD.updated_at
BEGIN
  UPDATE reports SET updated_at = CAST(strftime('%s','now') AS INTEGER) * 1000 WHERE id = NEW.id;
END;
```

**`updated_at` 在「报告不可变」的前提下没有任何意义。** 一个只在插入时被写入、
之后再无用途的列，加上一条专门为「别人更新它」而写的触发器，以及
`views/sharepage.go` 里**已经渲染好的**「更新 / Updated: 」字段 ——
这三件事合起来只指向一个结论：**报告本来就是设计成可以原地更新的**。
（`sharepage.go` 那一行至今渲染的 `updated_at` 恒等于 `created_at`，
这正是"实现了但从未被触发"的特征。）

### 2. 删除：`ON DELETE CASCADE`

```sql
report_id TEXT NOT NULL REFERENCES reports (id) ON DELETE CASCADE,
```

级联删除是**建表时就写下的**：删报告就该连带删掉它的全部分享链接。
且 `store.go` 的 DSN 确实带 `_pragma=foreign_keys(1)`（SQLite 默认**关闭**外键，
不开的话这条 cascade 是**死代码**）—— 已实测确认生效，见下方守卫。

## 裁决

| # | 问题 | 裁决 | 依据 |
| --- | --- | --- | --- |
| D1 | 非归属者更新/删除 | **404**，不是 403 | 与既有 `GET /api/reports/{id}`、`revoke` 完全一致：不确认「存在但不是你的」 |
| D2 | 更新语义 | **原地更新**：同一个 `report.id`、同一条分享链接，`updated_at` 前移 | schema 的触发器与 `updated_at` 列；见上 |
| D3 | 删除是否级联 | **是**，连带删掉该报告的全部分享链接 | 建表时的 `ON DELETE CASCADE` |
| D4 | 面板能否操作别人的报告 | **能**（god-view），root 同样能 | 与既有 `ownerCanAccess` 的 root/面板旁路一致 |
| D5 | `PATCH` 语义 | **局部更新**；未知字段 → **400**；空 patch → **400**；`metadata` **整体替换** | 见下方「三条我自行拍板的设计」 |
| D6 | 删除的响应 | **200 + 封套**（沿用 `DELETE /api/admin/shares/{token}` 的约定，不用 204） | 仓库既有约定 |

**归属永远不可转移**：`owner_key_id` 不在可更新字段白名单里，
`PATCH` 里出现它一律 **400**。否则一把密钥就能把自己的报告"送"给别人，
或者把别人的报告认领到自己名下。

## 三条我自行拍板的设计（用户未逐条裁决，可随时推翻）

### 1. 未知字段一律 400，而不是静默忽略

```
PATCH /api/reports/{id}  {"Content": "新正文"}     ← 大写 C
```

若按"忽略未知字段"实现，这个请求会返回 **200**，而正文**一个字都没变**。
调用方（很可能是 AI agent）会认为更新成功，然后把这个结论告诉用户。
**这是一个静默的错误结果，比报错坏得多。** 而仓库里已经有一个高度相关的
已知陷阱：请求体 camelCase、响应 snake_case，用错命名风格是**最常见的错误**。

所以：字段名写错 → **400 并列出可更新字段**。这与
`RequireAgentAllowExpired` 的取向一致 —— **默认严格，让错误可见**。

### 2. 空 patch 是 400，不是 no-op 200

`PATCH {}` 若返回 200，`updated_at` 仍会被前移，于是分享页的
「更新 / Updated」时间变了、内容没变 —— 页面在对读者**说谎**。
所以空 patch 直接 400。

### 3. `metadata` 整体替换，不合并

合并语义无法表达"删掉某个键"（`null` 到底是删除还是设为 null？）。
整体替换是唯一无歧义的选择：要清空就发 `{}`。

## 后果

- **变容易**：改一个笔误不必重发、不必作废已发出的链接。
- **必须保持为真**：
  - 非归属者更新/删除 → 404（且**在任何写入之前**判定，否则会泄漏存在性）；
  - `owner_key_id` 不可通过任何请求体字段改变；
  - 删除必须级联，且不能留下孤儿 share token；
  - 未知字段/空 patch → 400。
- **代价与风险（重要，说给使用者听）**：
  - **更新会改变已分享链接的内容**。这不是缺陷，是"更新"的定义 ——
    但如果链接已经发给外部读者，对方看到的就不再是你当初发的那一版。
    分享页会显示「更新 / Updated」时间，读者能看出它变过。
    若需要"发出即冻结"，那应当**重发一篇**而不是更新。
  - **删除不可逆**，且会**立刻**让所有已发出的链接变成 404（连带删除）。
    前端必须二次确认并说明这一点。
  - 没有软删除、没有版本历史。schema 里既没有 `deleted_at` 也没有版本表，
    加它们是另一个量级的改动；当前需求不需要。

## 刻意不做

- **不做软删除 / 回收站**：需要 `deleted_at` + 所有查询加过滤 + 分享链接语义
  重新定义，成本远超收益。
- **不做版本历史**：同上；而且已有"重发一篇"作为天然替代。
- **不做"更新时保留旧链接指向旧内容"**：那等于版本化，见上。
- **不把 `owner_key_id` 变成可更新字段**：归属是安全边界，不是数据字段。

## 验证

`pnpm run check` / `backend:vet` / `go test -race` 全绿，并逐条反证（见
`pitfalls/cases/2026-10-11-update-delete-and-the-dead-cascade.md`）：
把归属检查从"写入前"挪到"写入后"→ 测试红；从 `DELETE` 去掉 `PRAGMA foreign_keys`
→ 孤儿 token 测试红；让未知字段被静默忽略 → 对应测试红。
