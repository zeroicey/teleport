# `format: "html"` 报告原样渲染

**Status:** ✅ ACCEPTED · **Date:** 2026-10-11 · **Deciders:** 用户（"就是 ai 自己写 HTML 的代码以及样式 JS 逻辑 就不用太考虑那些安全问题 毕竟我都是给自己用"） · **Supersedes:** `AGENTS.md`「原始 HTML 不渲染」硬约定（仅限报告正文这条路径）

## 背景

`POST /api/reports` 一直接受 `format: "html"` 并原样入库，但分享页对它的处理是
一句 `此报告为 HTML 格式，但 HTML 渲染尚未启用。`（`views/sharepage.go:61`），
并且紧跟着的分支是死代码。也就是说：**这个格式被接受、被存储、然后被丢掉**，
发布者看到的是一个空白提示。

上一轮 handoff 把「HTML 渲染」列为下一件事，并要求先澄清两条路：

- **A**：允许 AI 直接上报 HTML/CSS/JS 并由分享页渲染。
- **B**：给现有 markdown 分享页加 TOC / 复制按钮 / 打印样式。

用户选了 **A**，并明确豁免安全顾虑（单用户自用、无值可攻）。

## 裁决

| # | 问题 | 裁决 | 依据 / 代价 |
| --- | --- | --- | --- |
| D1 | 渲染形态 | **不拼接**，而是在分享页里放一个同源 iframe，指向新端点 `GET {prefix}/s/{token}/html`，该端点把 `report.content` **逐字节原样**返回 | AI 写的是**完整页面**（自己的 `<html>/<head>/<style>/<script>`）。把它塞进 `<div>` 是非法 DOM（浏览器会把 `<style>` 提升、丢弃 `<head>`），而且分享页自己的 `#report-root` 排版会和 AI 的 CSS 打架。iframe 给 AI 一个真正的文档 |
| D2 | iframe 是否 `sandbox` | **不加 `sandbox`**（同源可编程） | 用户已豁免安全顾虑；加 sandbox（无 `allow-same-origin`）会让 AI 的 JS 拿不到 `localStorage`、`document.cookie`，直接破坏「AI 写的 JS 逻辑要能跑」这个需求。留一行常量 `views.HTMLReportSandbox`（空串=不隔离），将来想隔离改一行 |
| D3 | 外层分享页 CSP | 只加 `frame-src 'self'`，**保持 `script-src 'self'`**（不给外层加 `'unsafe-inline'`） | 报告正文跑在**子文档**里，子文档有自己的 CSP 响应头；外层页面本身不需要放宽。`default-src 'none'` 会让 `frame-src` 回退成 `'none'`，所以必须显式加 |
| D4 | 子文档 CSP | `views.HTMLReportContentSecurityPolicy`：`default-src 'none'` + `script-src 'unsafe-inline' 'unsafe-eval' * data: blob:` + `style-src 'unsafe-inline' *` + `img-src * data: blob:` + `connect-src *` + `frame-ancestors 'self'` | 不净化、不 nonce：AI 的 HTML/CSS/JS 原样执行。`frame-ancestors 'self'` 只允许本服务的分享页框它 |
| D5 | `X-Frame-Options` | 子文档响应**删除**全局中间件设的 `DENY`（显式 `h.Del`） | `httpx.SecurityHeaders` 给每个响应都塞 `X-Frame-Options: DENY`，不清掉 iframe 会被浏览器直接拒绝渲染。这是一个「全局中间件与按路由例外」的坑，已记入 pitfalls |
| D6 | 视图计数 | iframe 子资源**不**计数，只由外层分享页记一次 | 否则一次浏览记两次 |
| D7 | 链接语义 | 子端点与分享页完全一致：未知/已吊销 **404**、过期 **410**；markdown 报告访问该端点也是 **404**（不发明新状态） | 既有硬约定 |
| D8 | `HTMLMountEnabled` | **删除**。换成 `views.HTMLReportsRenderVerbatim = true`，并把 `aidoc` 的绊线测试改成断言新的真话 | 原常量 + 死分支表达的是「关闭」，继续留着只会误导 |
| D9 | markdown 路径 | **完全不动**：goldmark 仍不加 `WithUnsafe`，原始 HTML 仍被替换为 `<!-- raw HTML omitted -->` | 安全默认只对「AI 显式声明 format=html」让步 |

## 已知并被接受的代价（务必知情）

1. **报告正文与面板同源。** 一份 `format:"html"` 的报告里的 JS 运行在应用源上，
   而管理面板、`/api/admin/*` 也在同一源。它**不能**读走 `teleport_session`
   （HttpOnly），但可以对 `/api/admin/*` 发**同源带凭据**的请求 —— 也就是
   「以查看者身份操作面板」。单用户自用场景下用户已书面接受。
   想消除这一点：`HTMLReportSandbox = "allow-scripts allow-popups allow-forms"`，
   代价是 AI 的 JS 失去 `localStorage` / cookie（opaque origin），且 iframe
   必须自报高度（`postMessage`），不能再用同源测量。
2. **`format:"html"` 的报告里，markdown 转义不再保护任何东西**。发布者（agent）
   写什么就执行什么。分享链接是能力凭据：拿到链接的人看到的就是发布者写的页面。
3. **不再是「原始 HTML 一律不渲染」的仓库**。README / AGENTS.md / guide / llms.txt
   里所有相关断言必须在**同一次改动**里改写，否则文档与代码分叉
   （`aidoc` 的绊线测试就是为这件事存在的）。

## 迁移与回滚

- 无 schema 变更、无数据迁移。已存在的 `format:"html"` 报告**立即可以看了**。
- 回滚：把 `sharepage.go` 的 iframe 分支改回提示即可；子端点留着不影响任何人
  （没有外层页面会去请求它）。
