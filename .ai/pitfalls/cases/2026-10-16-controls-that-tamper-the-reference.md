# 负向对照改错了那一侧：把源文件和它自己比，永远相等

**Severity:** 🟠 medium · **First hit:** 2026-10-16（P5）· **Hits since:** 0

## Symptom

P5 彩排脚本的负向对照（「验证脚本必须先被证明会变红」）第一次跑出来是这样：

```text
==> negative control: a tampered copy must fail verification, by name
  tampered: 2 revoked link(s) made active, 2 permanent link(s) given an expiry

ERROR: verification PASSED on a tampered target. The checks cannot fail, so the green run above
means nothing. Tampered copy: /tmp/tmp.Ofx9f0yyIc/tampered.db
```

对照脚本自己否掉了自己——**这是它的正确行为**：它发现了一件真事，只是那件事说的是脚本写错了，
不是产品坏了。

## Root cause

我把「篡改」施加在**源文件**上，然后：
1. 用这个被篡改的文件当**目标**灌进 PG；
2. 又拿**同一个被篡改的文件**当**源**去 `verify`。

`verify` 的语义是「目标 vs 源」。源和目标成了同一份数据，于是它当然全绿——
**篡改被自己抹平了**。

这个坑的形状值得记下来，因为它不是「忘了断言」，而是**断言的对象错了**：

> 一个「A 与 B 一致」的检查，只有在 A、B 是两个独立对象时才可能失败。
> 篡改 `A` 之后又让 `B := A`，就把检查变成了重言式。

同一族里其它必须避免的写法：`verify(源, 源)`、`import(篡改后的源)` 后
`verify(篡改后的源)`、把断言写成「和它刚写进去的值相同」。

## Fix

改成**篡改目标、参照仍是原源**：

```bash
# 1. 先把原始源忠实导入 NEG_SCHEMA
"$TOOL" import -source "$SOURCE" -dsn "$(DSN_FOR "$NEG_SCHEMA")"
# 2. 在目标里破坏两个性质（用 psql，不碰源文件）
revoke_tamper=$(psql_scoped "$NEG_SCHEMA" "WITH u AS (UPDATE share_tokens SET is_active=1 WHERE is_active=0 RETURNING 1) SELECT count(*) FROM u")
# 3. 参照没有变：还是原始 $SOURCE
"$TOOL" verify -source "$SOURCE" -dsn "$(DSN_FOR "$NEG_SCHEMA")"   # 必须非 0
```

## Guard

两层，都在 `scripts/verify-p5.sh` 与 `internal/sqlitemigrate/import_pg_test.go` 里：

1. **变异必须被证明真的落地了。** `WITH u AS (UPDATE … RETURNING 1) SELECT count(*) FROM u`
   返回改动行数，`>= 1` 才继续。改不动任何行的对照是空对照（同族的空对照：过滤器没匹配、
   变量是空的、`grep -q ""`）。同一个守卫在 Go 测试里是
   `TestVerifyDetectsEachKindOfTamper` 的 15 个用例——它们在**同一个事务性 schema** 里先忠实导入、
   再篡改目标、再要求对应检查变红。
2. **不能只断言「它失败了」，要断言「它按名字失败」。** `grep -q "FAIL revoked_links_stay_404"`
   等：因为「因为别的原因失败」不是关于这两项检查的证据。这条也来自
   `.ai/pitfalls/cases/2026-10-16-verification-that-cannot-fail.md`。
3. **每个检查名字都必须有对应的篡改用例，而且这件事要**机械地**核一遍。**
   自查时把 `verify.go` 里 `v.add(...)` 的名字列出来，再去 `import_pg_test.go` 的
   `wantCheck` 列表里找——`ownership_boundary_unchanged` 是**后来才加**的一项，
   当时没有任何篡改用例，也就是说它从来没被证明会红过。已补两个（root 报告被改归某把密钥、
   密钥的报告被改成 root 报告）。这不是理论风险：**「新加的检查」是这类漏洞的高发区**，
   因为它天然没有历史对照。
4. **空表上的「逐行相等」是空检查，必须在输出里说出来。** 真源里 `key_renewals` 是 0 行，
   所以 `row_equality[key_renewals]` 永远绿——它只能证明「两边都空」（那个由计数检查负责）。
   现在这一行的 detail 会直接写 `0 rows — … this check compared nothing`，
   让空绿在日志里可见，而不是伪装成「每一行都一致」。

**规则：写「两个东西一致」的检查时，先说清楚这两个东西**分别**是谁，
以及什么样的输入会让它们**分别**变错。**

> **同一形状的第二次出现（同一天，更严重）**：搬运的**校验器**复用了搬运工具的
> `convertValue`，于是超范围整数的回绕在两侧同时发生、同时抵消，校验报绿而库里是坏值。
> 见 `.ai/pitfalls/cases/2026-10-16-verifier-shares-the-conversion-it-checks.md`。
> 两次的根因是同一句话：**检查的两个操作数必须由不同机制产生。**
