# 校验器和它校验的代码共用同一个转换函数，于是错误互相抵消

**Severity:** 🔴 high · **First hit:** 2026-10-16（P5）· **Hits since:** 0

## Symptom

搬运把源里一个**超范围整数**写错了，而校验器报 **GREEN**。

实测（不是推理）：

```text
源（SQLite，INTEGER 列，值是 2^63）
  sqlite3> SELECT quote(created_at), typeof(created_at) FROM key_renewals WHERE id='n-big';
  9.2233720368547758e+18|real          -- INTEGER 装不下 → SQLite 用 REAL 存储

目标（PostgreSQL，BIGINT 列）
  teleport=# SELECT created_at FROM key_renewals WHERE id='n-big';
  -9223372036854775808                  -- 应该是 9223372036854775808

导入工具自己的输出
  import committed: 7 row(s) in one transaction     exit=0
  verify: 15 passed, 0 failed                       exit=0   ← 全绿
```

`row_equality[...]`、`db_rendered_text[...]` 之外的所有检查都通过了。**唯一能发现它的那条检查
当时还不存在。**

## Root cause

两层，第二层才是要害。

**第一层（表层）：`int64(float64)` 对超范围值没有定义行为，本平台回绕到 `math.MinInt64`。**

```go
func integral(f float64, column string) (int64, error) {
    if f != math.Trunc(f) { ... }        // 只挡了小数
    return int64(f), nil                 // f = 9.2233720368547758e18 → MinInt64
}
```
来源是 SQLite 的**动态类型**：`INTEGER` 亲和性的列装不下 2^63，就按 REAL 存，
`sqlite3 -json` 于是打印 `9.2233720368547758e+18`（指数形式）。`json.Number.Int64()` 对带 `e`
的字面量直接失败，代码走 `Float64()` → `integral()` → 回绕。

**第二层（要害）：校验器和搬运工具用同一个 `convertValue`，所以两侧错得一模一样。**

```text
转换后的源值 = int64(9.2233720368547758e18) = -9223372036854775808
转换后的目标值 = 从 PG 读回来的          = -9223372036854775808
→ row_equality 判定「相等」
```

这是我在这份工作上第二次踩到同一个形状：

1. 负向对照那次（`controls-that-tamper-the-reference.md`）：把源和源自己比 → 永远相等；
2. 这次：把「源的转换结果」和「目标的转换结果」比，而两者都出自同一个转换函数 → 转换的错
   在两侧同时发生、同时抵消。

**共同点：检查的两个操作数不是独立的。**「A 与 B 一致」只有在 A、B 由**不同机制**产生时才有
信息量。

## Fix

**根因**（`import.go` 的 `integral`）：加范围检查。2^63 在 float64 里可精确表示，所以它是
**开区间上界**；-2^63 也可精确表示且是合法 int64，所以它是**闭区间下界**。

```go
const maxInt64Exclusive = float64(1 << 63)
const minInt64Inclusive = -float64(1 << 63)
if f >= maxInt64Exclusive || f < minInt64Inclusive {
    return 0, fmt.Errorf("column %s: %v does not fit in a 64-bit integer ... 转换会静默回绕", column, f)
}
```

**结构性**（`verify.go` 的 `checkDatabaseRenderedText`，新增 5 项检查）：让**两个数据库各自**
把值渲染成文本，再比字符串，**全程不经过 `convertValue`**：

```sql
-- 源（sqlite3，由 SQLite 渲染）
SELECT CAST(id AS TEXT) AS k, CAST(created_at AS TEXT), ... FROM t;
-- 目标（psql/pgx，由 PostgreSQL 渲染）
SELECT id::text, created_at::text, ... FROM t;
```

于是同一个坏值会渲成 `9.2233720368547758e+18`（源）vs `-9223372036854775808`（目标）→ **红**。

