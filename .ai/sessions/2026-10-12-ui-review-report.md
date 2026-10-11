# Teleport UI 审查报告

按 Vercel **Web Interface Guidelines**（审查时实时拉取）与 Anthropic **accessibility-review**（WCAG 2.1 AA）
对本项目前端与 Go SSR 页面做的审查结果。格式：`file:line`。

审查范围：`web/src/**`、`web/index.html`、`backend/internal/views/*.go`、`backend/internal/aidoc/html.go`。

---

## 一、审查中发现并修复的问题（本轮）

### 1. 相对 URL 的字体链接在深链接下 404 —— 🔴 高

`web/index.html:26` — `href="./fonts/fonts.css"` 改为 `href="%BASE_URL%fonts/fonts.css"`

Go 的 SPA 回退对**任意**未匹配路径返回 `index.html`，于是该文档会在
`/dashboard/reports/<id>`（四段深）下渲染。相对 URL 按**文档 URL** 解析：

| 当前 URL | `./fonts/fonts.css` 解析为 | 结果 |
| --- | --- | --- |
| `/yeciorez/teleport/` | `/yeciorez/teleport/fonts/fonts.css` | ✅ |
| `/yeciorez/teleport/dashboard/reports/rpt_x` | `/yeciorez/teleport/dashboard/reports/fonts/fonts.css` | ❌ 404 |

字体静默回退到系统字体，无 JS 报错、无白屏 —— 只有「零失败请求」断言能发现。
详见 `.ai/pitfalls/cases/2026-10-12-relative-asset-url-under-spa-fallback.md`。

### 2. `no-cache` 无校验器 → 每次整页加载重下 260–830 KB 字体 —— 🟠 中

`backend/internal/spa/spa.go` — 三处：

- `isContentHashed`：新增 `fontsDir` 识别，字体二进制按内容寻址（`<slug>-<NNN>-<sha256[:8]>.woff2`）；
- `serveFile` 调用点：规则 2 原本硬编码 `immutable=false`，字体走的正是这条分支 ——
  **判定函数再正确也永远不会被问到**；
- `etagFor` + `serveIndex` 改用 `ServeContent`：稳定名（`fonts.css`、`index.html`）
  终于能 304，而不是全量重下。

`embed.FS` 的 `ModTime()` 是零值，`ServeContent` 因此不发 `Last-Modified` ——
所以当时的 `no-cache` 实际等于「每次完整重下」。
详见 `.ai/pitfalls/cases/2026-10-12-no-cache-without-a-validator.md`。

### 3. 后端完全没有压缩 —— 🟠 中

`backend/internal/httpx/middleware.go` — 新增 `Compress` 中间件；
`backend/internal/api/router.go:233` — 挂到链路最内层。

`fonts.css` 原始 107,422 B → gzip 32,375 B。刻意只压
`text/*` / `application/json` / `svg`，跳过 Range / HEAD / 非 200 / woff2。

> 第一版实现有真实 bug：在**第一次 `Write`** 时判定，而 `http.ServeContent`
> 先发 header 再发 body —— 客户端拿到 `Content-Length: 107422` 却收到 gzip 字节，
> curl 报 `end of response with 74451 bytes missing`。
> 修法是让 `WriteHeader` 只记录、不刷出，判定完全基于元数据。
> 守卫 `TestCompressSurvivesServeContent` 已反证验过。

### 4. 系统字体栈 —— 用户明确要求

| 位置 | 改动 |
| --- | --- |
| `web/src/app.css` | `--font-sans` / `--font-mono` token 指向 Outfit / Noto Sans SC / JetBrains Mono |
| `backend/internal/views/sharepage.go:164` | `-apple-system,…,sans-serif` → `"Outfit","Noto Sans SC",sans-serif` |
| `backend/internal/views/sharepage.go:181,206,216,222` | `ui-monospace,…` → `"JetBrains Mono",ui-monospace,monospace` |
| `backend/internal/views/handoff.go:104,116` | 同上 |
| `backend/internal/aidoc/html.go:56,63` | 同上 |
| `web/src/share/main.ts` | Mermaid `fontFamily` 对齐 |

