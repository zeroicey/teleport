# 跨站会话 Cookie 会被浏览器拦掉，登录"莫名"失败

**Severity:** 🟠 medium · **First hit:** 2026-10-08 · **Hits since:** 0（设计期避开）

## Symptom

登录接口返回 200 + `Set-Cookie`，但紧接着的 `GET /api/admin/reports` 是 401 ——
Cookie 根本没被带上。控制台里可见第三方 Cookie 被阻止的提示。

## Root cause

如果前端在 `teleport.zeroicey.me`、而接口在 `api.hcyj.xyz`，会话 Cookie 就是
**跨站（third-party）Cookie**。Chrome 默认逐步淘汰、Safari ITP 直接拒绝，
`SameSite=None` 也救不了（还要求 `Secure`，且仍可能被拦）。

## Fix

**让前端与接口同源。** 本项目走过两个阶段：

1. 中间阶段：CF Worker 在 `teleport.zeroicey.me` 反代 `/api`、`/s` 到后端 ——
   Cookie 变成第一方。（`.ai/decisions/2026-10-08-same-origin-proxy-over-cf-worker.md`）
2. 最终：前端与后端同由 hcyj 提供，同一个源、同一个前缀 ——
   同源问题从根上消失。（`.ai/decisions/2026-10-09-mainland-reachability-single-entry.md`）

Cookie 属性：`HttpOnly; Secure; SameSite=Lax; Max-Age=43200`，
且 **`Path` 取应用前缀**而不是 `/`（共享主机上避免把凭据发给别的服务）。

## Guard

- 架构层面：`.ai/ARCHITECTURE.md` §4 把"只有一条入口、一个来源"确立为设计目标，
  并写明理由就是第三方 Cookie 与 `script-src 'self'`。
- 验收时用真实浏览器检查：登录后硬刷新深链接仍处于登录态，且 Cookie 是
  **第一方** `HttpOnly; Secure; SameSite=Lax`。
- 反例警号：`Set-Cookie` 出现但后续请求 401 —— 先怀疑跨站，而不是先怀疑签名逻辑。
