# Teleport

个人 AI 报告展示与时效分享平台 — 基于 Cloudflare 全家桶（Workers + D1 + Workers Assets），
无需传统服务器、免备案。

在终端 / 工作框中完成复杂工作（安全渗透、架构设计、开发进度等）后，通过一个 API 调用
把报告发布为在线网页，并得到一条**带时效的分享链接**。

**核心能力**

| 能力 | 说明 |
|---|---|
| 报告发布 | `POST /api/reports` 上报 Markdown，自动生成分享链接 |
| 时效分享 | 每条链接独立设置有效期（1 小时 ~ 永不过期），可随时禁用 / 吊销 |
| 内容渲染 | Markdown 服务端渲染 + 代码语法高亮 + Mermaid 图表（客户端懒加载） |
| 管理面板 | Vue 3 SPA，登录后查看报告、管理分享链接、复制 / 调期 / 禁用 |
| 安全 | `html:false` 转义原始 HTML、严格 CSP、常量时间鉴权比较、失效链接语义化 404/410 |

---

## 目录结构

```
teleport/
├── wrangler.toml                 # Worker + D1 + Assets 配置（含 staging/production 环境）
├── schema.sql                    # D1 完整 schema 快照（可直接 --file 执行）
├── migrations/
│   └── 0001_init.sql             # 增量迁移（wrangler d1 migrations apply 使用）
├── tsconfig.json                 # Worker 侧严格模式 TS 配置
├── tsconfig.web.json             # 前端（Vue SFC）TS 配置
├── vite.config.ts                # Vite 构建配置（root=web，输出到 public/）
├── worker-configuration.d.ts     # 由 `pnpm run cf-typegen` 生成，勿手改
├── .dev.vars.example             # 本地密钥模板（.dev.vars 已 gitignore）
│
├── scripts/
│   ├── hash-password.mjs         # 生成 ADMIN_PASSWORD_HASH
│   └── check-schema-sync.mjs     # 校验 schema.sql 与迁移文件未漂移
│
├── src/                          # ── Cloudflare Worker 后端 ──
│   ├── index.ts                  # Worker 入口：中间件编排 + 路由挂载 + 资源兜底
│   ├── types.ts                  # 业务类型（ReportRow / ShareTokenRow / ...）
│   ├── env.d.ts                  # 合并 secrets 到生成的 Env 接口
│   │
│   ├── middleware/
│   │   └── index.ts              # requestId / CORS / 安全头 / Bearer 鉴权 / Session / 错误映射
│   │
│   ├── lib/
│   │   ├── config.ts             # 绑定、变量与密钥的读取与校验
│   │   ├── errors.ts             # ApiError + 统一响应封套
│   │   ├── util.ts               # UUID / 高熵 token / 时间 / 常量时间比较
│   │   └── validate.ts           # 请求体校验（无外部依赖）
│   │
│   ├── render/
│   │   └── markdown.ts           # Markdown→HTML（markdown-it, html:false）+ 语法高亮
│   │
│   ├── services/
│   │   ├── reports.ts            # reports + share_tokens 的全部 SQL
│   │   ├── auth.ts               # 会话 Cookie 签名/校验 + Agent Bearer 校验
│   │   └── password.ts           # PBKDF2 口令哈希与校验
│   │
│   └── routes/
│       ├── reports.ts            # POST /api/reports            (Bearer)
│       ├── share.ts              # GET  /api/share/:token       (公开)
│       ├── revoke.ts             # POST /api/share/:token/revoke(Bearer)
│       ├── admin.ts              # /api/admin/*                 (Session)
│       └── sharePage.ts          # GET  /s/:token 服务端渲染公开页
│
├── web/                          # ── 前端源码 ──
│   ├── index.html                # 管理面板 SPA 入口
│   ├── public/_headers           # Workers Assets 响应头（share.js no-cache）
│   └── src/
│       ├── main.ts               # createApp + 路由挂载
│       ├── App.vue               # 顶栏 / 登录态分流 / 全局 401 处理
│       ├── api.ts                # 类型化 API 客户端（统一解包响应封套）
│       ├── format.ts             # 时间 / 过期 / 复制等展示工具
│       ├── styles.css            # 轻量样式（亮/暗色，无 CSS 框架）
│       ├── router/index.ts       # /dashboard 路由 + 登录守卫
│       ├── share/
│       │   └── main.ts           # 分享页客户端：Mermaid 懒加载 + 过期倒计时
│       └── views/
│           ├── LoginView.vue     # 密码登录
│           ├── ReportsView.vue   # 报告列表 + 客户端搜索
│           └── ReportDetailView.vue  # 报告详情 + 分享 Token 管理
│
└── public/                       # Workers Assets 静态目录（`pnpm run build` 的产物）
    ├── index.html                # 管理面板 SPA（由 Vite 生成，勿手改）
    ├── _headers                  # 响应头规则（share.js -> no-cache）
    └── assets/
        ├── share.js              # 分享页客户端（固定文件名 + 内容哈希）
        └── *-[hash].js           # 面板分包与 Mermaid 懒加载分块
```