### 5. 可访问性缺陷（真浏览器测出）

| 位置 | 问题 |
| --- | --- |
| `web/src/App.vue` | 无跳转链接、无 `<nav>` 语义、无 `<main id="main">` |
| `web/src/App.vue:52` | 导航链接触控高度 20px < WCAG 2.2 SC 2.5.8 的 24px → 加 `py-1.5`（32px） |
| `web/src/views/LoginView.vue:58` | 密码框无 `name`；错误信息无 `role="alert"` |
| `web/src/views/ReportsView.vue:52,74` | 加载态无 `role="status"`；表格无 `<caption>`/`scope`；搜索框无 `<label>` |
| `web/src/views/ReportDetailView.vue:441` | **无效 DOM**：`<dt>` 裸放在 `<dl>` 之外 → 改为 `<h3>` + `<pre aria-labelledby>` |
| `web/src/views/KeyManagementView.vue:914,972,1024,1069,1183` | 5 个对话框无 `role="dialog"` / `aria-modal` / 焦点陷阱 / Escape |

### 6. CSS 层叠：把 `<style scoped>` 搬进全局层导致规则被覆盖 —— 🟠 中

`web/src/app.css`（`@layer components` 内的顺序）— `.login-shell` / `.login-card`
原本写在 `.card` **之前**，两者在全局层里都是单类选择器、特异性相同，
于是 `.card` 的 `p-5` 覆盖了 `.login-card` 的 `padding: 28px`（实测 20px）。

原 `<style scoped>` 能赢，是因为 Vue 编译出的 `.login-card[data-v-…]` 属性选择器
带来了额外特异性 —— 搬进全局层后这份特异性不会跟着走。
构建、typecheck、Go 测试全部通过，只有真浏览器读 computed style 才能发现。
修法：整块移到 `.card` 之后，并在源码里写明顺序是刻意的。

### 7. 缺少 favicon —— 🟡 低

`web/public/` 下没有任何图标，浏览器标签页显示默认空白图标。
新增 `web/public/favicon.svg`（手绘 SVG，不依赖字体），
并在 **4 处**声明：`web/index.html`（用 `%BASE_URL%`）、
`backend/internal/views/sharepage.go:149`、`handoff.go:90`、
`backend/internal/aidoc/html.go:52`（后三者用绝对前缀 URL）。

### 8. 分享页图表字体与正文不一致 —— 🟡 低

`web/src/share/main.ts:46` — Mermaid 的 `fontFamily` 仍是系统字体栈，
而它渲染出的 SVG 就在分享页正文旁边。已改为与页面一致的
`"Outfit", "Noto Sans SC", sans-serif`。

### 9. `/ai` 页 CSP 拦截自托管字体 —— 🔴 高（已上线，静默降级）

`backend/internal/aidoc/html.go:20` — CSP 为
`default-src 'none'; style-src 'unsafe-inline'`，**既没有 `style-src 'self'`**
（导致 `<link rel=stylesheet>` 被直接拒绝），**也没有 `font-src 'self'`**
（即使样式表加载成功，`@font-face` 的抓取仍回退到 `default-src 'none'`）。

页面 200、内容完整可读，**只是悄悄用回系统字体**。上线后由「真浏览器打生产」
抓出。分享页本来就带这两条指令，所以同一个改动两个页面一好一坏 ——
正是这种不对称让它躲过了所有本地检查（本地 stub 不发 CSP 头）。

修法：`style-src 'self' 'unsafe-inline'` + `font-src 'self'`，
**保持 `script-src` 完全不存在**（该页按设计无脚本）。
守卫：`aidoc/html_test.go` 的 `TestCSPAllowsExactlyWhatThePageLoads`
把「页面引用了什么」与「策略允许什么」互校，已反证验过。

