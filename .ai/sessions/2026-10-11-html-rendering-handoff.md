# `format:"html"` 原样渲染 —— 已实现并本地验证，未提交未部署 — handoff

**Date:** 2026-10-11 · **Branch/Commit:** `main @ adc2be3` · **线上:** `adc2be3`（已部署并实测）

## Where we ended

上一轮 handoff 要求「开工前先问用户 A 还是 B，不要默认开工」。这一轮问了，用户选了
**A** 并**书面豁免安全顾虑**：

> 「就是 ai 自己写 HTML 的代码以及样式 JS 逻辑 就不用太考虑那些安全问题
> 毕竟我都是给自己用 就不用去管 XSS 注入攻击」

于是实现了 A：`format:"html"` 的正文**逐字节原样**渲染。B 路线（TOC/复制/打印）**没做**。

**代码完成、闸门全绿、真 Chromium 本地验过 → 已提交（`adc2be3`）并部署到 hcyj，线上实测通过。**
用户下一轮选择「先看实际效果再说」，所以**没有排新功能**。

## What changed

| File / area | Change | Why |
| --- | --- | --- |
| `views/sharepage.go` | 删 `HTMLMountEnabled`（及其死分支），改为 `HTMLReportsRenderVerbatim = true` + `HTMLReportSandbox = ""`；新增 `HTMLReportContentSecurityPolicy`；`SharePageOptions.HTMLReportURL`；html 报告渲染成 `<iframe class="report-frame">` + `wrap-wide` + 「在新窗口打开」链接 | 报告是**完整文档**，拼进 `<div>` 是非法 DOM；iframe 让它拥有自己的文档/CSS/JS |
| `views/sharepage.go` | 外层 CSP 只加 `frame-src 'self'`（`script-src` 仍 `'self'`） | `default-src 'none'` 会把 `frame-src` 回退成 `'none'`；报告脚本跑在子文档，不该放宽外层 |
| `api/router.go` | 新路由 `GET /s/{token}/html` + `handleShareHTML`：逐字节写回、`h.Del("X-Frame-Options")`、子文档 CSP、`no-store`、**不记浏览数**；markdown 报告 404 | 见 pitfalls：XFO/`frame-src`/`/s/` 兜底三个默认值都会静默挡住 frame |
| `api/helpers.go` | `renderSharePage` 传 `HTMLReportURL = cfg.ShareURL(token) + "/html"` | 前缀只写一处 |
| `web/src/share/main.ts` | 新增 `fitReportFrames()`：同源读 `contentDocument` 高度 + `ResizeObserver` + resize | iframe 高度只能靠同源测量（这也是不加 sandbox 的代价） |
| `web/src/views/ReportDetailView.vue` | 「正文」说明改写（原文案还写着「由 Worker 服务端渲染」） | 顺手修掉一处过时文案；面板仍只显示源文、不执行 |
| `aidoc/guide.md` `aidoc/html.go` `README.md` `AGENTS.md` `.ai/ARCHITECTURE.md` | `format:"html"` 的说法从「不渲染」改成「原样渲染 + 代价」 | 仓库硬约定被用户推翻，文档必须同一次改 |
| `aidoc/aidoc_test.go` | `TestHTMLCaveatMatchesImplementation` 改成**双向**绊线；`TestGuideDocumentsTheContract` 的 caveat 断言换掉 | 单向检查挡不住「改回去」时的漂移 |

新文件：`.ai/decisions/2026-10-11-html-report-rendering.md`（D1–D9 + 代价）、
`.ai/pitfalls/cases/2026-10-11-framed-report-blocked-by-defaults.md`（三个默认值 + 为什么不用 `srcdoc`）。

## Verified / unverified

- [x] `pnpm run check` 全绿、`pnpm run backend:vet` 无告警、`pnpm run build:release` 出二进制
- [x] **8 条反证**，每条都让对应测试变红：
      删 `h.Del("X-Frame-Options")` / 把正文转义 / 给子资源记浏览 / 去掉 `frame-src` /
      放行 markdown 走该端点 / 默认打开 `HTMLReportSandbox`（违反决策 D2）/
      guide 说「不渲染」而实现已开启（正向绊线）/ 实现改回关闭而 guide 仍承诺（反向绊线）
- [x] `internal/views` 新增 5 条单元测试（逐字节之外的渲染形态：framed-not-spliced、
      markdown 不加宽列、独立链接与 frame 指向同一 URL、sandbox 旋钮两个分支、两条 CSP 不同）
- [x] **本地真 Chromium**（`chromium-1248`）：iframe 被 `share.js` 撑到 `379px`；
      子页内联脚本 / 同源 `fetch` / `localStorage` 三项全绿（截图像素分析：绿 ✓ 出现、
      红 0 个，报告深色底占 iframe 区域）；markdown 回归：无 iframe、Mermaid 出 `<svg>`、
      `<script>` 仍变 `raw HTML omitted`
