# 包装 ResponseWriter 时「进入时写头」会覆盖 handler 自己设的 Content-Type

**Severity:** 🟠 medium（可把 JS bundle 变成 `text/plain`，页面白屏） · **First hit:** 2026-10-18 · **Hits since:** 1

## Symptom

在 `spa.Handler` 里包一层装饰器给所有响应加安全头（CSP / Permissions-Policy），
写成「进入 `ServeHTTP` 就先 `w.Header().Set(...)`，并跳过已经存在的键」：

```go
for k, v := range h.securityHeaders {
    if w.Header().Get(k) != "" { continue }   // 「不覆盖别人设的」——看起来对
    w.Header().Set(k, v)
}
```

断言「不覆盖 handler 自己的头」的测试**当场变红**：

```
Content-Type = "text/plain"; the security headers must not overwrite a decision made
closer to the bytes
```

也就是说：装饰器把 `Content-Type: text/plain`（配置里的一个安全头）打在了
`assets/index-abc123.js` 上。**一个「加固」改动会把 JS bundle 变成纯文本。**

## Root cause

**顺序，而不是逻辑。** `if Get(k) != "" { continue }` 表达的是「凡是没有的键就填上」，
但这个判断在**handler 还没跑**的时候执行——那时 `Content-Type` 必然是空的。
于是「不覆盖」的检查永远为真地不覆盖，而装饰器**先**写下的值反过来成了 handler 无法覆盖的
那个值（handler 用 `Set` 写的是同一个键，但语义上「先到先得」变成了先到者的胜利）。

关键在于：`http.Header` 在响应提交前是**共享可变状态**，谁先写谁的值就在那里，
「覆盖」与否取决于谁最后写，**不取决于谁更有权威**。想表达「不要覆盖内层的决定」，
唯一正确的时机是**内层的决定已经做完之后**。

## Fix

把头的写入推迟到**响应提交时**，用一个包装 writer（`backend/internal/spa/spa.go` 的
`headerWriter`）：`WriteHeader` / `Write` / `Flush` 里先 `commit()`，此时 handler 已经写完了
它自己的头，`Get(k) != ""` 才真正表示「别人已经设过」。

`Flush` 必须转发，否则 `http.ServeContent` 的范围请求会停止流式输出。

## Guard

`internal/spa/spa_test.go`：

- `TestSecurityHeadersDoNotOverwriteTheHandlersOwn` —— **就是它抓到这处缺陷的**。
  构造一个与 handler 真实设置的键**故意冲突**的配置（`Content-Type`），
  然后断言 handler 的值活下来。
- `TestSecurityHeadersApplyToEveryPathThisHandlerServes` —— 逐条路径（shell、客户端路由、
  hashed bundle、字体、favicon）断言头**确实到了**，防止「只在 shell 上有」。
- `TestNoSecurityHeadersConfiguredChangesNothing` —— 零值必须一个头都不加。

**推广的规则：凡是「加一层中间件/装饰器，但不许覆盖内层决定」的需求，
判据必须在实现内部发生之后才评估；在进入时评估只能是错的。**
（这也正是 `httpx.Compress` 要延迟 header 的原因，同一个家族。）