### 10. 交互与排版细节

| 位置 | 问题 |
| --- | --- |
| `web/src/app.css`（`.modal-backdrop` / `.modal`） | 缺 `overscroll-behavior: contain`，滚动会穿透到背景页 |
| `web/src/app.css`（`html`） | 缺 `touch-action: manipulation`（300ms 双击缩放延迟）与 `scroll-padding-top`（粘性顶栏遮挡锚点） |
| `web/src/app.css`（`.data-table`） | 缺 `font-variant-numeric: tabular-nums`，数字列宽度抖动 |
| `web/src/app.css`（`h1,h2,h3`） | 缺 `text-wrap: balance`，标题易留孤字 |
| `web/index.html` | 缺 `<meta name="theme-color">`，深色模式下地址栏与页面有色差 |
| `web/src/views/KeyManagementView.vue` ×12、`ReportDetailView.vue` ×5 | 表单控件缺 `name` + `autocomplete="off"`，密码管理器会误填 |
| `web/src/views/ReportsView.vue:62` | 搜索框缺 `spellcheck="false"`（按 ID 搜索时误标红） |
| `web/src/views/KeyManagementView.vue:1170` | 生成给 AI 的提示词 textarea 缺 `spellcheck="false"` |

---

## 二、审查后判定「不适用」的规则（说明理由，不计为问题）

| 规则 | 为什么不适用 |
| --- | --- |
| `user-scalable=no` 反模式 | 未使用；`web/index.html` 的 viewport 不含 `maximum-scale` |
| `transition: all` | 全仓库无 |
| `outline-none` 无替代 | 全仓库无；统一用 `:focus-visible` 加 outline |
| 图片缺尺寸 / `alt` | 项目无 `<img>`（Mermaid 由 SVG 内联渲染） |
| 大列表虚拟化 | 最大列表为密钥表（数十行），未到 50 行阈值 |
| 日期/数字 `Intl.*` | 已用 `toLocaleString('zh-CN', …)`（`web/src/format.ts:10`） |
| `onPaste` + `preventDefault` | 未使用；剪贴板只有 `copyText`（写方向） |
| Hydration Safety | Vue 3 SPA，无 SSR hydration |
| `autoFocus` | 仅 `LoginView` 密码框使用，属"单一主输入"的正当场景 |
| 破坏性操作需确认 | 已满足：删除/撤销走 `window.confirm` |
| 手势无键盘替代 | 无手势交互 |
| `prefers-reduced-motion` | 已全局遵守（`web/src/app.css` 的 `@media` 块） |
| 触控目标 44×44（SC 2.5.5） | 属 **AAA**，非 AA 要求。已满足 AA 的 24×24（实测 0 个低于 24px）。**保留为已知权衡**：数据表在手机上使用紧凑控件是刻意的 |

---

## 三、验证证据

| 项 | 方法 | 结果 |
| --- | --- | --- |
| 类型 | `pnpm run typecheck`（vue-tsc） | ✅ 无错误 |
| Schema | `pnpm run db:check-schema` | ✅ 2 个迁移同步 |
| 后端 | `cd backend && go test ./...` | ✅ 全通过 |
| 静态检查 | `pnpm run backend:vet` | ✅ 无告警 |
| 发布构建 | `pnpm run build:release` | ✅ 25 MB 单文件 |
| 字体实际加载 | 真 Chromium，7 组页面/主题 | ✅ Outfit + Noto Sans SC + JetBrains Mono |
| 控制台 / 网络 | 同上 | ✅ 0 错误、0 失败请求（含深链接） |
| 对比度（token） | 计算 WCAG 比值，浅色 + 深色 | ✅ 全部 ≥4.5:1，最低 4.83:1 |
| 对比度（渲染文本） | DOM 采样 + 有效背景回溯 | ✅ 4 个页面全过 |
| 结构语义 | landmark / 标题层级 / 表格 / 表单标签 | ✅ 0 失败 |
| 焦点可见 | 逐个程序化聚焦 | ✅ 0 个缺 outline |
| 触控目标 | 390px 视口 | ✅ 0 个低于 24×24 |
| 对话框键盘 | Tab ×12 不逃逸、Escape 关闭、`aria-modal` | ✅ 全部通过 |
| 移动端溢出 | 390px | ✅ 水平溢出 0px |
| 缓存头 | 真实二进制 `curl` | ✅ 字体 `immutable`、`fonts.css` ETag → 304 |
| 压缩完整性 | `curl --compressed` 比对字节 | ✅ 107,422 → 107,422 |