> `public/` 下的 `index.html` 与 `assets/` 均为 `pnpm run build` 的**生成产物**，请勿手改。
> `pnpm run deploy` 会先构建再部署，因此无需手动提交产物；若要接入 CI，也可将 `public/` 加入 `.gitignore`。

### 为什么这样分层

| 目录 | 职责 | 约束 |
|---|---|---|
| `routes/` | HTTP 语义：解析、状态码、响应 | 不写 SQL |
| `services/` | 业务逻辑与持久化 | 不感知 `Request`/`Response` |
| `lib/` | 无状态工具与校验 | 无副作用 |
| `middleware/` | 横切关注点 | 不包含业务分支 |

好处是后续接入前端框架、替换渲染管线、或增加 KV 缓存时，改动都被限制在单层内。

---

## 接口一览

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| `POST` | `/api/reports` | `Authorization: Bearer <AGENT_SECRET_KEY>` | AI 上报报告；携带 `autoShareHours` 时同步生成分享链接 |
| `GET` | `/api/reports/:id` | Bearer | 读取单篇报告源文 |
| `GET` | `/api/share/:token` | 公开 | 只读获取报告；失效/过期分别返回 404 / 410 |
| `POST` | `/api/share/:token/revoke` | Bearer | 手动禁用链接（幂等） |
| `GET` | `/s/:token` | 公开 | 服务端渲染的分享页面 |
| `GET` | `/api/health` | 公开 | 健康检查（含 D1 探活） |
| `POST` | `/api/admin/login` | 口令 | 登录并下发签名 Session Cookie |
| `POST` | `/api/admin/logout` | 公开 | 清除 Session |
| `GET` | `/api/admin/session` | Session | 检查会话状态 |
| `GET` | `/api/admin/reports` | Session | 报告列表（`limit` / `offset` / `category`） |
| `GET` | `/api/admin/reports/:id` | Session | 报告详情 + 其全部分享 Token |
| `POST` | `/api/admin/reports/:id/shares` | Session | 为报告新建分享 Token（`expiresInHours`） |
| `PATCH` | `/api/admin/shares/:token` | Session | 调整过期时间 / 启用禁用 |
| `DELETE` | `/api/admin/shares/:token` | Session | 吊销链接 |

所有响应统一为：

```jsonc
{ "ok": true,  "data": { /* ... */ }, "requestId": "..." }
{ "ok": false, "error": { "code": "gone", "message": "This share link has expired" }, "requestId": "..." }
```

### 上报示例

```bash
curl -X POST https://reports.example.com/api/reports \
  -H "Authorization: Bearer $AGENT_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "内网渗透测试报告 #12",
    "category": "pentest",
    "format": "markdown",
    "content": "# 概览\n\n```mermaid\ngraph TD; A-->B;\n```\n",
    "metadata": { "target": "10.0.0.1", "cve": ["CVE-2024-1234"], "status": "done" },
    "autoShareHours": 24
  }'
```

返回：

```json
{
  "ok": true,
  "data": {
    "id": "4bc4200b-...",
    "share": {
      "token": "R1Ku0lg0YNoMXVfJgGuzYA",
      "expires_at": 1791390719094,
      "url": "https://reports.example.com/s/R1Ku0lg0YNoMXVfJgGuzYA"
    }
  }
}
```

---

## 数据模型

时间统一为 **epoch 毫秒**（`INTEGER`，与 `Date.now()` 对齐）；`expires_at = 0` 表示永不过期。

### `reports`

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | TEXT PK | UUID v4 |
| `title` | TEXT | 标题 |
| `category` | TEXT | `pentest` / `architecture` / `progress` / ... 默认 `general` |
| `format` | TEXT | `markdown`（默认）或 `html` |
| `content` | TEXT | 正文（含 Markdown / Mermaid） |
| `metadata` | TEXT | JSON（`CHECK json_valid`） |
| `created_at` / `updated_at` | INTEGER | 毫秒时间戳 |

