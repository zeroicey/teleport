# Architecture

## 1. Purpose

Teleport 是**个人 AI 报告展示与时效分享**平台。AI 在终端/工作框里完成复杂工作（安全渗透、
架构设计、开发进度）后，用一次 `POST /api/reports` 把 Markdown 上报为在线网页，拿到一条
**带有效期的分享链接**（1 小时 ~ 永不过期，可随时禁用/吊销）。

面向单用户（个人），不做多租户、不做协作、不做评论。显式不做的事：不渲染原始 HTML
（`format: 'html'` 未开放）、不做全文检索、不做图片上传、不做报告删除 API。

关键取舍：**省事 > 功能**。部署产物是单个静态 Go 二进制（前端用 `//go:embed` 打进去），
服务器上不需要 Node、不需要 Go、不需要与二进制同步的静态目录。

## 2. Stack

| Layer | Technology | Version | Why this one |
| --- | --- | --- | --- |
| 前端 | Vue 3 + Vite + pnpm | Vue 3.5 · Vite 8 | 用户明确要 Vue（否决了 SvelteKit/Astro/Next）；SPA 而已，不需要 SSR 框架 |
| 后端 | Go（stdlib `net/http`，无 Web 框架） | go 1.26 | 单静态二进制、交叉编译、systemd 友好；`ServeMux` 的 `{param}` + `PathValue` 够用 |
| 数据库 | SQLite via `modernc.org/sqlite` | 1.60.1 | 纯 Go 驱动，**无 cgo** → `CGO_ENABLED=0` 才能成立（见 §5.6） |
| 渲染 | goldmark + Chroma | 1.8.6 / 2.27.0 | 服务端 Markdown 渲染；`html:false` 天然 XSS 安全 |
| 托管 | systemd + Caddy（docker 容器） | — | 服务器 3.5 GiB 内存、**没有 docker compose**，只有 `docker run` |
| 图表 | Mermaid（客户端懒加载） | 12.1.0 | 打包后 5.3 MB，绝不能进 Worker/二进制；只在分享页动态 import |

> 历史：本项目原为 Cloudflare 全家桶（Workers + D1 + Hono + Vue），**该方案已退役**。
> 直接原因是 CF 免费版 anycast IP 在大陆被 TCP 层封锁 —— 见 §6 与
> `.ai/pitfalls/cases/cloudflare-free-ip-blocked-in-mainland.md`。

## 3. Repository map

| Path | Responsibility | Touches |
| --- | --- | --- |
| `backend/main.go` | 进程入口、信号处理、`hash-password`/`migrate`/`version` 子命令 | config, store |
| `backend/internal/config/` | `.env` 读取与校验；**前缀的唯一来源**（`Route()`、`AppBaseURL()`） | 无 |
| `backend/internal/api/` | 路由挂载与 handler，全部挂在 `cfg.RoutePrefix` 下 | config, store, views, markdown, aidoc |
| `backend/internal/httpx/` | 中间件链、响应封套、Session HMAC、Bearer 常量时间比较 | 无 |
| `backend/internal/password/` | PBKDF2-HMAC-SHA256 | 无 |
| `backend/internal/store/` | 全部 SQL、内嵌迁移、UUID/token 生成 | sqlite |
| `backend/internal/markdown/` | goldmark + Chroma 渲染器 | 无 |
| `backend/internal/validate/` | 请求体校验（**复刻 JS 语义**，见 §5.4） | 无 |
| `backend/internal/views/` | 分享页 HTML + CSP 常量 | 无 |
| `backend/internal/spa/` | 内嵌静态托管：SPA 回退、缓存头、**路径穿越防护** | 无 |
| `backend/internal/aidoc/` | 给 AI 读的使用说明（`guide.md` + 占位符替换） | config, markdown |
| `backend/internal/webui/` | `//go:embed dist/`，构建标签 `embed_frontend` 切换桩实现 | 无 |
| `web/src/` | Vue 面板（登录/列表/详情）+ 分享页客户端入口 | api.ts 封套 |
| `scripts/` | `build.sh`（发布构建）、`check-schema-sync.mjs` | — |
| `schema.sql` | DDL 快照，与内嵌迁移逐字节一致（CI 校验） | — |

## 4. Core topology

```
浏览器 ──https──> api.hcyj.xyz/yeciorez/teleport/...
                        │
                  Caddy(:443, docker)  handle /yeciorez/teleport*   ← 保留前缀，不 strip
                        │
                        ▼
                  Go(:8788, 仅监听 docker 网桥 172.17.0.1) ─> SQLite(WAL)
                        ├─ /api/*      JSON API（统一封套）
                        ├─ /s/{token}  服务端渲染分享页（严格 CSP）
                        ├─ /ai /ai.md /llms.txt   给 AI 的说明
                        └─ 其余        内嵌 Vue SPA（客户端路由回退）
```

