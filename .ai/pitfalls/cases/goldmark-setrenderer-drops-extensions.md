# goldmark 的 `SetRenderer` 会静默丢掉已注册的扩展

**Severity:** 🟠 medium（静默错误输出） · **First hit:** 2026-10-09 · **Hits since:** 1

## Symptom

表格、删除线、自动链接**全部不渲染**，但渲染器本身"工作正常"：Markdown 照样出 HTML，
没有任何报错，只是扩展语法退化成纯文本。测试若不专门断言表格/删除线，就完全看不出来。

## Root cause

先 `goldmark.New(goldmark.WithExtensions(Table, Strikethrough, Linkify, ...))`，
再调用自定义渲染器时用了 `SetRenderer(...)`。`SetRenderer` 会**替换**掉 goldmark
在应用 extensions 时装配好的默认渲染器 —— 于是扩展的渲染逻辑一起没了。

## Fix

注册自定义 node renderer 时用 **renderer option**，不要 `SetRenderer`：

```go
goldmark.New(
    goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
    goldmark.WithParserOptions(parser.WithAutoHeadingID()),
    goldmark.WithRendererOptions(
        html.WithXHTML(),
        renderer.WithNodeRenderers(util.Prioritized(customRenderer, 100)),
    ),
)
```

配套的两个坑（同一次排查中发现）：

- `WithHardWraps` **不接收参数**（不是 `WithHardWraps(true)`）。
- 代码围栏取语言时 `strings.Fields(info)` 可能为空 → 未加长度保护会
  `panic: index out of range [0] with length 0`。

## Guard

`internal/markdown` 的测试必须**逐项断言扩展真的生效**（表格 → `<table>`、
删除线 → `<del>`、自动链接 → `<a href>`），而不只是断言"渲染出了 HTML"。
只断言输出非空是这条陷阱的温床。

## 关联安全约束

自定义渲染器不得引入 `html.WithUnsafe()`：分享页的 XSS 安全正建立在
「原始 HTML 全部转义」之上（`.ai/ARCHITECTURE.md` §5.5）。
