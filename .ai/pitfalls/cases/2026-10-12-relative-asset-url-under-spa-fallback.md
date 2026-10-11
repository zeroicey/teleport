# 相对路径的静态资源 URL，在 SPA 回退下按路由深度解析

**Severity:** 🔴 high（线上必然失效，但只在深链接刷新时） · **First hit:** 2026-10-12 · **Hits since:** 1

## 症状

自托管字体接入后，在 `web/index.html` 里写：

```html
<link rel="stylesheet" href="./fonts/fonts.css" />
```

浅层路由（`/yeciorez/teleport/`、`/dashboard`）看起来完全正常，字体加载成功。

但从**深链接刷新**时字体全部 404：

```
GET /yeciorez/teleport/dashboard/reports/5a4f6490-.../fonts/fonts.css
  → 404
```

页面本身仍然是 200、仍然渲染、仍然能登录 —— 只是**字体静默回退到系统字体**。
没有控制台报错（CSS 请求失败不抛 JS 异常），没有白屏，肉眼在开发机上
（系统有 Noto Sans SC）几乎看不出差别。

## 根因

`backend/internal/spa/spa.go` 的规则 4：**任何未匹配的路径都返回 `index.html`**。
这是 SPA 回退，本身正确。

于是 `index.html` 会在**任意深度**的 URL 下被渲染。而相对 URL 是按
**当前文档的 URL** 解析的，不是按"这个文件在构建里放在哪"：

| 当前 URL | `./fonts/fonts.css` 解析成 | 结果 |
| --- | --- | --- |
| `/yeciorez/teleport/` | `/yeciorez/teleport/fonts/fonts.css` | ✅ |
| `/yeciorez/teleport/dashboard` | `/yeciorez/teleport/fonts/fonts.css` | ✅ |
| `/yeciorez/teleport/dashboard/reports/rpt_x` | `/yeciorez/teleport/dashboard/reports/fonts/fonts.css` | ❌ 404 |

只有**最后一段**被当作文件名替换掉。报告详情路由是四段深，所以必然踩中。

而 `spa.go` 规则 3 还明确把 `api/`、`s/`、`k/` 前缀排除在回退之外，
说明作者已经知道"回退会吞掉一切"这件事 —— 只是**静态资源引用**这条路径没人想到。

## 为什么没有测试拦住

- `spa_test.go` 测的是**服务端**对某个路径返回什么，它无法知道 HTML 里的
  相对 URL 会被浏览器解析成什么。
- 前端测试（`typecheck`）不解析 HTML。
- 构建产物检查（`pnpm run build`）只验证文件被复制，不验证引用可达。
- **开发时也不容易发现**：`pnpm run dev:web` 下 Vite 的 dev server 对未知路径
  的处理与 Go 的回退不同，且开发者通常停在浅层路由。

一句话：**这是一个只有"真实浏览器 + 深链接 + 检查网络请求"才能暴露的问题。**

## Fix

**已修（2026-10-12）**：改用 Vite 的 HTML 环境变量替换，让 URL 由前缀推导：

```html
<link rel="stylesheet" href="%BASE_URL%fonts/fonts.css" />
```

`%BASE_URL%` 展开为 `vite.config.ts` 的 `base`，也就是 `ROUTE_PREFIX` ——
**前缀仍然只写一处**，而且它在任意深度都是绝对路径。

已核验构建产物：

```
dist/index.html → href="/yeciorez/teleport/fonts/fonts.css"
```

并且真实二进制下，深链接回退返回的 shell 里也是这条绝对 URL。

**Go SSR 页同理，但用的是另一条路**：分享页 `/s/{token}` 与交接页 `/k/{id}`
都是两段深，模板里必须用 `cfg.AssetBaseURL` 拼绝对 URL，不能写 `./`：

```go
b.WriteString(`<link rel="stylesheet" href="` + markdown.Escape(assetBase) + `/fonts/fonts.css" />`)
```

## Guard

1. **在 SPA 回退的场景下，HTML 里引用的静态资源一律用绝对前缀 URL。**
   相对路径在"任何未匹配路径都返回同一个文档"的服务端模型下没有意义。
2. `web/index.html` 里那段注释写明了**为什么**必须用 `%BASE_URL%`，
   而不是"这里要用 %BASE_URL%"——搬注释时搬理由。
3. 验收必须包含**深链接刷新**：本轮的真浏览器验证脚本对每个页面都检查
   `failed requests`，而深链接（`/dashboard/reports/rpt_a1b2c3d4e5f60718`）
   是其中一个用例。**只有断言"零失败请求"才能发现 404 的字体。**
4. 这条陷阱对**任何**将来加进 `index.html` 的资源都成立：
   favicon、manifest、preload、图片。加一个就检查一次。

## 一句话

> 当服务端对**任意**路径都返回同一个文档时，那个文档里的**相对 URL 就是错的** ——
> 它会在你最深的那个路由上失效，而且失败得毫无声音。
