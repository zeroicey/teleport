# 大陆可达性优先：前端与后端合并到 hcyj 单入口

**Status:** ✅ ACCEPTED · **Date:** 2026-10-09 · **Deciders:** lead · **Supersedes:** `2026-10-07-cloudflare-fullstack.md`, `2026-10-08-same-origin-proxy-over-cf-worker.md`

## Context

用户报告 `https://teleport.zeroicey.me/` 在中国大陆**怎么都打不开**。此前架构是
CF Worker（`teleport.zeroicey.me`）反代 `/api`、`/s` 到 `api.hcyj.xyz` 的 Go 后端。

排查结论（两个大陆观测点：广东电信家宽、hcyj 本身）：

| 检查 | 结果 |
| --- | --- |
| DNS | 正常（223.5.5.5 / 119.29.29.29 / 8.8.8.8 / 1.1.1.1 都返回同一对 CF IP） |
| ICMP | 通（233ms，0% 丢包） |
| TCP 443/80/2053 | **全部阻断**，连续 3 轮稳定 |
| 同一 SNI 换 CF 边缘 IP | `104.16.0.1`…`188.114.96.1` → 200 |
| 另一些 CF IP | `162.159.0.1`、`108.162.192.1` → 403；`172.65.0.1` → 超时 |

→ **封锁在 IP 层**，不是域名层也不是 SNI 层。DNS 没被污染、ICMP 也通，所以不是路由黑洞。
对照：同一台服务器的 `api.hcyj.xyz/yeciorez/teleport/api/health` 从大陆 **200**。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. 前端也交给 hcyj，单入口 | 直接消除被封锁的一跳；同源同前缀 | 放弃 CF 的静态托管与全球边缘 | easy |
| B. CF「优选 IP」 | 保留 CF | **要求 DNS 移出 CF，Worker 随即失效**，方案自毁 | hard |
| C. CF China Network | 官方大陆加速 | 企业版 + 域名 ICP 备案 | n/a |
| D. 维持现状，只做境外 | 零改动 | 用户主场景就是大陆 | n/a |

## Decision

**前端与后端合并到 hcyj，只有一个公开入口**：
`https://api.hcyj.xyz/yeciorez/teleport/`，Caddy 用 `handle`（保留前缀）反代到
`172.17.0.1:8788`。CF 侧全部下线（Worker 删除、`teleport.zeroicey.me` 的 DNS 记录随
`custom_domain` 一并移除）。

**安全上没有退步**：原本 Worker 只是转发一跳，而
`api.hcyj.xyz/yeciorez/teleport/api/...` 本来就是公网可达的 —— 它从未真正「保护」后端。

## Consequences

- 会话 Cookie 变成真正的第一方，`script-src 'self'` 有实际意义（这是 B 之外 A 的额外收益）。
- 前端构建必须支持路径前缀：Vite `base`、`createWebHistory(BASE_URL)`、`api.ts` 的
  `BASE_URL` 三处同源推导，**前缀只写一处**。
- 部署变成「上传一个文件」——没有独立的静态资源部署步骤。
- 副作用发现：`zeroicey.me` 与 `gh.zeroicey.me` 解析到**同一对被封锁 IP**，用户主站
  在大陆同样不可达。这不属于本仓库，需单独修。
- `wrangler.toml`、`src/index.ts`、`worker-configuration.d.ts` 已从仓库移除，不再需要
  Cloudflare 凭证。

## Revisit when

获得 ICP 备案 + 可用的境内 CDN，或 CF 在该账户下分配到大陆可达的边缘 IP。
**任何重加 CDN 的改动，必须先用大陆观测点复验**（境外观测点测不出封锁）。