### `share_tokens`

| 列 | 类型 | 说明 |
|---|---|---|
| `token` | TEXT PK | 128 位高熵 URL-safe 随机串 |
| `report_id` | TEXT FK | → `reports.id`，`ON DELETE CASCADE` |
| `created_at` | INTEGER | 毫秒时间戳 |
| `expires_at` | INTEGER | 0 = 永不过期 |
| `is_active` | INTEGER | 1 启用 / 0 手动禁用 |
| `view_count` | INTEGER | 访问计数 |

一篇报告可以持有**任意多条**互相独立的失效链接。

---

## 快速开始

> **包管理器：pnpm**（已在 `package.json` 的 `packageManager` 字段锁定 `pnpm@10.33.0`）。
> 请勿使用 npm / yarn，仓库只保留 `pnpm-lock.yaml` 单一锁文件。

```bash
pnpm install

# 1. 创建 D1 数据库，把输出的 database_id 填进 wrangler.toml
pnpm exec wrangler d1 create teleport-db

# 2. 本地建表
pnpm run db:migrate:local

# 3. 配置本地密钥
cp .dev.vars.example .dev.vars
pnpm run hash-password 'your password'   # 结果填入 ADMIN_PASSWORD_HASH

# 4. 启动 Worker（http://localhost:8787）
pnpm run dev

# 5. 可选：启动前端开发服务器（http://127.0.0.1:5173，带 HMR，/api 自动代理到 8787）
pnpm run dev:web
```

### 关于 pnpm 的两点注意

**1. 构建脚本需要显式放行**（pnpm 10 默认拦截 `postinstall`，是供应链安全特性）。
本项目已在 `pnpm-workspace.yaml` 的 `onlyBuiltDependencies` 中按**精确包名**放行四个必需的包：

| 包 | 为什么必须放行 |
|---|---|
| `esbuild` | Vite 依赖的平台二进制 |
| `workerd` | `wrangler dev` 所需的 Workers 运行时 |
| `sharp` | miniflare/wrangler 使用的原生图像库 |
| `blake3-wasm` | miniflare 的构建步骤 |

若 `pnpm install` 后提示 `Ignored build scripts`，说明放行清单缺失或包名有变，
用 `pnpm approve-builds` 查看并按需补入 —— **不要**用通配符一次性放行全部。

**2. 严格依赖隔离**：pnpm 不会把未声明的传递依赖提升到 `node_modules` 顶层。
好处是杜绝幽灵依赖；代价是若某处 import 了未在 `package.json` 声明的包，
npm 下可能"碰巧能跑"，pnpm 下会立刻报错。这是**期望行为**，不要用
`node-linker=hoisted` 去绕过它。

> 日常开发建议同时开两个终端：`pnpm run dev`（Worker + D1）与 `pnpm run dev:web`（Vue HMR）。
> 只跑 `pnpm run dev` 也可以，此时访问的是 `pnpm run build` 产出的静态面板。

### 部署

> **无头 / 远程主机上的认证**
> `wrangler login` 会在本机 `http://localhost:8976` 开一个 OAuth 回调服务器，
> 无头机收不到回调，因此**用不了**。二选一：
>
> **方式 A — 设备码**（交互式，码只有 5 分钟有效期）
> ```bash
> pnpm exec wrangler login --device --browser=false
> # 然后在任意能上网的浏览器打开提示的 URL 并输入设备码
> ```
>
> **方式 B — API Token**（推荐，无需竞速，也适合 CI）
> 在 https://dash.cloudflare.com/profile/api-tokens 建 token，权限：
> `Workers Scripts:Edit`、`D1:Edit`、`Workers Routes:Edit`、`Account Settings:Read`。
> ```bash
> export CLOUDFLARE_API_TOKEN=...
> export CLOUDFLARE_ACCOUNT_ID=...
> ```
> Wrangler 会自动读取这两个环境变量，之后所有命令都无需登录。

