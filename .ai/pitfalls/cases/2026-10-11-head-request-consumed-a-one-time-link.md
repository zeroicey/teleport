# 一次 `HEAD` 请求就把一次性交接链接烧掉了（Go `ServeMux` 把 HEAD 交给 GET 模式）

**Date:** 2026-10-11 · **Area:** `api/router.go` · `api/keys.go` · `httpx/errors.go`

## 症状

交接链接（`/k/{id}#<secret>`）是**一次性**的：领取成功即消耗，页面把密钥显示给收件人。
线上功能一切正常，直到一次独立对抗验证发现：

```
HEAD /api/agent-keys/applications/{id}   with X-Teleport-Claim: <correct secret>
  → 200，链接被消耗，agent_keys 多出一行
GET  /api/agent-keys/applications/{id}   with the same secret
  → 404（已经领过了）
```

**收件人什么都没拿到**：`HEAD` 的响应没有 body，密钥在这一刻被派生出来又被丢弃。
一个代理、预取器、链接检查器、或者任何「先 HEAD 一下看资源在不在」的工具，
只要它带着 claim 头重发请求，就能把收件人的链接变成废纸 —— 而且没有任何错误提示。

## 根因

不是我们写了 `HEAD` 分支，而是 **Go 1.22+ 的 `net/http.ServeMux` 会把 `HEAD` 请求交给
`GET` 模式**。注册的是：

```go
mux.HandleFunc("GET "+p+"/api/agent-keys/applications/{id}", s.handlePollKeyApplication)
```

看起来「只有 GET 能进来」，实际上 `HEAD` 也进得来，而 handler 里没有任何方法判断。
所有「GET 是安全的」的直觉在这里都不成立：这个 GET 处理器**会写状态**（消耗链接、建密钥行）。

## 修复

```go
if r.Method == http.MethodHead {
    w.Header().Set("Allow", http.MethodGet)
    httpx.WriteError(w, r, httpx.MethodNotAllowed("Claim this application with GET"))
    return
}
```

同样的守卫也加在 `handlePollRenewal`（它同样返回凭据）。新增
`httpx.MethodNotAllowed`（405 + `code:"method_not_allowed"`）。

## 防线

- `TestKeyHandoffClaimRefusesHEAD`：HEAD 必须 405 + `Allow: GET`，**且随后用同一个 secret
  的 GET 必须仍然 200**（只断言 405 不够 —— 真正的损失是「链接被烧掉」，所以要断言链接还活着）。
- 反证 **F11**（`bin/e2e/falsify-handoff.py`）：把守卫删掉，该测试必须变红（已验证变红）。

## 教训

- **「GET 只读」是要检查的假设，不是语言保证。** 一旦 GET 处理器会改状态，就必须显式拒绝
  `HEAD`（以及想清楚 `OPTIONS`）。`ServeMux` 的 `GET` 模式 = `GET + HEAD`。
- **一次性凭据的「销毁」必须和「交付」绑在一起。** 这次失败的本质是「消耗」发生了而
  「交付」不可能发生（无 body）。凡是不可逆操作，都要问一句：**所有能让它发生的路径，
  收件人是否都能拿到结果？** 不能的路径要拒绝，而不是让它跑完。
- **对抗验证要找「非预期的方法/路径/顺序」，而不是重复正常流程。** 正常流程（浏览器 GET）
  在这条 bug 存在时 100% 正常，所以自测永远看不到它。
- 同类风险清单（值得逐个检查）：`HEAD`、`OPTIONS`、`GET` 上的重试、浏览器预取、
  邮件/IM 的链接展开、`?` 参数变体（会进日志）—— 它们的共同点是**由第三方而非用户触发**。
