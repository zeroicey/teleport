# 两个「守卫其实没在守」的发现（都靠反证才暴露）

这次是新写 PG schema 的 P1 阶段。我按规矩给每个转换都配了守卫，然后**逐个把转换改坏，
看守卫是否变红** —— 5 次反证里有 4 次第一轮没红，每一条都对应一个真问题。

## 发现 A：否定型断言太弱，两种实现都能通过

**症状**：把 `CHECK (metadata IS JSON)` 换成 `CHECK ((metadata)::json IS NOT NULL)`，
`TestPGMetadataCheckRejectsInvalidJSON` **仍然全绿**。

**根因**：测试只断言了「插入非法 JSON 时出错」：

```go
if err := insert(`not json at all`); err == nil { t.Fatal("invalid JSON was accepted") }
```

而 `CAST` 版本的 CHECK 会**抛异常**（`22P02 invalid input syntax for type json`），
那也是 `err != nil`。所以两种翻译给测试的答案一模一样 —— 测试没有区分能力。

**这正是本仓库早就记录过的那条 pitfall**：否定型断言必须排除「因为别的原因也返回了同样的
结果」。`json_valid()` 的契约是「**返回 false，拒绝这一行**」；`CAST` 的契约是「**抛异常**」。
它们是不同的契约，测试必须断言**是哪一个**：

```go
var pgErr *pgconn.PgError
if !errors.As(err, &pgErr) || pgErr.Code != "23514" {   // 23514 = check_violation
    t.Fatalf("... want a CHECK violation 23514")
}
```

改完后反证立刻抓住：`SQLSTATE 22P02, want a CHECK violation 23514`。

## 发现 B：`go test` 缓存会让读取包外文件的守卫静默跳过

**症状**：往 `schema.pg.sql` 里追加一行（模拟"改了迁移但忘了重跑生成脚本"），
`TestPGSnapshotMatchesMigrations` **仍然报告 PASS**。

**根因**：**Go 的测试缓存只看包自己的输入**（源文件、依赖、环境、标志）。
这个测试读的是 `../../../schema.pg.sql` —— 包**外**的文件。Go 无从知道它变了，
于是直接返回上一次的 PASS，测试体根本没跑。

**后果**：一个守卫可能长期处于「从未真正执行」的状态，而每个人看到的都是绿色。
这比没有守卫更坏，因为它提供的是**虚假的保证**。

**修法（两条一起）**：

1. `scripts/check.sh` 里加一道**shell 比较**：把生成器的输出写到临时文件，与提交的快照
   `cmp`。shell 比较不会被任何缓存跳过。这是主守卫。
2. 闸门里的 `go test` 改成 `go test ./... -count=1`，并写明原因 —— 凡是读包外文件的守卫
   都受这个缓存影响。

Go 测试保留（新克隆的仓库缓存是冷的，它会真正执行），但不再依赖它作为唯一防线。

## 顺带：反证脚本自己也会骗人

第一轮 5 次反证里有 3 次报「守卫没抓住」，其中有 **2 次其实是我的替换根本没匹配上**
（reports 表里 `updated_at` 与 `owner_key_id` 之间有注释行，我拼的 pattern 匹配不到），
测试自然不变红。

**所以反证脚本必须先断言「改动真的生效」再跑测试**：

```python
assert s.count(old) == 1, "pattern matched 0 times — the mutation would be a no-op"
```

否则「守卫没抓住」这个结论本身可能是假的，而你据此去"修"一个根本没问题的守卫。

## 教训

1. **断言「失败了」是不够的，要断言「因为什么失败」。** 错误码 / SQLSTATE / 具体消息。
2. **守卫必须被反证过才算存在。** 这次 5 条里有 2 条在反证前是装饰品。
3. **反证脚本自己也要被验证**：先证明变异生效，再解读结果。
4. **任何读取「包/模块之外」的检查都要怀疑缓存**，或者干脆放在不可缓存的地方（shell/CI 脚本）。
