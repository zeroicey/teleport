# SPA 兜底会吞掉未知 `/api/*`，让 404 变成 HTML

**Severity:** 🟠 medium · **First hit:** 2026-10-09 · **Hits since:** 1（已加回归测试）

## Symptom

前端 SPA 接管了前缀根路径的兜底（返回 `index.html`）之后，一个未知的
`/api/nope` 会**匹配到 SPA 兜底**，于是 XHR 客户端收到一个 HTML 页面。

实际报错是 `response.json()` 抛解析错误，用户看到的是「服务器返回了非预期的响应」，
而**不是真实的 404** —— 排查方向被彻底带偏。

## Root cause

`net/http` ServeMux 按**最长模式**匹配。SPA 兜底注册在 `p + "/"`，
未知的 `/api/*` 比它更长就会被兜底吃掉。没有更具体的 `/api/` 模式时，
任何未知 API 路径都退化成 HTML。

## Fix

给 `p+"/api/"` 与 `p+"/s/"` 各注册一个**更具体的**兜底 handler，返回 JSON 404：

```go
mux.HandleFunc(p+"/api/", s.handleAPINotFound)   // JSON 封套
mux.HandleFunc(p+"/s/", s.handleShareNotFound)   // 同样不落 SPA
mux.HandleFunc(p+"/", s.handleSPA)               // 兜底最后
```

由于 `net/http` 优先匹配更长的模式，`/api/` 与 `/s/` 的子路径不会再落到 SPA。

## Guard

**`TestUnknownRouteWithSPAStaysJSON`** 就是这条的回归测试：把
`mux.Handle(p+"/api/", root)` 与 `p+"/s/"` 两行删掉，它会**立刻失败**。

写这条测试的价值在于它锁的是**顺序与具体性**，而不是某个 handler 的返回值 ——
这正是"重构时会静默破坏"的那一类性质。
