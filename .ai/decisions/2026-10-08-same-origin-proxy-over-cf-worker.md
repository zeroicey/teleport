# 前后端分离 + CF Worker 同源反代

**Status:** 🪦 REJECTED · **Date:** 2026-10-08 · **Deciders:** lead · **Superseded by:** `2026-10-09-mainland-reachability-single-entry.md`

## Context

在中间阶段，架构是「前端留在 Cloudflare，后端迁到国内服务器」：浏览器只访问
`https://teleport.zeroicey.me`（CF Worker），Worker 把 `/api/*`、`/s/*` 反代到
`https://api.hcyj.xyz/yeciorez/teleport/...`，其余路径由 Workers Assets 提供 Vue SPA。

迫使这个设计的约束：**第三方 Cookie 淘汰**。若前端直连 `api.hcyj.xyz`，会话 Cookie
就是跨站 Cookie，Chrome 默认拦截、Safari ITP 直接拒绝，登录会莫名其妙失败。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. CF Worker 同源反代 | Cookie 第一方；接口地址对浏览器隐藏 | 多一跳；**CF 在大陆不可达**（当时未验证） | easy |
| B. 前端直连 `api.hcyj.xyz` | 少一跳 | 跨站 Cookie 被拦，登录必坏 | easy |
| C. 前端与后端同域 | 无跨站问题 | 需要后端能托管前端 | easy |

## Decision

选择 A。Worker 只做两件事：`/api/*` 与 `/s/*` 反代到国内后端，其余交给
`env.ASSETS.fetch`；`run_worker_first = ["/api/*","/s/*"]`。
前缀 `/yeciorez/teleport` **保留**（Worker 拼接时不 strip，Caddy 侧用 `handle`）。

## Consequences

- 交付并验证通过：11/11 Playwright 检查、XSS 全清、Mermaid 渲染、Cookie 为第一方
  `HttpOnly; Secure; SameSite=Lax`、零控制台错误。
- 前缀必须对后端可见这条被固化下来（路由、`PUBLIC_BASE_URL`、分享链接、分享页里的
  `/assets/share.js` 都依赖它）。
- **这套方案最终被否决**：`teleport.zeroicey.me` 在大陆两个观测点**稳定**不通，
  ICMP 通、DNS 正常、TCP 443 被阻断。C 方案（前端与后端同域，同由 hcyj 提供）
  才是正解。
- 排查过程中的方法论教训：曾用 Azure 香港做观测点得到 300/300 全通，据此误判为
  「本地运营商偶发抖动」。**境外观测点无法检测大陆封锁。**

## Revisit when

不适用 —— 已被后继决策取代。
