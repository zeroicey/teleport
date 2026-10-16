# gofmt 会把注释里的 `''` 改成弯引号（而旧闸门从不检查格式）

**发现方式**：给重写后的仓库补闸门时，第一次跑 `gofmt -l` 就报出 4 个文件不干净 ——
而这份代码**正在生产运行**。

## 两个独立的发现

### 1. 旧仓库的闸门里没有格式检查，也没有 vet

```
check:        pnpm run typecheck && pnpm run db:check-schema && pnpm run backend:test
backend:vet:  cd backend && go vet ./...        ← 存在，但不在 check 里
```

后果（都是无害但真实存在的）：一个错位的 map 字面量、一个测试函数头被压成
`func TestX(t *testing.T) {	s := newTestStore(t)`、一个文件没有结尾换行。
它们不影响行为，但**会污染重写期间每一份 diff** —— 一个被改动的文件同时混着
"格式化噪声"与"语义改动"，review 就没法只看语义了。

### 2. `gofmt` 会重排 doc comment，并把 `''` 变成 `”`

最小复现（Go 1.27）：

```bash
printf 'package p\n\n// Value is %s%s when empty.\nfunc F() {}\n' "'" "'" > g.go
gofmt g.go
# package p
#
# // Value is ” when empty.      ← 两个 ASCII 撇号变成了 U+201D
# func F() {}
```

这是 Go 1.19 起 doc comment 重排的一部分（它把注释当 godoc 语法重新渲染，
顺手做了"智能引号"）。**gofmt 通常不动注释内容，所以这一条很容易被误判成 diff 工具出错** ——
我第一次看到 `-` 行是 `''`、`+` 行是 `”` 时，用的就是"gofmt 不改注释"这条（正确但不完整的）先验，
差点把它当成显示问题。

生产代码里被影响的那句注释原本写的是「whose owner_key_id is `''`」（指空字符串）。
重排后它会变成「whose owner_key_id is `”`」—— 一句技术注释被改成了弯引号，
**语义上更容易误读**（看起来像一个具体的、带方向的符号，而不是空串）。

**修法**：改措辞，而不是让 gofmt 改字符。

```go
// published by the root credential, whose owner_key_id is the empty string.
```

## 守卫

- `scripts/check.sh` 把 `gofmt -l` 与 `go vet` 放进闸门（旧仓库两样都没有）。
- 那道检查的失败信息里**直接写了这条弯引号的坑与修法**，因为下一个撞上它的人
  最可能的反应也是"这 diff 是不是坏了"。

## 教训

**「工具不会做那件事」的先验要用最小复现去校验，而不是靠它推理。** 我本来会把它归类成
diff 显示问题而放过 —— 真正让我停下来的是那句注释里的 `''` 出现在 `-` 行、
而 `”` 出现在 `+` 行，方向不对：gofmt 只可能把原文变成格式化结果，不可能反过来。

顺带一条更普遍的：**闸门少一项，漂移就会累积在被检查不到的地方**。这次是格式；
同一份检查表里还暴露出"agentkey 的三个派生标签零测试覆盖"（见
`frozen-contract-with-no-test.md`）。两个都是同一种失败：没人检查的东西，会安静地变坏。