```bash
# 1. 建生产库（需先把 database_id 填进 [env.production.d1_databases]）
pnpm exec wrangler d1 create teleport-db-prod

# 2. 建表（--env production 不能漏，否则会打到别的库）
pnpm exec wrangler d1 migrations apply DB --remote --env production

# 3. 三个密钥，逐个 --env production
pnpm exec wrangler secret put AGENT_SECRET_KEY    --env production
pnpm exec wrangler secret put SESSION_SECRET      --env production
pnpm run hash-password '你的面板密码'              # 复制输出
pnpm exec wrangler secret put ADMIN_PASSWORD_HASH --env production

# 4. 构建 + 部署
pnpm run deploy

# 5. 把部署输出的 URL 填回 PUBLIC_BASE_URL，再部署一次
pnpm run deploy
```

> **第 5 步为何要部署两次**：`PUBLIC_BASE_URL` 决定 API 返回的分享链接前缀。
> 首次部署前你不知道真正的 `*.workers.dev` 子域名，只能先占位；
> 拿到真实地址后填回去重部署，分享链接才是对的。
>
> **关于域名**：`workers.dev` 子域名免费、免备案，够用就不必买域名。
> 想挂自定义域名，取消 `[[env.production.routes]]` 注释并把
> `workers_dev` 改为 `false` —— **两者必须同时改**，否则 Worker 没有任何可访问入口。

| 命令 | 作用 |
|---|---|
| `pnpm run dev` | Worker 开发服务器（含本地 D1） |
| `pnpm run dev:web` | Vite 前端开发服务器（HMR + `/api` 代理） |
| `pnpm run build` | 构建 Vue 面板到 `public/` |
| `pnpm run deploy` | 构建 + 部署到 production |
| `pnpm run deploy:staging` | 构建 + 部署到 staging |
| `pnpm run typecheck` | Worker 类型检查（`tsc --noEmit`） |
| `pnpm run typecheck:web` | 前端类型检查（`vue-tsc`） |
| `pnpm run typecheck:all` | 两侧一起检查 |
| `pnpm run cf-typegen` | 依据 wrangler.toml 重新生成绑定类型 |
| `pnpm run db:migrate:local` | 本地 D1 迁移 |
| `pnpm run db:migrate:remote` | 生产 D1 迁移（`--env production`） |
| `pnpm run db:migrate:staging` | staging D1 迁移 |
| `pnpm run db:check-schema` | 校验 `schema.sql` 与迁移文件一致 |
| `pnpm run hash-password` | 生成管理员口令哈希 |

> 迁移脚本用 **binding 名 `DB`** 而非数据库名，这样 `--env` 会自动解析到该环境
> 对应的 `database_id`。若写死数据库名，跨环境时容易迁移到错误的库。

---

## 渲染管线（Markdown + Mermaid）

分享页 `GET /s/:token` 采用**混合渲染**：Markdown 在 Worker 服务端渲染，Mermaid 在客户端懒加载。
这个划分不是随意选的，而是由体积决定的。

### 为什么这样拆分

| 环节 | 位置 | 原因 |
|---|---|---|
| Markdown → HTML | **Worker 服务端** | markdown-it 仅 ~116 KB；且 `html:false` 让原始 HTML 天然被转义，是整条链路最关键的一道 XSS 防线 |
| 代码语法高亮 | **Worker 服务端** | 只需精选语言子集（26 种），避免引入全部 190+ 语言 |
| Mermaid 图表 | **客户端懒加载** | Mermaid 解包后 **121 MB**，打包后仍有 ~5 MB 分块；只在文档真的含图时才 `import()` |

实测产物：`assets/share.js` 仅 **1.3 KB gzip**，Mermaid 拆成独立分块
（最大 `elk` 455 KB gzip、`cytoscape` 137 KB gzip），纯文字报告完全不加载它们。
若把 Mermaid 静态引入，**每一篇纯文本报告都会被迫下载约 1 MB**。

### Markdown 渲染（`src/render/markdown.ts`）

- `MarkdownIt({ html: false })` —— 报告正文里的 `<script>`、`<img onerror>` 一律被转义为文本。
- 依赖 markdown-it 内置的 `validateLink` 拦截 `javascript:` / `vbscript:` / 危险 `data:` 协议，不自己写过滤器。
- 外链自动加 `target="_blank" rel="noopener noreferrer nofollow"`，防止 `window.opener` 反向控制。
- 语法高亮使用 `highlight.js/lib/core` + **精选 26 种语言**（含别名 `ts`/`sh`/`yml`/`ps1` 等）。
  无语言标记或高亮失败时，**退回转义后的纯文本**，绝不输出原始源码。

### Mermaid 渲染（`web/src/share/main.ts`）