**第一版这里写错了，而且正是「把假设当事实」的又一次**：当时写的是「SQLite 的 `CAST AS TEXT`
用 15 位有效数字」。实测（`sqlite3 :memory:`）：
`CAST(0.30000000000000004 AS TEXT)` → `0.30000000000000004`，`CAST(5e-324 AS TEXT)` →
`4.9406564584124654e-324`，`printf('%!.17g', 0.1)` → `0.1` —— SQLite 的 REAL→TEXT 是**最短往返**的，
不是 15 位；`sqlite3 -json` 也一样。所以「渲染会丢位数」这个担心在**这个 shell 版本上不成立**，
但**它无法被本工具发现**：比较两侧都走同一条 JSON float 管道，真的丢了位数也会一起丢、一起相等。

修法因此改成**堵掉那条管道**，而不是去比渲染：`source.go` 里 REAL 列不再作为 JSON number 读，
改读 SQLite 自己的 `printf('%!.17g', col)`（`CASE WHEN typeof(col)='real'` 才转，否则原样——
否则 `printf` 会把 TEXT 强转成数字，`'abc'` → `0.0`，那是**自己引入**的静默损坏）。
于是 REAL 的精度保证是结构性的：JSON float 路径不再参与，`Float64()` 只做 ParseFloat（精确），
`integral` 的范围拒绝仍在。夹具里现在也有一个真正需要 17 位的值（`0.30000000000000004`）,
`TestImportPreservesRealPrecision` 用 PG 的 `float8send` 比 IEEE754 字节。

`db_rendered_text` 也因此改名并升级为 **`db_hex_bytes`**：比的是**字节**
（源 `lower(hex(col))` vs 目标 `lower(encode(convert_to(col::text,'UTF8'),'hex'))`），
因为文本渲染本身也要经过 `sqlite3 -json` + `encoding/json`，而 Go 的 JSON 解码器会把**非法 UTF-8
替换成 U+FFFD** —— 那条路上一侧的替换会在另一侧同样发生，**比文本永远发现不了**。字节能发现。
（实测真源：**0 个**文本格子含非法 UTF-8，所以这是个**潜在**洞而非活洞 —— 而这正是最便宜关闭它的时候。）
只比整数列与文本列，REAL 仍然排除：`hex()` 对 REAL 给的是它的**文本**字节
（实测 `hex(0.1)` = `302E31`，即 "0.1"，**不是** IEEE754 的 `3fb999999999999a`），
而两库对整数的渲染本来就不同（`720.0` vs `720`），比它们只会产生假警报。

## Guard

- `TestConvertValueRefusesIntegersOutsideInt64`：7 个边界用例（int64 最大/最小**必须通过**，
  2^63 的三种写法、±1e30 **必须报错**）。**反证过**：把范围检查 `if false &&` 掉，
  5 个子用例立刻变红，并把回绕后的 `-9223372036854775808` 打出来。
- `TestImportRefusesAValueThatWouldWrap`：端到端——把夹具里一个值改成 2^63，
  要求导入**拒绝**（错误里要点出 `does not fit` 与列名），且目标**一行都没提交**。
  **反证过**（同上一次反证覆盖）。这条比单元测试重要：它证明的是「停下来了、没写坏值」。
- `TestCompareRenderedText`：纯函数层面的反证，含「NULL 不等于空字符串」「缺行/多行要报」。
- `scripts/verify-p5.sh` 的真源彩排现在有 **20 项**检查，其中 5 项是
  `db_rendered_text[...]`（真源上共 157 个格子）。

**规则（三条，都可以机械检查）：**

1. **校验器不许复用被测代码的转换/解析函数。** 如果两边都要「把 X 变成 Y」，就必须有第三个
   独立实现或由外部系统（这里是数据库自己）来产生其中一侧。
2. **浮点转整数必须显式查范围**，不能只查小数部分：Go 的 `int64(float64)` 超范围时结果未定义。
3. 写「A 与 B 一致」时，先回答：**A 和 B 分别是谁产生的？** 若答案是「同一个函数」，
   这条检查等于重言式。