**只有一条入口、一个来源**：前端、API、分享页同源同前缀。这不是审美问题 —— 它让会话
Cookie 是**第一方**的（不受 Safari ITP / Chrome 第三方 Cookie 淘汰影响），也让分享页的
`script-src 'self'` 有实际意义。

**三条关键流**：

1. **上报**：`POST /api/reports`（Bearer）→ validate → store.CreateReport → 若带
   `autoShareHours` 同步建 token → 返回 `ShareURL(token)`（由 config 拼前缀）。
2. **读取分享**：`GET /s/{token}` 或 `GET /api/share/{token}` → `store.ResolveShare`
   返回三态 `ok|missing|gone` → `missing`=404（**未知与已吊销不可区分**）、`gone`=410。
3. **面板登录**：`POST /api/admin/login` → PBKDF2 校验 → HMAC 签名 Cookie（`teleport_session`，
   12h，`Path=RoutePrefix`）→ 后续 `requireSession` 验签。

## 5. Invariants

1. **前缀是一份数据，不是字符串常量。** `/yeciorez/teleport` 只由 `ROUTE_PREFIX` 推导；
   Go 路由、`vite.config.ts` 的 `base`、`web/src/api.ts` 的 `BASE_URL`、分享链接、Cookie Path
   全部从它来。**Caddy 必须用 `handle`，不能用 `handle_path`**（剥前缀会让所有路由 404）。
2. **响应封套固定**：`{ok:true,data,requestId}` / `{ok:false,error:{code,message},requestId}`。
3. **链接状态不泄漏**：未知 token 与已吊销 token 都 → 404；仅过期 → 410。**Enforced**：
   `internal/store` 的 `ResolveShare` 测试 + `internal/api` 的 404/410 矩阵。
4. **JS 语义校验**：`title` 长度按 **UTF-16 code unit** 计（不是字节、不是码点），`trim()`
   用 ECMAScript 空白集合（**不含 U+0085 NEL**）。这是从 TS 版逐条复刻来的，有专门的
   `UTF16Len`/`JSTrim` 与 emoji 测试。**Enforced**：`internal/validate`。
5. **原始 HTML 永不渲染**：goldmark 不加 `WithUnsafe`；`views.HTMLMountEnabled = false`；
   分享页 `default-src 'none'` + `script-src 'self'`。开启 `format:'html'` 前必须先上
   DOMPurify + nonce CSP。
6. **零 cgo**：`modernc.org/sqlite` 是纯 Go；`store.go` 里的 `_ "modernc.org/sqlite"`
   **空导入不能删**（删掉仍编译、仍过 vet，运行时才 `unknown driver`）。**Enforced**：
   `CGO_ENABLED=0` 发布构建 + 见 §6。
7. **时间统一 epoch 毫秒**，`expires_at = 0` = 永不过期。
8. **未知 `/api/*` 必须仍是 JSON**，不能退化成 SPA 的 HTML 404。**Enforced**：
   `TestUnknownRouteWithSPAStaysJSON`（删掉两个更具体的兜底就会失败）。
9. **`_`-前缀与 `_TEMPLATE.md` 是惰性的**：模板与示例，不被 dump/sync 视为记忆条目。
10. **schema 不漂移**：`schema.sql` 与 `backend/internal/store/migrations/` 由
    `pnpm run db:check-schema` 强制一致。

## 6. Boundaries and known debts

- **单文件部署的真实约束**：`//go:embed` 目录必须先由 Vite 产出，所以
  `scripts/build.sh` 的顺序（前端 → 后端）不能反；`embed_frontend` 标签是干净仓库里
  `go test`/`go vet` 仍可用的原因（没有它走「无前端」桩）。
- **`aidoc` 的存在理由**：SPA 是客户端渲染的，agent 抓一个 URL 只会拿到空壳。任何
  **必须被 agent 读到**的内容都得服务端渲染 —— 这就是 `/ai`、`/ai.md`、`/llms.txt`
  是 Go handler 而不是 Vue 路由的原因。
- **大陆可达性是硬约束**：`api.hcyj.xyz` 是唯一入口；CF 前端已全部下线（Worker 删除、
  DNS 记录移除）。若未来重加 CDN，必须先验证大陆可达性，且只能用**大陆观测点**。
- **未做的**：`format:'html'`；`CleanupExpired` 未挂定时任务；无删除报告 API；无 SQLite
  自动备份；无多用户（单管理员密码）。
- **共享主机风险**：`api.hcyj.xyz` 同时承载另一个服务（兜底反代 :3000），所以
  Cookie `Path` 必须是应用前缀而不是 `/`；站点级安全头必须排除本前缀（`@notTeleport`），
  否则与后端的更严格策略重复冲突。
- **凭证明文在服务器上**：`/data/services/teleport/teleport.env`（600 root:root）。
  轮换步骤见 `runbooks/deploy-teleport.md`。