- 仅当页面存在 `.mermaid` 元素时才 `import('mermaid')`。
- `securityLevel: 'strict'` —— Mermaid 会在 iframe 中沙箱化渲染，并禁用 `click` 指令。
  报告内容是**不可信输入**，这一项不是可选项。
- 用 `Promise.allSettled` 逐个渲染：单个图表语法错误不会导致整页白屏，
  失败的那个会把源码显示出来便于排查。

### 内容安全策略（CSP）

```
default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
img-src 'self' data:; font-src 'self'; connect-src 'self';
base-uri 'none'; form-action 'none'; frame-ancestors 'none'; upgrade-insecure-requests
```

- **`script-src 'self'` 且页面内零内联脚本** —— 因此无需维护 nonce，内容路径上没有任何可执行内联 JS。
- 唯一的放宽是 `style-src 'unsafe-inline'`：Mermaid 运行时会把 `<style>` 注入它生成的 SVG。
  这**不能**执行脚本。
- `img-src 'self' data:` 让报告里的图片只能来自本站或 data URI，无法用作外链追踪像素。

### 缓存策略

`web/public/_headers` 中把 `/assets/share.js` 设为 `no-cache`：它必须用**固定文件名**
（Worker 服务端渲染时硬编码引用），所以不能靠内容哈希失效，只能每次回源校验。
其余资源由 Vite 加内容哈希，可长期缓存。

---

## 管理面板（Vue 3）

### 技术选型

用 **Vue 3 + Vite 纯 SPA**，而非 Nuxt。原因：面板是私有页面，不需要 SEO / SSR；
公开分享页已经由 Worker 服务端渲染（这样才能返回真正的 404/410）。
引入 SSR 框架只会增加构建复杂度而没有任何收益。

Vite 的 `root` 指向 `web/`，`build.outDir` 直接输出到 `public/`，
交给 Workers Assets 的 `[assets]` 绑定伺服。产物按路由分包，首屏 gzip 约 37 KB。

### 路由

| 路径 | 视图 | 说明 |
|---|---|---|
| `/dashboard` | `LoginView` | 密码登录 |
| `/dashboard/reports` | `ReportsView` | 报告列表 + 客户端搜索 |
| `/dashboard/reports/:id` | `ReportDetailView` | 报告详情 + 分享 Token 管理 |

`wrangler.toml` 中 `not_found_handling = "single-page-application"` 保证深链接可刷新。
为让 `/`、`/dashboard` 与任意深链接都能正确解析，路由使用 `createWebHistory('/')`
并把 `/dashboard` 写入各 route path（而非作为 history base）。

### 功能

- 登录 / 登出，全局 401 自动跳回登录页并保留 `?redirect=`。
- 报告列表：标题、分类、格式、相对时间；支持按标题/分类/ID 过滤。
- 报告详情：元数据、正文源码、分享链接管理 ——
  新建（1 小时 / 6 小时 / 24 小时 / 7 天 / 30 天 / 永不过期）、复制链接、
  调整有效期、禁用 / 恢复、吊销。

### 前端安全边界

路由守卫（`router.beforeEach`）**只是体验优化**，不是安全措施 ——
真正的鉴权在服务端 `requireSession`。客户端可以绕过路由守卫，
但绕不过 `/api/admin/*` 的 Cookie 校验。

---

## 安全设计要点

- **Agent 鉴权**：`POST /api/reports` 与所有写操作要求 Bearer Token，比较使用常量时间函数。
  该中间件挂在**路由器级别**（`reports.use('*', ...)`），因此新增路由默认受保护而非默认公开。
- **失效链接语义**：未知 token 与被吊销 token 同样返回 **404**，避免通过状态码区分
  “从未存在”与“已被撤销”；仅**已过期**返回 **410**。分享页同样服务端返回 404/410，
  而不是先给 200 再由前端报错。
- **分享页面不缓存**：`Cache-Control: no-store`，防止已吊销链接残留在中间缓存。
- **XSS**：Markdown 渲染前不内联原始 HTML；`format: 'html'` 目前**关闭**
  （`HTML_MOUNT_ENABLED = false`）。将来开启必须先接入 DOMPurify + 严格 CSP。
  页面内嵌 JSON 已转义 `<`，避免 `</script>` 逃逸。