复现脚本（当前在 `/tmp/uiverify/`，**未进仓库**以避免引入 playwright 依赖）：

```bash
node /tmp/uiverify/stub-server.mjs   # 端口 8099，托管 backend/internal/webui/dist
node /tmp/uiverify/verify.mjs        # 真浏览器：字体/控制台/网络/键盘/移动端
node /tmp/uiverify/audit.mjs         # WCAG：对比度/结构/焦点/触控目标
node /tmp/uiverify/logincheck.mjs    # 登录页 computed style（抓层叠回归）
```

补充证据（真实二进制，端到端）：

| 项 | 结果 |
| --- | --- |
| 三个 SSR 页面（`/ai`、`/s/{token}`、`/k/{id}`）字体链接 + 图标链接 | ✅ 各 2/2 |
| SSR 页面无系统字体栈 | ✅ 0 处 |
| 分享页 CSP 仍为 `font-src 'self'` | ✅ 未放宽 |
| favicon 类型 / ETag / 304 | ✅ `image/svg+xml` + 304 |
| 深链接 shell 声明图标（前缀绝对） | ✅ |
| 登录页 computed style（padding/maxWidth/flex/渐变/字体） | ✅ 24/24 |
| 既有契约（`/api/*` JSON、未知分享 token 404） | ✅ 未变 |

**生产真浏览器（`/tmp/uiverify/prodcheck.mjs`，走 Caddy + Cloudflare + TLS）：37/37**

| 检查 | 结果 |
| --- | --- |
| 4 组页面/主题 + `/ai` | ✅ 200、字体实际加载、0 控制台错误、0 失败请求 |
| 字体切片实际传输 | ✅ 494 KB / 9 片（证明 `unicode-range` 生效，不是整包下载） |
| `/ai` 三个字族 | ✅ Outfit + Noto Sans SC + JetBrains Mono |
| 资源 URL 携带路由前缀 | ✅ `href="/yeciorez/teleport/fonts/fonts.css"` |
| 线上缓存头 | ✅ `share.js`/shell `no-cache`、哈希资源 `immutable` |
| 字体内容寻址 | ✅ `outfit-000-….woff2` → `immutable` |
| `fonts.css` ETag / gzip | ✅ 条件请求 304；gzip 解码 107,422 字节与本地一致 |
| 分享页 CSP（真实 token） | ✅ `font-src 'self'` + `style-src 'self' 'unsafe-inline'` |

> 这一轮最有价值的教训：**本地 stub 不发 CSP 头**，所以整个本地验证套件
> 对「策略拦截资源」这类缺陷是完全盲的 —— 只有真浏览器打真环境才抓得到。
> 它确实抓到了一个已上线的真实 bug（第 9 条）。

---

## 四、遗留事项

1. **未 `git commit`** —— AGENTS.md 要求提交前先问人类。
2. **未从大陆观测点验证**（AGENTS.md 完成定义第 3 条）。本轮字体为自托管、
   不引入任何外域依赖，但线上仍需实测确认。
3. 浏览器验证脚本在 `/tmp`，是否收进 `scripts/` 待人类决定。
4. Go SSR 页仍是各自内联的手写 CSS，未与 SPA 统一（两者构建模型不同，见
   `.ai/decisions/2026-10-12-tailwind-css-migration.md` 的 Revisit 条件）。
