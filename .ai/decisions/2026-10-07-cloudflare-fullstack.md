# 基于 Cloudflare 全家桶的全站部署

**Status:** 🪦 REJECTED · **Date:** 2026-10-07 · **Deciders:** user · **Superseded by:** `2026-10-09-mainland-reachability-single-entry.md`

## Context

用户初始需求：构建「基于 Cloudflare 全家桶的个人 AI 报告展示与时效分享平台」——
轻量、无需传统服务器、**免备案**。场景是终端/工作框里做完复杂工作（渗透测试、架构设计、
开发进度）后，把报告发布成在线网页并拿到带时效的分享链接。

指定技术选型：Workers (TypeScript) + D1 (SQLite) + Pages/Workers Assets +
Markdown/Mermaid 渲染。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. Cloudflare 全家桶 | 免备案、免运维、免费额度 | **大陆不可达**（当时未知） | hard（已建代码） |
| B. 国内服务器从零 | 大陆可达 | 需备案（若用域名）、要自己运维 | hard |

## Decision

选择 A：Workers + D1 + Workers Assets + Hono，Vue 3 由用户在第二轮明确指定
（否决了建议的 SvelteKit/Astro/Next.js）。`html` 渲染按用户要求「后面再说」，
`format:'html'` 预留但始终关闭。

## Consequences

- 交付可用：D1 迁移、上报→分享→读取→吊销全链路、401/404/410 语义、管理面板登录、
  SPA 资源图、CORS 白名单，均有测试。
- `pnpm` 为唯一包管理器（用户否决 npm）；Playwright 与全部 E2E 产物按用户要求删除
  （「我自己去点去测就好了」）。
- **这套方案最终被否决**：CF 免费版 anycast IPv4 在大陆被 TCP 层封锁，域名与证书都正常
  但 443 完全打不开。见后继决策与
  `.ai/pitfalls/cases/cloudflare-free-ip-blocked-in-mainland.md`。
- 保留下来的东西：数据模型（`reports`/`share_tokens`）、接口形状、响应封套、
  404/410 语义、epoch 毫秒约定 —— 这些在 Go 重写时逐条对齐，未重新设计。

## Revisit when

CF 免费版在该账户下分配到大陆可达的边缘 IP，或获得 ICP 备案 + CF China Network。