- **管理面板**：HttpOnly + Secure + SameSite=Lax 的签名 Cookie（HMAC-SHA256），
  口令使用 PBKDF2-HMAC-SHA256（210,000 次迭代）。也可用 Cloudflare Access 前置，
  此时接受 `Cf-Access-Authenticated-User-Email` 头。
- **安全响应头**：`nosniff`、`X-Frame-Options: DENY`、`no-referrer`、`noindex`。
- **CORS**：仅回显 `PUBLIC_BASE_URL` 配置的源，绝不反射任意 Origin。

---

## 后续路线

1. **HTML 安全渲染**：DOMPurify + nonce CSP 后开启 `format: 'html'`。
2. **运维**：为 `share_tokens` 增加定时清理（Cron Triggers + `purgeExpiredTokens`）。
3. **面板增强**：报告删除、分页、按分类筛选服务端化、新建报告入口。
4. **渲染增强**：代码块复制按钮、Mermaid 图表导出 SVG/PNG、目录（TOC）锚点。
5. **可选**：KV 缓存热点分享页、R2 存放附件、自定义域名。

---

## 已验证行为

### 生产环境（已实际部署并验证）

部署地址：`https://teleport.zeroicey-hp.workers.dev`

由于开发主机位于中国大陆网络，`*.workers.dev` 整个域名不可达（DNS 污染 + SNI 重置），
因此**无法从部署机上直接验证线上服务**。改用 Cron Trigger + Service Binding 在
Cloudflare 网络内部发起探测，结果写入 D1 再通过 API 读回，实测结论：

| 探测 | 结果 |
|---|---|
| `GET /api/health` | 200，`database: "ok"`（D1 连通正常） |
| `POST /api/reports` 无令牌 | 401 `unauthorized` |
| `POST /api/reports` 带令牌 | 201，返回分享链接 |
| `GET /api/share/:token` | 200，内容完整回读 |
| `GET /s/:token` | 200，服务端渲染 |
| `GET /s/未知token` | 404 |
| `GET /dashboard` | 200，SPA 外壳 |
| 渲染检查 | Mermaid 节点存在 ✓ 语法高亮生效 ✓ `<script>` 已转义 ✓ 无原始脚本泄漏 ✓ |
| 1 MiB 大报告 | 上报 201、渲染 200（未触发 CPU 1102） |
| 超过 `MAX_CONTENT_BYTES` | 413 `payload_too_large`，在渲染前拦截 |

> 若在无头机上部署，注意 `wrangler login` 的 OAuth 回调固定指向 `localhost:8976`，
> 远端收不到；请用 `--device` 设备码或 `CLOUDFLARE_API_TOKEN`。
>
> 另有两个易踩的坑：**同一 zone 内的 Worker 之间不能用 `fetch()` 互调**（报 error 1042），
> 必须用 Service Binding；`wrangler d1 create` 建议的 binding 名是自动派生的，
> 但本项目代码读的是 `env.DB`，必须写回 `DB`。

### 后端（`wrangler dev` + 真实 D1 实例）

- `schema.sql` 可独立在 `sqlite3` 执行；`wrangler d1 migrations apply --local` 成功应用 13 条语句（删库重跑亦通过）。
- 上报 → 自动生成分享链接 → 公开读取 → 计数递增的完整链路。
- 鉴权：缺失 / 错误 / 非 Bearer 令牌均返回 401；正确令牌返回 201。
- 吊销：未鉴权 401；已吊销与未知 token 均返回 404；重复吊销幂等返回 200。
- 过期：`expires_at` 置为过去后，`/api/share` 与 `/s/:token` 均返回 410。
- 管理面板：错误口令 401、伪造 Cookie 401、正确登录可列出报告、登出清除 Cookie。
- 分享 Token 管理：新建 / 调整过期 / 禁用 / 恢复 / 删除全部生效。
- 静态资源：`/`、`/dashboard` 与 SPA 深链接返回 200；`/api/*` 未匹配路径返回 JSON 404 而非 SPA HTML。
- CORS：仅回显配置的源，`Origin: https://evil.example` 不返回 ACAO 头。
- `pnpm run typecheck`（Worker）与 `pnpm run typecheck:web`（Vue）均无错误；`pnpm run db:check-schema` 通过。
- `pnpm install --frozen-lockfile` 在干净目录可复现安装，并构建成功。

### 前端

构建与类型检查已通过（`pnpm run build`、`pnpm run typecheck:web`）。
界面交互请按「快速开始」启动后自行点击验证 —— 本项目不包含自动化 UI 测试。