- [x] 头核对：分享页 `frame-src 'self'` + `XFO: DENY`；子文档**无 XFO**、有独立 CSP；
      正文与提交字节**完全一致**（1845 B）
- [x] **线上（2026-10-11 11:38 部署 `adc2be3`）**：本地与线上 sha256 一致
      （`bd60a4a1…`）；线上发测试报告 → 正文 sha256 跨线一致（`4035aecd…`）；
      分享页 `frame-src 'self'` + `XFO: DENY`、子资源无 XFO；Chromium 实测 iframe `379px`、
      子页三项 JS 自检全绿；markdown 回归（`QDE56Dvt4kRsvCQjyJ6DRA`）无 iframe 且 Mermaid 正常；
      部署后日志 `error|panic|fatal` = 0；测试报告已删除，计数回到 3/4/1，零孤儿
- [x] **大陆观测点**：koma（腾讯云 `124.221.144.97`）视角两个 URL 均 200 且头正确
- [ ] **未验证：北京观测点 `bjbuwe`**（只用了 koma + hcyj 本机）
- [ ] **未验证：Safari / 移动端**。iframe 同源测量在 `file://` 或跨源时才会退化，
      本部署同源，理论上没问题，但没在真实移动浏览器上打开过
- [ ] **未验证：报告自带 `X-Frame-Options`/CSP 时** —— 那是子文档自己的响应头，
      由我们下发，报告作者改不了

## Loose ends

- [ ] **提交 + 部署**（等用户点头）
- [ ] B 路线：TOC（**注意中文 slug 必须自建**，见上一轮 handoff）、代码复制按钮、打印样式
- [ ] 隔离档位：`HTMLReportSandbox` + `postMessage` 高度上报（当前默认不隔离，是知情选择）
- [ ] 其余见 `CURRENT_TASK.md`「未做」

## Do not repeat

- **全局中间件设的头，按路由例外必须显式删。** `httpx.SecurityHeaders` 给**每个**响应
  塞 `X-Frame-Options: DENY`；忘了 `h.Del` 的表现是**页面 200、日志 0 error、单元测试全绿、
  框内空白**，只有浏览器控制台报错。
- **`default-src 'none'` 会让 `frame-src` 回退成 `'none'`。** 老注释写着
  「deliberately no frame-src, so no iframe can be created」—— 加 iframe 时必须**连注释一起改**，
  否则下一个人会当它是笔误删掉。
- **`/s/` 是一条 JSON-404 兜底**，`/s/...` 下的新端点必须显式注册，否则 iframe 里显示 JSON。
- **别用 `<iframe srcdoc>`**：`srcdoc` 文档**继承父页 CSP**，于是「子文档有自己的宽松策略」
  不成立，内联脚本会被父页 `script-src 'self'` 挡掉。必须用真实子资源才能拿到自己的响应头。
- **别用「只在 CSS 里出现过的类名」当渲染断言。** 我写的
  `strings.Contains(page, "wrap-wide")` 在**所有**页面上都为真（内联 `<style>` 里就有
  `.wrap-wide` 规则），于是「markdown 不加宽列」这条断言**永远不可能失败** ——
  被同一批新测试当场抓住。要匹配**元素**（`<div class="wrap wrap-wide">`），
  不要匹配样式表里的选择器名。
- **改全局变量做反证时，备份要放在会保留的路径。** 本环境 `/tmp` 在 bash 调用之间**不保留**，
  `cp f /tmp/x.bak || cp f $W/x.bak` 这种写法会**两边都失败**（`||` 左边"成功"返回 0），
  于是脚本把文件改坏却没有备份可回滚。反证脚本要用 `trap ... EXIT` + 工作区内的备份目录。
- **`aidoc` 的绊线要双向写。** 原版只在「guide 说关闭 & flag 说开启」时报错；
  这次把 flag 删掉后，单向断言会在「有人改回关闭」时**静默放过**。
- **`/tmp` 在本环境的每次 bash 调用之间不保留**（E2E 的 DB/二进制/截图都因此丢过一次），
  要跨调用保留的临时文件放工作区里被 gitignore 的 `bin/` 下。

## Next step

**已部署，无待办。** 用户要自己发几篇 html 报告试手感，再决定下一轮
（候选：B 路线 TOC/复制/打印、隔离档位、密码保护链接、登录限流）。

回滚材料：二进制 `/data/services/teleport/teleport.prev-20261011-113817`、
数据库 `/data/services/teleport/teleport.db.bak-20261011-113817`（**先停服再拷**）。
无 schema 变更、无数据迁移，回滚二进制即回到旧行为；子端点留着无害。

**本地验证脚手架**（`bin/`，已 gitignore）：`bin/e2e/run.sh` 起本地实例，
`payload-html.json` 是自检用 html 报告（内联脚本 / 同源 fetch / localStorage 三项），
`shot-html-share.png`、`shot-online-html.png` 是分享页截图。
