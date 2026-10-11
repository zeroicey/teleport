# 分享页里的报告 iframe：三处「默认值」会静默地把它挡住

**Status:** 🩹 已修（2026-10-11） · **Related:** `decisions/2026-10-11-html-report-rendering.md`

把 `format: "html"` 报告放进分享页的 iframe 时，有三处**继承来的默认值**会让这个
frame 什么都不显示 —— 页面 200、后端日志 0 error、单元测试全绿，只有浏览器控制台
和一个空白框。三条都属于「局部按需放宽，但默认值在别处」这个家族。

---

## 坑 1：`httpx.SecurityHeaders` 给**每个**响应塞 `X-Frame-Options: DENY`

### 症状

分享页正常返回，`<iframe src=".../s/{token}/html">` 也在 HTML 里，但框内空白。
后端访问日志显示那个 URL **200**、body 字节数正确。

### 根因

全局中间件（`httpx/middleware.go` 的 `SecurityHeaders`）是无条件下发的：

```go
h.Set("X-Frame-Options", "DENY")
```

它写在**所有**响应上，包括那个「注定要被同源页面框住」的子文档。
浏览器看到 `DENY` 就直接不渲染，连请求算不算成功都不重要。

### 修法

子文档处理器显式删除它，并用 CSP 补回同等强度的限制：

```go
h.Del("X-Frame-Options")
h.Set("Content-Security-Policy", views.HTMLReportContentSecurityPolicy) // 内含 frame-ancestors 'self'
```

**守卫**：`TestHTMLReportRendersVerbatim` 的子测试
「frame is not blocked by X-Frame-Options」同时断言**两件事**：
子文档没有 `X-Frame-Options`，而分享页**仍然**是 `DENY`。
只断言前者的话，把中间件整个删掉也能过。

---

## 坑 2：`default-src 'none'` 会让 `frame-src` 回退成 `'none'`

### 症状

CSP 里没写 `frame-src` 时，iframe 被 CSP 拦下（控制台报
`Refused to frame ... because it violates ... default-src 'none'`）。

### 根因

`frame-src` 是**回退到 `default-src`** 的指令之一。分享页基线是
`default-src 'none'`，所以「没写 frame-src」等于「禁止一切 frame」——
而这条在加 iframe 之前是**刻意**的（`sharepage.go` 的老注释甚至写着
「deliberately no frame-src, so no iframe can be created」）。注释和代码
必须一起改，否则下一个人会以为那是笔误。

### 修法

`views.ContentSecurityPolicy` 增加 `frame-src 'self'`，只允许同源 frame。

**守卫**：同一条测试断言外层 CSP 含 `frame-src 'self'`，且
`script-src` 仍是 `'self'`（**没有**因为要跑报告脚本而放宽外层）。
另外 `router_test.go` 有一条「响应头整串等于 `views.ContentSecurityPolicy`」的断言，
防止这里被复制成第二份而漂移。

---

## 坑 3：`/s/` 是一条 JSON-404 兜底，未注册的子路径会回 JSON

### 症状（未踩到，靠先读路由避开）

`/s/{token}/html` 若没注册，会落到 `mux.Handle(p+"/s/", root)`，
iframe 里显示一个 JSON `{"ok":false,...}` 文本。

### 根因

`/api/` 与 `/s/` 各有一条更具体的兜底，保证未知路径返回 JSON 封套而不是 SPA 的
HTML（见 `spa-fallback-swallows-api-404.md`）。这条兜底对**新增的子路径同样生效**，
所以任何 `/s/...` 下的新端点都必须显式注册。

### 修法

在分享页路由旁显式注册，并留注释说明为什么不能省。

---

## 顺带记录：为什么没用 `<iframe srcdoc>`

第一版设计想把报告正文塞进 `srcdoc`。**`srcdoc` 文档会继承父页面的 CSP**，
于是「子文档有自己的宽松策略」这个前提不成立 —— 报告的内联脚本会被父页的
`script-src 'self'` 挡住，除非反过来放宽分享页自己。用一个真实的子资源
（`/s/{token}/html`）才能让它拿到自己的响应头，也就才能把两套策略分开。
