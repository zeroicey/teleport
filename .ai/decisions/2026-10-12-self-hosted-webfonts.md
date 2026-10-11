# 自托管 Web 字体：Outfit + Noto Sans SC + JetBrains Mono

**Status:** ✅ ACCEPTED · **Date:** 2026-10-12 · **Deciders:** 用户（「不用系统自带字体」）· **Supersedes:** none

## Context

用户要求 UI 优化，并明确提出两条：**CSS 迁移到 TailwindCSS**、**不用系统自带字体**。

原状态：`web/src/styles.css` 手写 CSS，`font-family` 是
`-apple-system, BlinkMacSystemFont, "Segoe UI", "Noto Sans SC", Roboto, …`；
Go 服务端渲染的分享页 / 交接页 / `/ai` 页同样是系统字体栈。

约束（都是既有的，不是新加的）：

1. **运行时不能碰 CDN。** 整个架构存在的理由就是大陆可达性（见
   `.ai/decisions/2026-10-09-mainland-reachability-single-entry.md`）。Google Fonts
   在大陆不可达，`fonts.googleapis.com` 必须只出现在**构建/取材期**，不能出现在产物里。
2. **单文件部署。** 字体必须进 `//go:embed`，跟着二进制走，不能是部署时另传的资源目录。
3. **CSP 不许放宽。** 分享页与交接页的 CSP 是 `font-src 'self'`；
   `style-src 'self' 'unsafe-inline'`。字体必须同源。
4. **前缀只写一处。** `/yeciorez/teleport` 由 `ROUTE_PREFIX` 推导（硬约定）。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. 继续用系统字体栈 | 零字节、零改动 | 用户明确否决；中文在 Windows/Linux 上落到宋体或 Droid Sans Fallback，跨平台观感差异大；数字宽度不齐 | — |
| B. 运行时 `<link>` 到 Google Fonts | 一行搞定，自动拿最新 | **大陆不可达**，直接违反架构前提；且 CSP 要放宽到外域 | easy |
| C. 引入 `@fontsource/*` npm 包 | 有版本管理、Vite 自动处理 | 拉丁字体可行，但 **Noto Sans SC 的 CJK 分包**在 npm 上体积与切片策略不受控；`@fontsource` 产物进 `assets/` 会被内容哈希，Go 模板无法稳定引用 | medium |
| D. **构建期取材 + 自托管 + 内容哈希文件名**（选定） | 运行时零外域；进 embed 跟着二进制；CSP 不动；切片保留 | 需要一次性取材脚本；仓库多 4.4 MB；刷新字体要重跑脚本 | medium |

## Decision

**D。** 具体规则：

1. **字体**：`Outfit`（拉丁 UI/标题）、`Noto Sans SC`（中文正文，按 `unicode-range` 切片）、
   `JetBrains Mono`（ID / token / 代码）。三者都是可变字重 `wght@400..700`。
2. **取材脚本** `scripts/fonts/fetch_fonts.py`，**只在人工需要时跑**，绝不进构建流程
   （构建必须能离线跑）。它伪装 Chrome UA 取 Google 的 CSS，把 `gstatic` 的绝对 URL
   重写成 `url(./<name>.woff2)` 并把二进制落到 `web/public/fonts/`。
3. **产物位置 `web/public/fonts/`**，不是 `web/src/`。放 `public/` 有两个后果，都是要的：
   - Vite **不做内容哈希**，文件名由脚本决定 —— Go 模板才能用**稳定名**引用
     `fonts/fonts.css`，不需要读构建清单；
   - 不进 `assets/`，因此**不会**触发 `spa.go` 里 `stableAssetFiles` 必须恰好 1 条的断言。
4. **二进制文件名带内容哈希**：`<slug>-<NNN>-<sha256[:8]>.woff2`。
   `fonts.css` 本身保持稳定名。理由见 Consequences 第 2 条 —— 这不是美观问题。
5. **引用方式**：
   - SPA：`web/index.html` 里 `<link rel="stylesheet" href="%BASE_URL%fonts/fonts.css" />`。
     **必须用 `%BASE_URL%`**，理由见 `.ai/pitfalls/cases/2026-10-12-relative-asset-url-under-spa-fallback.md`。
   - Go SSR（分享页 / 交接页 / `/ai`）：用**绝对**前缀 URL
     `{assetBase}/fonts/fonts.css`（`assetBase` = `cfg.AssetBaseURL`，
     默认等于 `AppBaseURL()`）。`/s/{token}` 与 `/k/{id}` 都是两段深，相对路径会 404。
6. **系统字体栈全部删除**，包括 Go 模板里的 5 处 `-apple-system,…` 与 4 处
   `ui-monospace,…`，统一为 `"Outfit","Noto Sans SC",sans-serif` /
   `"JetBrains Mono",ui-monospace,monospace`。

## Consequences

1. **仓库增加 4.4 MiB（109 个 woff2）。** 二进制从 ~21 M 涨到 25 M。
   接受：单文件部署下这是唯一不依赖外域的做法。
2. **字体文件名必须带内容哈希，否则每次整页加载重下 260–830 KB。**
   这是本轮最容易漏掉、后果最重的一条：
   - `spa.go` 只给「名字能自证字节不变」的 URL 发 `immutable`，其余一律 `no-cache`；
   - 而 `embed.FS` 的 `ModTime()` 是**零值**，`http.ServeContent` 因此**不发 `Last-Modified`**；
   - 于是 `no-cache` 无法退化成 304，浏览器**只能**重新下载整个 body。
   所以：字体二进制内容寻址（`immutable`），`fonts.css` 稳定名 + **ETag**（304）。
   为了让稳定名也能 304，`spa.go` 新增 `Handler.etagFor`（进程内惰性缓存），
   `serveIndex` 也从裸 `Write` 改成 `ServeContent`。
3. **`unicode-range` 是 CJK 方案的全部。** Noto Sans SC 109 个切片；
   实测单页只取 5–15 片（登录 260 KB、报告详情 830 KB），而不是整包 4.4 MB。
   取材脚本把「`unicode-range` 存在」写成硬断言 —— 丢了它，每页变成多兆下载。
4. **`@theme` 的暗色覆盖用不带 layer 的 `@media`。** 不带 layer 的规则优先级高于
   Tailwind 的 `@layer theme`，所以覆盖生效，且不必复制整套 token。
5. **新增 gzip 中间件**（`httpx.Compress`）。`fonts.css` 原始 107 KB；
   后端此前完全没有压缩，稳定名的 107 KB 每次加载都要完整走一遍。
   压缩后 32 KB（gzip）/ 18.7 KB（brotli）。
   刻意只压 `text/*`、`application/json`、`svg`，且跳过 Range / HEAD / 非 200 ——
   woff2 本身已压缩，再压只会更大。**顺序是正确性的一部分**：
   `ServeContent` 先发 header 再发 body，判定必须早于 header 刷出，
   否则客户端拿到 `Content-Length: 107422` 却收到 gzip 字节，响应被截断。
6. **`pnpm run build` 不需要网络。** 字体是已提交的产物，取材是显式动作。
   代价：刷新字体要人跑脚本并提交 109 个文件。

## Revisit when

- 需要第四个字族，或仓库体积成为实际问题（届时考虑按需裁剪 CJK 切片，而不是回退到 CDN）；
- `unicode-range` 在某个目标浏览器上不再被遵守（那会让本方案的核心优势消失）；
- 项目决定引入反向代理（Caddy）承担压缩 —— 届时 `httpx.Compress` 可以考虑移除，
  但它目前保证了「裸二进制」与「代理后」行为一致，删之前先确认这一点仍不重要。
