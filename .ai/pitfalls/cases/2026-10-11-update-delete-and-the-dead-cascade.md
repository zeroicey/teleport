# 报告更新/删除：一条会「倒流」的时间戳，和一条可能从未生效的级联

**Status:** 🩹 已修（2026-10-11） · **Related:** `decisions/2026-10-11-report-update-and-delete.md`

实现「报告更新/删除」时踩到两个坑，都属于**「代码写了、但从未真正生效」**这一类 ——
和 `2026-10-10-renewal-unreachable-after-expiry.md` 是同一个家族。

---

## 坑 1：`updated_at` 会倒退到 `created_at` 之前（最多 999ms）

### 症状

更新一篇刚发布的报告后，分享页可能出现：

```
创建 / Created: 2026-10-11 00:51:33
更新 / Updated: 2026-10-11 00:51:32      ← 比创建时间还早一秒
```

而 `updated_at` 的**字面值**更奇怪：`1791651093000` vs `created_at = 1791651093897`
—— 差值 **-897ms**，且末尾是 `000`。

### 根因：一条只在「值没变」时才触发的触发器

schema 里有一条为更新而写的触发器：

```sql
CREATE TRIGGER trg_reports_touch_updated_at
AFTER UPDATE ON reports
FOR EACH ROW
WHEN NEW.updated_at = OLD.updated_at          -- ← 只在"调用方没改它"时触发
BEGIN
  UPDATE reports
     SET updated_at = CAST(strftime('%s','now') AS INTEGER) * 1000   -- ← 截断到整秒
   WHERE id = NEW.id;
END;
```

我最初把 `updated_at` 写成朴素的 `?`（Go 传 `time.Now().UnixMilli()`）：

```sql
updated_at = ?     -- ❌
```

**毫秒精度的 `now` 与已存值相等，是完全可能的** —— 任何"同一毫秒内两次写入"
都会命中：创建后立刻更新、一次请求内的连锁更新、测试里的快速循环。

一旦相等，`NEW.updated_at = OLD.updated_at` 成立 → **触发器启动** →
用 `strftime('%s','now') * 1000` 覆盖，而那个表达式**只精确到秒**。
于是毫秒部分被抹成 `000`，值可能落到**最多 999ms 之前**，即 `created_at` 之前。

### 修法

```sql
updated_at = MAX(?, updated_at + 1)     -- ✅
```

- `updated_at + 1` 保证**严格递增**，于是"值没变"永远不成立，触发器**永不触发**，
  那条秒级精度的赋值再也进不了数据。
- `MAX(now, ...)` 让系统时钟回拨也无法把列改小。

### 这个 bug 是怎么被发现的（比 bug 本身更值得记）

**不是 code review 发现的，是一次「反证」跑出来的。**

我写测试时出于"避免同毫秒导致断言无意义"的直觉，加了一句
`time.Sleep(3 * time.Millisecond)`。**那句话正好把 bug 藏起来了** ——
睡过 3ms 之后 `now != updated_at`，触发器不触发，测试全绿。

真正暴露它的是**反证**：我故意删掉「未知字段检查」让测试变红，
红出来的 JSON 响应体里带着 `created_at:1791651093897, updated_at:1791651093000`。
**我是在读一个"故意搞坏"的失败输出时，看见了另一个 bug。**

两条教训：

1. **为了让断言"稳定"而加的 sleep，常常是在移除被测条件。**
   这个测试本该覆盖"同毫秒更新"，而我用 sleep 把同毫秒**排除**了。
   正确的做法是让**实现**保证不变量（严格递增），测试**不加 sleep** ——
   现在 `TestUpdateReportKeepsUpdatedAtStrictlyIncreasing` 在紧循环里跑 5 次更新，
   一次都不睡。
2. **反证不只是确认测试非空，它还会把实现的真相打印出来。** 失败输出里的
   原始响应体，比通过时的绿色更值得读。

### 守卫

| 测试 | 断言 | 反证 |
| --- | --- | --- |
| `TestUpdateReportKeepsUpdatedAtStrictlyIncreasing`（store） | 连续 5 次同毫秒更新，`updated_at` 严格递增且 ≥ `created_at` | 改回 `updated_at = ?` → 红：`updated_at = 1791651158000 did not advance past 1791651158167` |
| `TestUpdateReportIsInPlaceAndVisibleThroughTheExistingLink`（api） | 更新后 `updated_at > created_at`，**且测试内不 sleep** | 同上 |

---

## 坑 2：`ON DELETE CASCADE` 可能是一条从未生效的声明

### 症状

`share_tokens` 建表时就写了：

```sql
report_id TEXT NOT NULL REFERENCES reports (id) ON DELETE CASCADE,
```

看起来"删报告自动删链接"是既有保证。**但 SQLite 默认不强制外键** ——
`PRAGMA foreign_keys` 默认 **OFF**。不开的话：

- 删报告**不会**删它的 share token；
- 那些行变成**永久孤儿**：任何 `JOIN reports` 的查询都看不到它们，
  于是**没有任何接口能发现或清理它们**；
- 分享链接仍然 404（因为 join 不到报告），所以**症状是隐形的** ——
  除了数据库里悄悄堆积的死行，什么都看不出来。

### 修法

不是改代码，是**确认它已经是开的**（`store.go`）：

```go
dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&..."
```

关键在 `_pragma=foreign_keys(1)` —— 它**按连接**生效，所以必须写在 DSN 上
（连接池里每条连接都要带上），而不是启动时执行一次 `PRAGMA`。

### 守卫

`TestDeleteReportRemovesItsShareTokens`（store）与
`TestDeleteReportCascadesToItsShareLinks`（api）**直接数 `share_tokens` 的行数**，
而不是通过 join 去推断 —— 因为 join 恰恰是看不见孤儿的那条路径。

反证：从 DSN 删掉 `&_pragma=foreign_keys(1)` → 红：
`orphaned share tokens after delete = 2, want 0 (ON DELETE CASCADE did not fire)`。

---

## 坑 3：一个"看起来在测 A、实际被 B 挡住"的空测试

`TestPatchRefusesFieldsItWouldOtherwiseSilentlyIgnore` 第一版每个 case 只发一个坏字段：

```
{"Content":"# nope"}
```

**反证时它没红** —— 因为服务端还有一条"空 patch → 400"的规则也在拒绝它，
而"未知字段 → 400"那条被我删掉后，请求仍然 400。
**测试绿着，却什么都没证明。**

修法：每个 case **再带一个合法字段**：

```
{"content":"# applied?","Content":"# nope"}
```

这样只有"未知字段检查"能拦住它；同时断言那个**合法字段也没有被应用**
（全有或全无，否则调用方能靠夹带合法字段绕过检查）。

修完之后反证才成立：删掉未知字段检查 → 红，且响应体里
`"content":"# applied?"` 清楚显示改动被静默应用了。

**教训：一个否定型断言（"返回 400"）必须排除"因为别的原因也返回 400"。**
多条校验规则并存时，每条规则都需要一个"只触发它自己"的用例。

---

## 一句话教训

**「代码写了」不等于「代码生效了」。** 触发器可能永不触发，外键可能从未强制，
测试可能被无关规则挡住。这三件事都**不会报错**，只会让行为与声明悄悄分叉 ——
所以每加一条声明式机制（触发器/外键/约束），都要有一条**直接读底层状态**的守卫。
