# 交接页的「协议漂移」：服务端与浏览器各写一半，接口测试全绿而功能全废

**Date:** 2026-10-11 · **Area:** `views/handoff.go` · `api/keys.go` · `web/src/share/main.ts`

## 症状

一键交接链接在 HTTP 层面**完全正常**：`POST /api/admin/key-handoffs` 201 返回链接，
`GET /k/{id}` 200 返回一页看起来正确的说明页，Go 侧测试全绿（链接带 fragment、
页面 no-store、状态四态齐全）。但在真实 Chromium 里打开链接后：

1. 「领取密钥」按钮**永远不出现**，页面停在「正在检查链接…」；
2. 修掉第一个问题后，点击按钮报「这条链接已经领取过了，或者已经失效」——而它一次都没被领过；
   网络面板显示请求打到了 `/api/api/agent-keys/applications/{id}`，**404**。

## 根因：两个 bug 都属于「同一个字符串由两处独立约定」

| # | 服务端产出 | 客户端预期 | 后果 |
| --- | --- | --- | --- |
| 1 | `/k/{id}#<secret>`（裸 secret） | `#k=<secret>`（`URLSearchParams.get('k')`） | fragment 解析成空 → 判定「链接不完整」→ 按钮不显示 |
| 2 | `data-app-base` 位置传的是 `AppBaseURL()+"/api"` | 客户端自己再拼 `/api/...` | `/api/api/...` → 404 → 页面说「已经领过了」（404 被折叠成这一种解释） |

两个 bug 都**不可能被 Go 测试发现**：Go 测试断言的是「URL 里 fragment 等于 secret」和
「页面里有 data 属性」，而这两件事当时都是对的。漂移发生在「服务端字符串」与
「另一门语言里写死的解析方式」之间 —— 中间没有任何编译器或类型检查。

第 2 个 bug 的次生伤害更值得记：**404 的语义被借用成三种情况**（已领取 / 不存在 /
路径写错），于是「路径写错」被页面上报成「链接已被领取」，把使用者引向完全错误的动作
（去找管理员重发，而不是报告 bug）。

## 修复

- 客户端以**裸 fragment** 为准（`location.hash.slice(1)`），同时兼容 `k=` 前缀，
  避免手工改过的链接变成一页不能用的页面。
- `KeyHandoffOptions.APIBaseURL` 改名为 `AppBaseURL`，只传应用根；`/api` 由客户端拼。
  名字本身就是契约：叫 "APIBase" 就会有人往里塞 `/api`。
- 新增两条 Go 断言（`TestKeyHandoffLinkCarriesTheSecretOnlyInTheFragment`）：
  fragment 不得含 `=`，URL 不得含 `?`。

## 防线（可复现）

1. `backend/internal/api/handoff_test.go` —— fragment 形状 + 无 query + `data-app-base`
   不得是 API 根。用 `bin/e2e/falsify-handoff.py` 的 **F9/F10** 反证：
   把产出改回错误形状，这两条必须变红（已验证变红）。
2. `bin/e2e/verify-handoff.cjs` —— 真 Chromium 端到端：真点击（`Input.dispatchMouseEvent`，
   不是 `element.click()`）、真读取 DOM、**监听网络响应**确认打到的是
   `/api/agent-keys/applications/...`（不是 `/api/api/...`）且状态 200。
   这一条是本次唯一能同时抓住两个 bug 的检查。

## 教训

- **跨语言/跨进程的字符串契约必须有「消费方视角」的测试。** 生产方自测再充分也只证明
  它产出了自己以为的东西。让浏览器（或任何真实消费方）读一遍，是最便宜的真相来源。
- **只做 DOM 断言不够，要看网络。** 第二个 bug 的 DOM 表现（"已经领过了"）与真实原因
  （路径错）毫无关系；只看页面文字会修错地方。
- **合成点击 ≠ 用户点击。** `element.click()` 不产生 user activation，
  Clipboard API 会合法拒绝；用 `Input.dispatchMouseEvent` 才是真实路径，
  同时也要 `Emulation.setFocusEmulationEnabled`，否则 headless 页面 `document.hasFocus()`
  为 false，剪贴板必然失败。
- **错误语义越折叠，越容易把 bug 说成功能。** 404 折叠「已领取/不存在」是刻意的安全设计，
  但它让「路径写错」也长得一样。这类折叠旁边必须留一条网络层检查。
