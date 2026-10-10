# Teleport

个人 AI 报告展示与时效分享平台。在终端 / 工作框中完成复杂工作（安全渗透、架构设计、
开发进度等）后，通过一次 API 调用把报告发布为在线网页，并得到一条**带时效的分享链接**。

**当前架构：单二进制（Go 内嵌前端）**

| 层 | 技术 | 部署位置 |
|---|---|---|
| 前端 | Vue 3 + Vite（pnpm） | **编译进 Go 二进制**（`go:embed`） |
| 后端 | Go + SQLite（纯 Go 驱动，无 cgo） | 国内云服务器 hcyj — Caddy 分流至 `/yeciorez/teleport` |
| 入口 | 单一来源：`https://api.hcyj.xyz/yeciorez/teleport/` | 前端、API、分享页同源同前缀 |

前端与后端**构建为同一个可执行文件**：Vite 产物写进 `backend/internal/webui/dist/`，
由 `//go:embed` 打进二进制。部署就是上传一个文件、重启服务，服务器上不需要
Node、不需要 Go、也没有任何需要与二进制同步的静态目录。

> **历史说明**：本项目原先基于 Cloudflare 全家桶（Workers + D1 + Hono）全站部署，
> 之后改为「前端在 Cloudflare Workers Assets + Worker 反代后端」。**该方案已废弃**，
> 原因是 Cloudflare 免费版分配的 anycast IPv4 在中国大陆被 TCP 层封锁（详见
> 「已知陷阱 · 5」），域名解析正常、ping 得通，但 443 端口完全打不开。
> 现在前端与后端都由国内服务器直接提供。

**核心能力**

| 能力 | 说明 |
|---|---|
| 报告发布 | `POST /api/reports` 上报 Markdown，自动生成分享链接 |
| 时效分享 | 每条链接独立设置有效期（1 小时 ~ 永不过期），可随时禁用 / 吊销 |
| 内容渲染 | Markdown **服务端渲染**（Go + goldmark + Chroma）+ Mermaid 客户端懒加载 |
| 管理面板 | Vue 3 SPA，登录后查看报告、管理分享链接、复制 / 调期 / 禁用 |
| 密钥自助申请 | agent 提交申请 → 面板批准 → agent 一次性领取明文密钥（服务端只存哈希） |
| 安全 | 原始 HTML 不渲染、严格 CSP、常量时间鉴权比较、失效链接语义化 404/410 |

---

## 目录结构

```
teleport/
├── schema.sql                    # 数据库 schema 快照（可直接喂给 sqlite3）
├── vite.config.ts                # Vite 构建配置（root=web，输出到 Go embed 目录）
├── tsconfig.web.json             # 前端（含 Vite 配置）TS 配置
│
├── scripts/
│   ├── build.sh                  # 发布构建：前端 + 后端 → 单个自包含二进制
│   └── check-schema-sync.mjs     # 校验 schema.sql 与 Go 内嵌迁移未漂移
│
├── backend/                      # ── Go 后端（全部业务逻辑 + 前端托管）──
│   ├── main.go                   # 启动 / 优雅退出 / hash-password / migrate / version
│   ├── deploy/
│   │   ├── teleport.service      # systemd unit（含沙箱加固）
│   │   └── teleport.env.example  # 生产环境变量模板
│   └── internal/
│       ├── config/               # 环境变量读取与校验
│       ├── httpx/                # 中间件、响应封套、Session 签名、Bearer 鉴权
│       ├── password/             # PBKDF2-HMAC-SHA256
│       ├── store/                # SQLite 访问层 + 内嵌迁移
│       │   └── migrations/       # *.sql，编译进二进制
│       ├── markdown/             # goldmark + Chroma 渲染
│       ├── validate/             # 请求体校验（复刻原 JS 语义）
│       ├── domain/               # 业务类型
│       ├── views/                # 分享页服务端渲染模板
│       ├── spa/                  # 静态前端托管（SPA 回退 / 缓存 / 路径穿越防护）
│       ├── aidoc/                # 给 AI 的使用说明（/ai、/ai.md、/llms.txt）
│       │   └── guide.md          # 正文，//go:embed 进二进制
│       ├── webui/                # //go:embed 前端产物（dist/ 由 Vite 生成）
│       └── api/                  # 路由挂载
│
└── web/                          # ── 前端源码 ──
    ├── index.html                # 管理面板 SPA 入口
    └── src/
        ├── main.ts               # createApp + 路由挂载
        ├── App.vue               # 顶栏 / 登录态分流 / 全局 401 处理
        ├── api.ts                # 类型化 API 客户端（统一解包响应封套）
        ├── format.ts             # 时间 / 过期 / 复制等展示工具
        ├── env.d.ts              # import.meta.env 类型
        ├── styles.css            # 轻量样式（亮/暗色，无 CSS 框架）
        ├── router/index.ts       # /dashboard 路由 + 登录守卫
        ├── share/
        │   └── main.ts           # 分享页客户端：Mermaid 懒加载 + 过期倒计时
        └── views/
            ├── LoginView.vue     # 密码登录
            ├── ReportsView.vue   # 报告列表 + 客户端搜索
            └── ReportDetailView.vue  # 报告详情 + 分享 Token 管理
```

> `backend/internal/webui/dist/` 全部内容均为 `pnpm run build` 的**生成产物**，
> 请勿手改，且已被 gitignore —— 它只是 `//go:embed` 的输入目录，不是部署目标。

### 请求路径

```
浏览器 ──https──> api.hcyj.xyz/yeciorez/teleport/...
                        │
                    Caddy(:443)  handle /yeciorez/teleport*
                        │         （保留前缀，不 strip）
                        ▼
                  Go(:8788, 仅监听 docker 网桥) ─> SQLite
                        ├─ /api/*        JSON API
                        ├─ /s/{token}    服务端渲染的分享页
                        └─ 其余         内嵌的 Vue SPA（客户端路由回退）
```

**只有一个来源，没有第二个入口。** 前端、API 与分享页同源同前缀，因此：

- 会话 Cookie 是**第一方**的，不受 Safari ITP / Chrome 第三方 Cookie 淘汰影响；
- 分享页的 `script-src 'self'` 有实际意义（脚本确实来自同一来源）；
- 不存在「前端在某处、后端在另一处」导致的环境漂移。

> **为什么不再让浏览器直连 Cloudflare**：见「已知陷阱 · 5」。简单说，CF 免费版
> 分配的那两个 IP 在大陆是断的，而 `api.hcyj.xyz` 是通的。

---

## 接口一览

所有路径都以 `/yeciorez/teleport` 为前缀挂在 `api.hcyj.xyz` 上。
下表中的路径是**相对前缀**的；浏览器与 API 客户端都用同一个前缀访问，
前端、接口与分享页完全同源。

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| `POST` | `/api/reports` | `Authorization: Bearer <agent 密钥>` | AI 上报报告；带 `autoShareHours` 时同步生成分享链接（密钥见「密钥自助申请」） |
| `GET` | `/api/reports/:id` | Bearer | 读取单篇报告源文（**仅限该密钥自己发布的**，别人的返回 404） |
| `GET` | `/api/share/:token` | 公开 | 只读获取报告；失效/过期分别返回 404 / 410 |
| `POST` | `/api/share/:token/revoke` | Bearer | 手动禁用链接（幂等；按报告归属判定，别人的返回 404） |
| `GET` | `/s/:token` | 公开 | 服务端渲染的分享页面 |
| `GET` | `/api/health` | 公开 | 健康检查 |
| `POST` | `/api/admin/login` | 口令 | 登录并下发签名 Session Cookie |
| `POST` | `/api/admin/logout` | 公开 | 清除 Session |
| `GET` | `/api/admin/session` | Session | 检查会话状态 |
| `GET` | `/api/admin/reports` | Session | 报告列表（`limit` / `offset` / `category`） |
| `GET` | `/api/admin/reports/:id` | Session | 报告详情 + 其全部分享 Token |
| `POST` | `/api/admin/reports/:id/shares` | Session | 为报告新建分享 Token（`expiresInHours`） |
| `PATCH` | `/api/admin/shares/:token` | Session | 调整过期时间 / 启用禁用 |
| `DELETE` | `/api/admin/shares/:token` | Session | 吊销链接 |

### 给 AI 的使用说明（`/ai`、`/ai.md`、`/llms.txt`）

这三个路由是**给 AI Agent 读的**：把站点地址交给 AI 就能让它独立发布报告。

| 路径 | `Content-Type` | 用途 |
|---|---|---|
| `/ai.md` | `text/markdown` | **首选**。纯 Markdown 使用说明，agent 直接读，无需剥 HTML |
| `/ai` | `text/html` | 同一份内容的网页版（无脚本，人也能看） |
| `/llms.txt` | `text/plain` | 按约定提供的发现入口，指向上面两个 |

```bash
# 把这一条给 AI 就够了
https://api.hcyj.xyz/yeciorez/teleport/ai.md
```

**为什么必须是服务端渲染的独立路由**，而不是 SPA 的一页、也不是一条分享链接：

- **SPA 页面 AI 读不到。** 前端是客户端渲染的，agent 抓 URL 只会拿到空的
  `<div id="app">` 外壳。
- **分享链接会过期。** 挂成 `/s/<token>` 等于给每个已缓存了该链接的 AI 埋一颗定时炸弹
  （到期 410、被吊销 404）。这三个路由是**永久稳定**的。
- **免鉴权是刻意的。** agent 必须在申请到密钥*之前*就能读到这份说明（先从第 2 节
  自助申请，才有密钥可用）。
- **事实从 live 配置注入。** 正文里用 `{{APP_BASE}}`、`{{ROUTE_PREFIX}}`、
  `{{DEFAULT_SHARE_HOURS}}`、`{{MAX_CONTENT_BYTES}}`、`{{ENVIRONMENT}}`、
  `{{KEY_APPLICATION_TTL}}`、`{{KEY_CLAIM_WINDOW}}`、`{{KEY_APPLY_PER_HOUR}}` 占位符，
  每次请求从 `config.Config` 替换 —— 换域名、改前缀或调限流后文档自动跟着走，不会静默撒谎。
- **正文随二进制发布。** `guide.md` 经 `//go:embed` 编进去，与代码同一个 git revision，
  不存在"文档更新了、代码没更新"的错位。

正文在 `backend/internal/aidoc/guide.md`。改它之后要重建才生效（`pnpm run build:release`）。

> ⚠️ 这三个路由挂在 `ROUTE_PREFIX` **之下**，没有占用 `api.hcyj.xyz` 的根路径 ——
> 该主机根路径属于另一个服务，抢 `/llms.txt` 会劫持别人的路由。

所有响应统一封套（与旧版完全一致）：

```jsonc
{ "ok": true,  "data": { /* ... */ }, "requestId": "..." }
{ "ok": false, "error": { "code": "gone", "message": "This share link has expired" }, "requestId": "..." }
```

### 密钥自助申请（agent self-service keys）

agent **不再向人类索取密钥**：自己提交申请 → 人类在面板点批准 → agent 按服务端给的间隔
轮询、一次性取回明文密钥。决策与冻结契约见
[`.ai/decisions/2026-10-10-agent-self-service-keys.md`](.ai/decisions/2026-10-10-agent-self-service-keys.md)。

**字段命名约定**：**请求体 camelCase**（`label`、`purpose`、`requestedHours`、`expiresInHours`），
**响应 snake_case**（`claim_secret`、`created_at`、`expires_at`、`token_prefix`）。下表所有响应形状
都是响应字段。

**公开（按 IP 限流）**

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| `POST` | `/api/agent-keys/applications` | 无 | 体 `{label, purpose?, requestedHours?}`；**201** 返回 `{id, claim_secret, status, created_at, expires_at, poll_interval_seconds}` |
| `GET` | `/api/agent-keys/applications/:id` | 头 `X-Teleport-Claim: <claim_secret>` | 轮询状态；批准后**首次**领取返回明文 `key.token` 并把申请单转 `claimed`。`pending`/`rejected` 为 200 且无 `key`；二次领取 → 404；申请单过期或领取窗口关闭 → **410 `gone`**；活跃密钥满 → 409 `too_many_active_keys` |
| `GET` | `/api/agent-keys/renewals/:id` | 头 `X-Teleport-Claim: <claim_secret>` | 查续期结果：`{id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at}` |

**agent 自有（`Authorization: Bearer <自己的密钥>`）**

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/agent-keys/me` | 自身状态：`{id, name, token_prefix, created_at, expires_at, revoked_at, last_used_at, request_count, note, root}`；`expires_at = 0` = 永不过期（**无界，不是"已过期"**），`root` 对普通密钥恒为 `false` |
| `POST` | `/api/agent-keys/renewals` | 体 `{requestedHours?}`；**201** 返回 `{id, key_id, status:"pending", requested_hours, created_at, claim_secret}`。**续期需人类批准**；批准后在现有到期时间上**叠加**批准时长：`granted = max(当前 expires_at, now) + 批准时长` —— **只延长、绝不缩短**，`0` 保持 `0`（因此提前续期不损失剩余时间）；已有 pending 时返回 **409 `renewal_exists`**。批准只看「提交时刻」与「是否被撤销」：**过期前**提交的续期获批后会把已过期的密钥**恢复可用**；密钥**一旦过期**就无法再新提交续期（Bearer 401）；**已撤销**的密钥永不被复活（批准 → **404**） |

**面板（Session Cookie）**

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/admin/key-applications?status=pending` | 审批队列（默认 `pending`，按 `created_at` 倒序），返回 `domain.KeyApplication` 数组：`{id, label, purpose, requested_hours, status, created_at, expires_at, decided_at, claim_deadline, issued_key_id, requester_ip, user_agent, approved_name, approved_hours, approved_note}` |
| `POST` | `/api/admin/key-applications/:id/approve` | 体 `{name?, expiresInHours?, note?}`；`expiresInHours=0` → 永不过期，缺省则用申请里的 `requestedHours`；返回更新后的申请单 |
| `POST` | `/api/admin/key-applications/:id/reject` | 体 `{reason?}`；返回更新后的申请单 |
| `GET` | `/api/admin/keys` | 全部密钥（`domain.AgentKey` 数组，**绝不含 `token_hash` 与明文**） |
| `POST` | `/api/admin/keys` | 体 `{name, expiresInHours?, note?}`；手动转交路径，**201** 返回 `{token, key}`（`token` 明文仅此一次） |
| `PATCH` | `/api/admin/keys/:id` | 体 `{name?, expiresInHours?, revoked?, note?}`；改 `expiresInHours` 即用户直接续期（**重设**为 `now + 小时数` —— 这是显式**缩短**授权的唯一途径）；返回更新后的密钥 |
| `GET` | `/api/admin/key-renewals?status=pending` | 续期申请列表 |
| `POST` | `/api/admin/key-renewals/:id/approve` | 体 `{expiresInHours?}`；缺省则用申请里的 `requestedHours`；**在现有到期时间上叠加（只延长，见上）** |
| `POST` | `/api/admin/key-renewals/:id/reject` | — |

**只存哈希、明文仅此一次**

- `agent_keys` 只有 `token_hash`（`sha256` hex，`UNIQUE`）与供人辨识的 `token_prefix`；
  `key_applications.claim_hash` 同理只存 `sha256(claim_secret)`。
- 明文密钥只在批准后的**首次领取**响应里出现一次，之后**永不可再取**：再领即 404，
  服务端刻意不告诉你"已经领过"。人类也无法从面板或数据库读出 agent 的密钥再转交 ——
  面板的"手动转交"路径是**新建**一把、当场显示一次明文。
- `claim_secret` 同样只在提交申请 / 发起续期的响应里出现一次；丢了只能重新申请。
  （续期的 `claim_secret` 不落库：它由同一个 `K_derive` 子密钥按 `renewal_id + key_id` 现场重算，
  所以连它的哈希都不需要存。）
- 鉴权时对来钥求 `sha256` 再查 `UNIQUE` 索引（O(log n)），**不做逐行常数时间比较**
  （表大了就是 DoS 面）；`AGENT_SECRET_KEY` 这一路仍走 `subtle.ConstantTimeCompare`。
  两路都失败统一 401，不用响应时间区分失败原因。

**为什么密钥是派生出来的，而不是暂存明文**

| 落选方案 | 为什么不用 |
|---|---|
| 批准到领取之间把明文暂存起来 | 暂存窗口内明文落盘，这一段的任何备份 / 快照 / WAL 副本都等于泄漏 |
| 让 agent 自己生成密钥、只提交哈希 | 服务端无法约束熵：agent 可以提交 `"1234"` 的哈希，批准后即得弱密钥 |

因此选择**服务端派生**：

```
K_derive = HMAC-SHA256(SessionSecret, "teleport/derive/agent-key/v1")   # 子密钥，避免跨协议复用
token    = base64url(HMAC-SHA256(K_derive, "teleport/agent-key/v1|" + appID + "|" + claim_secret))
```

熵由服务端保证（256 位），领取时按上式重算并只落 `sha256(token)` ——
**任何时刻数据库里都没有明文密钥**。代价是 `SessionSecret` 的地位上升：它现在同时是
密钥派生根，泄漏后果从"可伪造会话"扩大到"可重算所有 agent 密钥"（已记入 `.ai/pitfalls/`）。

**归属规则与限额**

- `POST /api/reports` 落 `reports.owner_key_id = principal.KeyID`（root 密钥落 `''`）。
- `GET /api/reports`：**列出调用者自己发布的报告**（root 列出全部）。这是「管理自己的报告」
  所需要的全部能力——读别人的 id 一律 404，所以忘了 id 就等于永久失去该记录，
  这个接口是唯一的找回途径。
- `GET /api/reports/:id`、`POST /api/share/:token/revoke`：**只有发布者本人**（或 root / 面板）
  能操作；非归属者一律 **404**，不暴露"存在但不是你的"。
- **密钥过期后有 90 天续期宽限期**（`KEY_RENEWAL_GRACE`，默认 `2160h`；设 `0` 关闭）。
  宽限期内，过期密钥**唯一**能调用的接口是 `POST /api/agent-keys/renewals`——
  读报告、发报告、撤销链接全部 401。续期**不换 `key.id`**，所以归属保持不变；
  重新申请会换 id，旧报告即永久失去。**已撤销的密钥不享受宽限**（撤销是人类的明确决定）。
  注意 Go 的 duration 没有 `d` 单位，90 天要写 `2160h`。
- 每个 agent 响应都带 `X-Teleport-Key-Expires-At`（Unix **秒**；密钥永不过期时不出现）
  与 `X-Teleport-Key-Expired: true`（仅"已过期但仍在宽限期内"时出现），
  让 agent 不必反复轮询 `/me` 就能知道自己何时失效。
- 申请入口完全开放但有限流：`KEY_APPLY_PER_HOUR`（默认 5，每个 IP 每小时）、
  `KEY_APPLICATION_TTL`（默认 24h，申请单存活期，未批准即自动失效）、
  `KEY_CLAIM_WINDOW`（默认 30m，批准后的领取窗口）、`KEY_MAX_PENDING`（50）、
  `KEY_MAX_ACTIVE`（100）。限流是**进程内**计数，重启清零（单二进制的已知取舍）。
- `AGENT_SECRET_KEY` 降级为 **root / 应急密钥**：保留全部权限，其发布的报告
  `owner_key_id = ''`，只有 root 与面板能读；既有密钥与既有数据不受影响。

### 上报示例

```bash
curl -X POST https://api.hcyj.xyz/yeciorez/teleport/api/reports \
  -H "Authorization: Bearer $TELEPORT_KEY" \
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
      "url": "https://api.hcyj.xyz/yeciorez/teleport/s/R1Ku0lg0YNoMXVfJgGuzYA"
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

迁移文件内嵌在 Go 二进制里（`backend/internal/store/migrations/*.sql`），
启动时自动执行，无需手工建表。`schema.sql` 是给人看 / 给 `sqlite3` 用的快照，
`pnpm run db:check-schema` 会校验两者未漂移。

---

## 本地开发

> **包管理器：pnpm**（`package.json` 的 `packageManager` 锁定 `pnpm@10.33.0`）。
> 请勿使用 npm / yarn，仓库只保留 `pnpm-lock.yaml` 单一锁文件。

需要两个终端：Go 后端与 Vite 开发服务器。

```bash
pnpm install

# ── 终端 1：Go 后端 ─────────────────────────────────────────────
cd backend
# 生成面板口令哈希
go run . hash-password 'your password'

LISTEN_ADDR=127.0.0.1:8788 \
  ROUTE_PREFIX=/yeciorez/teleport \
  PUBLIC_BASE_URL=http://127.0.0.1:5173 \
  COOKIE_SECURE=false \
  SESSION_SECRET=dev-session-secret \
  AGENT_SECRET_KEY=dev-agent-secret \
  ADMIN_PASSWORD_HASH='<上一步输出的值>' \
  DB_PATH=./data/teleport.db \
  go run .

# ── 终端 2：前端（HMR） ────────────────────────────────────────
pnpm run dev:web    # http://127.0.0.1:5173/yeciorez/teleport/
```

`vite.config.ts` 把 `<前缀>/api` 与 `<前缀>/s` 代理到 `127.0.0.1:8788`，**且不做 rewrite**：
浏览器请求的路径与后端看到的路径逐字节相同，dev 与 prod 不会出现
「本地能跑、线上 404」的偏差。

> 若只想验证后端（不跑前端），可以完全不构建前端：`webui` 在没有
> `embed_frontend` 构建标签时会报告「无前端」，服务退化为纯 API。
> 想在没有重新编译的情况下换前端资源，可用 `STATIC_DIR=/path/to/dist` 指向磁盘目录。

### 关于 pnpm 的两点注意

**1. 构建脚本需要显式放行**（pnpm 10 默认拦截 `postinstall`，是供应链安全特性）。
本项目已在 `pnpm-workspace.yaml` 的 `onlyBuiltDependencies` 中按**精确包名**放行：

| 包 | 为什么必须放行 |
|---|---|
| `esbuild` | Vite 依赖的平台二进制 |

> 历史上还放行过 `workerd` / `sharp` / `blake3-wasm`（Cloudflare 工具链所需）。
> 这些依赖已随 Cloudflare 方案一并移除，若清单里仍有它们，可以直接删掉。

若 `pnpm install` 后提示 `Ignored build scripts`，说明放行清单缺失或包名有变，
用 `pnpm approve-builds` 查看并按需补入 —— **不要**用通配符一次性放行全部。

**2. 严格依赖隔离**：pnpm 不会把未声明的传递依赖提升到 `node_modules` 顶层。
好处是杜绝幽灵依赖；代价是若某处 import 了未在 `package.json` 声明的包，
npm 下可能"碰巧能跑"，pnpm 下会立刻报错。这是**期望行为**，不要用
`node-linker=hoisted` 去绕过它。

### 命令一览

| 命令 | 作用 |
|---|---|
| `pnpm run dev:web` | Vite 前端开发服务器（HMR + `/api`、`/s` 代理） |
| `pnpm run build` | 构建 Vue 面板 + 分享页客户端到 `backend/internal/webui/dist/` |
| `pnpm run build:release` | **发布构建**：前端 + 后端 → 单个自包含二进制到 `bin/` |
| `pnpm run typecheck` | 前端类型检查（`vue-tsc`） |
| `pnpm run db:check-schema` | 校验 `schema.sql` 与 Go 内嵌迁移一致 |
| `pnpm run backend:test` | Go 单元 / 集成测试 |
| `pnpm run backend:vet` | Go 静态检查 |
| `pnpm run backend:build` | 同 `build:release`（等价入口，便于记忆） |
| `pnpm run check` | 以上检查一把梭 |

---

## 部署

### 构建：一个文件

```bash
pnpm run build:release          # → bin/teleport-linux-amd64
```

它做两件事，顺序不能反：

1. `vite build` → 写进 `backend/internal/webui/dist/`（`//go:embed` 的输入目录）；
2. `go build -tags embed_frontend` → 把上一步的产物打进二进制。

`embed_frontend` 这个构建标签是关键：没有它，`webui` 走的是「无前端」桩实现，
于是 `go build` / `go test` / `go vet` 在前端尚未构建的干净仓库里依然可用。
发布构建显式打开它，二进制里就有了完整前端。

编译完成后会打印版本、体积、SHA256，并自检 `version --frontend`（应为 `embedded`）。

### 后端（Go + SQLite，hcyj）

服务器上没有 Go 工具链，因此**在本地交叉编译**后上传。因为前端已经在二进制里，
上传的东西只有这一个文件：

```bash
# 1. 本地编译（纯静态、无 cgo；自动把 git revision 编进二进制）
pnpm run build:release         # → bin/teleport-linux-amd64

# 2. 服务器准备（首次）
ssh hcyj
useradd --system --no-create-home --shell /usr/sbin/nologin teleport
mkdir -p /data/services/teleport && chown teleport:teleport /data/services/teleport

# 3. 上传二进制与 unit
scp bin/teleport-linux-amd64 hcyj:/data/services/teleport/teleport.new
scp backend/deploy/teleport.service hcyj:/etc/systemd/system/teleport.service

# 4. 写环境文件（含密钥，600 root:root）
scp backend/deploy/teleport.env.example hcyj:/data/services/teleport/teleport.env
#   编辑填入 AGENT_SECRET_KEY / SESSION_SECRET / ADMIN_PASSWORD_HASH
#   哈希用：./teleport hash-password '你的密码'

# 5. 原子替换 + 启动
ssh hcyj 'cd /data/services/teleport && chmod 700 teleport.new && chown teleport:teleport teleport.new \
          && mv teleport.new teleport && systemctl restart teleport'
journalctl -u teleport -f
```

> **升级时用 `mv` 覆盖而不是 `scp` 直接写目标文件**：`scp` 会以截断方式打开
> 目标文件，若服务此刻正在运行，会短暂看到一个残缺的可执行文件；`mv` 是同目录
> 内的原子替换，要么是旧的、要么是新的。上传到 `.new` 再 `mv` 才是安全的。

> 想确认线上跑的到底是哪次提交：
> ```bash
> ssh hcyj '/data/services/teleport/teleport version'            # git revision
> ssh hcyj '/data/services/teleport/teleport version --frontend' # embedded / none
> ```
> `-dirty` 表示构建时工作区有未提交改动。构建可复现 —— 同一 revision + 同一
> `-X main.version` 会得到**逐字节相同**的二进制，可用 `sha256sum` 对比本地与线上产物。

服务监听 `172.17.0.1:8788`（docker 网桥网关），**不对公网暴露**，只有 Caddy 能访问。
数据目录 `/data/services/teleport/`，SQLite 使用 WAL 模式。
unit 内已启用 `ProtectSystem=strict`、`NoNewPrivileges`、`MemoryDenyWriteExecute` 等沙箱选项，
并把 `/data/services/teleport` 设为唯一可写路径。

### Caddy 分流（hcyj）

`api.hcyj.xyz` 由 docker 容器 `caddy` 占用 443 端口。项目在自己的路径前缀上分流：

```caddyfile
handle /yeciorez/teleport* {
    reverse_proxy 172.17.0.1:8788 {
        header_up Host {host}
        header_up X-Real-IP {remote}
        header_up X-Forwarded-For {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

三个关键点：

1. **用 `handle` 而不是 `handle_path`** —— 前缀必须**保留**。Go 服务的所有路由都挂在
   `/yeciorez/teleport` 下（含前端资源与分享页），且分享链接里也写死了该前缀；
   在此处剥掉会让所有路由 404。
2. **站点级安全响应头要排除该前缀**（`@notTeleport not path /yeciorez/teleport*`）。
   后端对公开分享页发的是更严格的策略（`default-src 'none'`、`X-Frame-Options: DENY`），
   两边同时下发会产生重复且互相冲突的头。
3. Caddy 按路径**最长匹配**选 `handle`，因此该块优先于兜底块，`api.hcyj.xyz` 原有服务不受影响。

> 该容器原先 `restart=no`，重启机器后整个 `api.hcyj.xyz` 都会消失。
> 已改为 `docker update --restart unless-stopped caddy`。

### 前端

**前端没有独立部署步骤** —— 它已在后端二进制里。`pnpm run build:release` 之后，
重启服务即完成前端更新。

> Cloudflare 侧已全部下线：Worker 已删除、`teleport.zeroicey.me` 的 DNS 记录
> （原先由 `custom_domain = true` 自动创建）也已随 Worker 删除而移除。
> `wrangler.toml`、`src/index.ts`、`worker-configuration.d.ts` 等文件均已从仓库移除。
> 仓库里不再有 `wrangler` 依赖，因此也不需要 Cloudflare 凭证。
>
> 但**域名本身仍是 Cloudflare 托管的**：`zeroicey.me` 及其它子域的 A 记录依然指向
> 那批在大陆被封锁的 CF IP，访问状态与本次改动无关，需要另行处理（例如自行
> 在该账户下把记录改成可用的 IP）。这一点不在本项目范围内。

---

## 渲染管线（Markdown + Mermaid）

分享页 `GET /s/:token` 采用**混合渲染**：Markdown 与代码高亮在 Go 服务端渲染，
Mermaid 在客户端懒加载。这个划分由体积决定。

| 环节 | 位置 | 原因 |
|---|---|---|
| Markdown → HTML | **Go 服务端** | goldmark 体积小，且默认不放行原始 HTML，是整条链路最关键的 XSS 防线 |
| 代码语法高亮 | **Go 服务端** | Chroma 只需精选语言子集，避免引入全部语言定义 |
| Mermaid 图表 | **客户端懒加载** | Mermaid 打包后仍有数 MB 分块；只在文档真的含图时才 `import()` |

实测产物：`assets/share.js` 仅约 **2.4 KB**，Mermaid 拆成独立分块
（最大 `elk` 455 KB gzip、`cytoscape` 137 KB gzip），纯文字报告完全不加载它们。
若把 Mermaid 静态引入，**每一篇纯文本报告都会被迫下载约 1 MB**。

### Markdown 渲染（`backend/internal/markdown/render.go`）

- goldmark 使用默认配置，**不开启 `WithUnsafe`** —— 报告正文里的 `<script>`、
  `<img onerror>` 一律被替换为 `<!-- raw HTML omitted -->`。
- 危险链接协议（`javascript:` / `vbscript:` / 危险 `data:`）由 goldmark 内置逻辑拦截。
- 启用 Table / Strikethrough / Linkify 扩展与自动标题 ID；渲染器覆写通过
  `renderer.WithNodeRenderers` 注册，**不用 `SetRenderer`**（后者会丢掉扩展注册的渲染器）。
- 语法高亮使用 Chroma，输出 class 而非内联样式；无语言标记或高亮失败时
  **退回转义后的纯文本**，绝不输出原始源码。

### Mermaid 渲染（`web/src/share/main.ts`）

- 仅当页面存在 `.mermaid` 元素时才 `import('mermaid')`。
- `securityLevel: 'strict'` —— 禁用 `click` 指令与脚本执行。报告内容是**不可信输入**，
  这一项不是可选项。（注：`strict` 并**不**创建 iframe，只有 `'sandbox'` 才会；
  因此分享页 CSP 无需 `frame-src`。）
- 用 `Promise.allSettled` 逐个渲染：单个图表语法错误不会导致整页白屏。

### 内容安全策略（CSP）

```
default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
img-src 'self' data:; font-src 'self'; connect-src 'self';
base-uri 'none'; form-action 'none'; frame-ancestors 'none'; upgrade-insecure-requests
```

- **`script-src 'self'` 且页面内零内联脚本** —— 无需维护 nonce。
- 唯一的放宽是 `style-src 'unsafe-inline'`：Mermaid 运行时会把 `<style>` 注入它生成的 SVG。
  这**不能**执行脚本。
- `img-src 'self' data:` 让报告里的图片只能来自本站或 data URI，无法用作外链追踪像素。

### 安全响应头与缓存策略

**安全头**：全部由 Go 进程统一下发（`httpx.SecurityHeaders` 中间件 +
`views.ContentSecurityPolicy`），不再依赖 Cloudflare 的 `_headers` 文件——
那份文件是 Workers Assets 专有的，已随方案一起删除。

- 所有响应：`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、
  `X-Frame-Options: DENY`、`X-Robots-Tag: noindex`
- 分享页额外带严格 CSP（见上）
- 静态资源与 API 走同一套中间件，不存在「一种资源有头、另一种没有」的空档

**缓存**：由 `internal/spa` 按**文件名形态**决定，分两档：

| 路径 | 响应头 | 理由 |
|---|---|---|
| `/assets/<name>-<hash>.js` | `public, max-age=31536000, immutable` | Vite 内容哈希命名，同一 URL 的字节**永不改变** |
| `/assets/share.js` | `no-cache` | **例外**：`vite.config.ts` 把它固定成稳定路径（Go 硬编码引用），字节随部署变化 |
| `index.html` | `no-cache` | 稳定文件名但内容随部署变化，必须每次复用校验 |
| 其它实体文件 | `no-cache` | 同上 |

> `/assets/*` 用 `immutable` 是安全的，因为文件名里有内容哈希；但**前提是
> `index.html` 与 `share.js` 保持 `no-cache`**——否则部署后客户端会拿着旧外壳
> 去请求已被删除的哈希包。这两条是一体的，不能只改一半。
>
> ⚠️ **`share.js` 是 `/assets/` 下唯一的非哈希名**，因此判定必须看**名字**而不是目录：
> 曾经把整个 `/assets/` 当作 `immutable`，导致回访浏览器最长一年执行旧的分享页 JS，
> 且不报任何错。见 `.ai/pitfalls/cases/2026-10-10-share-js-cached-immutable-for-a-year.md`。
> 例外清单在 `spa.go` 的 `stableAssetFiles`，由
> `TestServesStableAssetUncached` 与 `TestStableAssetListMatchesBuild` 守着。

---

## 管理面板（Vue 3）

### 技术选型

用 **Vue 3 + Vite 纯 SPA**。面板是私有页面，不需要 SEO / SSR；
公开分享页已由 Go 服务端渲染（这样才能返回真正的 404/410）。
引入 SSR 框架只会增加构建复杂度而没有任何收益。

Vite 的 `root` 指向 `web/`，`build.outDir` 直接输出到
`backend/internal/webui/dist/`——**也就是 `//go:embed` 的输入目录**。
产物按路由分包。

### 路由

| 路径（相对前缀） | 视图 | 说明 |
|---|---|---|
| `/dashboard` | `LoginView` | 密码登录 |
| `/dashboard/reports` | `ReportsView` | 报告列表 + 客户端搜索 |
| `/dashboard/reports/:id` | `ReportDetailView` | 报告详情 + 分享 Token 管理 |

路由的 `history` 基址取自 `import.meta.env.BASE_URL`（即 Vite 的 `base`，
也就是 `/yeciorez/teleport/`），因此地址栏始终保留前缀，
且 `router.push` 不会导航到应用之外。

深链接可刷新的保证来自 `internal/spa`：任何未匹配到实体文件、且不属于
`/api/`、`/s/` 的路径都会返回 `index.html`。

### 功能

- 登录 / 登出，全局 401 自动跳回登录页并保留 `?redirect=`。
- 报告列表：标题、分类、格式、相对时间；支持按标题/分类/ID 过滤。
- 报告详情：元数据、正文源码、分享链接管理 ——
  新建（1 小时 / 6 小时 / 24 小时 / 7 天 / 30 天 / 永不过期）、复制链接、
  调整有效期、禁用 / 恢复、吊销。

### 前端安全边界

路由守卫（`router.beforeEach`）**只是体验优化**，不是安全措施 ——
真正的鉴权在服务端 Session 校验。客户端可以绕过路由守卫，
但绕不过 `/api/admin/*` 的 Cookie 校验。

---

## 已知陷阱

### 1. SQLite 驱动必须显式空导入

`internal/store/store.go` 中的 `_ "modernc.org/sqlite"` 不能删。
删掉后代码**照样编译、照样通过 vet**，直到运行时才报
`sql: unknown driver "sqlite"` —— 纯 Go 驱动是靠 `init()` 注册的。

### 2. 系统级 `docker exec` 看不到宿主的 `/tmp`

`docker exec caddy caddy validate --config /tmp/Caddyfile.new` 校验的是
**容器内**的文件。宿主 `/tmp` 与容器 `/tmp` 是两个目录，因此它可能在校验一个
陈旧副本并报告 "Valid configuration"，而真正要生效的配置根本没被检查过。
改 Caddyfile 时用 `docker cp` 送进容器，或直接校验 bind-mount 后的路径。

### 3. `handle_path` 会吃掉路由前缀

见「部署 / Caddy 分流」第 1 条：前缀是路由的一部分，必须用 `handle`。

### 4. 共享主机上的 Cookie 作用域

`api.hcyj.xyz` **不是本项目独占的**——它同时承载另一个服务（兜底反代到 :3000）。
因此会话 Cookie 的 `Path` 必须是应用前缀 `/yeciorez/teleport`，而不是 `/`。
若用 `/`，浏览器会把我们的会话 Cookie 附加到该主机上**每一个**请求，
包括访问另一个服务的请求：既是不必要的凭据暴露，也容易在排查时误导人。

代码里 `CookiePath` 默认取 `RoutePrefix`（见 `internal/config/config.go`），
只有显式设置 `COOKIE_PATH` 才会覆盖。实测 `Set-Cookie` 确实带
`Path=/yeciorez/teleport`。

### 5. Cloudflare 免费版在中国大陆被 TCP 层封锁（本方案的直接起因）

**这是整个架构改动的根因，也是本项目最重要的一条经验。**

现象：`https://teleport.zeroicey.me/` 在大陆两个观测点（广东电信家宽、hcyj 本身）
**稳定**打不开；但在境外（Azure 香港）300/300 全通。

排查过程与结论：

| 检查项 | 结果 |
|---|---|
| DNS 解析 | **正常**。阿里 223.5.5.5、DNSPod 119.29.29.29、8.8.8.8、1.1.1.1 都返回 `104.21.55.38` / `172.67.144.109` |
| ICMP ping | **通**。233ms，0% 丢包 |
| TCP 443 / 80 / 2053 | **全部被阻断**，连续 3 轮稳定 |
| 同一 SNI、换 CF 边缘 IP | `104.16.0.1`、`104.17.0.1`、`104.18.0.1`、`104.24.0.1`、`104.27.0.1`、`188.114.96.1` 等 → **200** |
| 同一 SNI、另一些 CF IP | `162.159.0.1`、`108.162.192.1` → 403；`172.65.0.1` → 超时 |

结论：**封锁发生在 IP 层，不是域名层、也不是 SNI 层**。域名、证书、Worker、
后端全部正常，纯粹是 CF 免费版分配到的**那两个 anycast IPv4** 在大陆被拦。
DNS 没被污染（那通常是返回假 IP），ICMP 也通，所以不是路由黑洞——是 TCP 层过滤。

**同一次排查还发现**：`zeroicey.me` 与 `gh.zeroicey.me` 解析到**同一对**被封锁的 IP，
所以用户主站同样在大陆不可达。这个问题比 teleport 本身影响更大，但它不属于本仓库，
需要单独修（换记录 / 换解析）。

**为什么不用「优选 IP」绕过**：CF 的优选 IP 玩法要求把 DNS 移出 Cloudflare
（用 CNAME 接入），可一旦 DNS 不托管在 CF，Worker 就不再运行，整个方案失效。
CF 官方的大陆加速（China Network）需要企业版 + 域名 ICP 备案。

**最终解法**：把前端也交给国内服务器。原本 Worker 的职责只是「反代 `/api`、`/s`
+ 托管静态资源」，而 `api.hcyj.xyz/yeciorez/teleport/api/...` 本来就是公网可达的
——也就是说 Worker 并没有真正「保护」后端，它只是一跳转发。因此把前端挪到 hcyj
**不降低任何安全性**，却直接消除了被封锁的那一跳。

> **验证方法论上的教训**：早先曾用 Azure 香港作为观测点，得到 300/300 全通，
> 于是误判为「本地上游运营商的偶发抖动」。**境外观测点无法检测大陆封锁。**
> 判断大陆可达性必须用大陆观测点，且要多点交叉验证。

### 6. Vite 的 `base` 必须跟着部署前缀走

应用挂在 `/yeciorez/teleport/` 而不是域名根路径下，如果 Vite 用默认的 `base: '/'`，
`index.html` 会引用 `/assets/index-xxx.js`——这个路径在共享主机上会打到**别的服务**，
或者在当前服务上 404。

`vite.config.ts` 里 `base` 由 `ROUTE_PREFIX` 统一推导，构建产物引用的是
`/yeciorez/teleport/assets/...`。同时 `web/src/api.ts` 的前缀取自
`import.meta.env.BASE_URL`（也就是同一个 `base`），`web/src/router/index.ts` 的
`createWebHistory` 也用 `BASE_URL`。**前缀只写一处**，三者不可能漂移。

> 排查手法：构建后直接看 `backend/internal/webui/dist/index.html`，
> 里面**不应**出现任何 `src="/assets/...` 这种根绝对路径。

### 7. 未知 `/api/*` 路径必须仍是 JSON

当前端 SPA 接管了前缀根路径的兜底（返回 `index.html`）之后，一个未知的
`/api/nope` 会**匹配到 SPA 兜底**，于是 XHR 客户端收到一个 HTML 页面。
这比看起来更糟：客户端对 HTML 执行 `response.json()` 会抛解析错误，
报出来的是「服务器返回了非预期的响应」，而不是真实的 404。

解法是给 `p+"/api/"` 与 `p+"/s/"` 各注册一个更具体的兜底（`net/http` 会优先
匹配更长的模式）。`TestUnknownRouteWithSPAStaysJSON` 就是这条的回归测试，
把两个兜底删掉后它会失败。

---

## 安全设计要点

- **Agent 鉴权（双路）**：`POST /api/reports` 与所有 agent 接口要求 Bearer Token。
  第一条路对 `AGENT_SECRET_KEY` 做 `subtle.ConstantTimeCompare`（root），
  否则对来钥求 `sha256` 交给密钥表按 `UNIQUE` 索引查找；**两路都走完才判定失败**，
  避免用响应时间区分"root 不匹配"与"表里没有"。失败统一 401。
- **归属即权限**：`reports.owner_key_id` 记录发布者，非归属者的读 / 撤销一律 **404**
  （与"未知 token 与已吊销都返回 404"同一套语义）；root 密钥与面板不受归属限制。
- **失效链接语义**：未知 token 与被吊销 token 同样返回 **404**，避免通过状态码区分
  "从未存在"与"已被撤销"；仅**已过期**返回 **410**。分享页同样服务端返回 404/410，
  而不是先给 200 再由前端报错。
- **分享页面不缓存**：`Cache-Control: no-store`，防止已吊销链接残留在中间缓存。
  另加 `X-Robots-Tag: noindex` 与 `X-Frame-Options: DENY`。
- **静态资源缓存分层**：`/assets/*` 由 Vite 内容哈希命名，发
  `immutable` + 一年；`index.html` 与 `share.js` 是稳定文件名，发 `no-cache`，
  保证部署后立刻生效、不会把客户端钉在旧构建上。
- **路径穿越防护**：SPA 处理器用 `path.Clean` 折叠 `..`，并且 `fs.FS` 自身
  也会拒绝跳出根目录；测试里覆盖了 `../`、`%2e%2e`、`....//` 等写法。
- **XSS**：Markdown 渲染时**不**放行原始 HTML；`format: 'html'` 目前**关闭**
  （`HTMLMountEnabled = false`）。将来开启必须先接入净化库 + 严格 CSP。
- **管理面板**：HttpOnly + Secure + SameSite=Lax 的签名 Cookie（HMAC-SHA256），
  口令使用 PBKDF2-HMAC-SHA256（默认 600,000 次迭代，本地计算无运行时上限，
  校验时兼容旧的低迭代次数哈希）。也可用 Cloudflare Access 前置，
  此时接受 `Cf-Access-Authenticated-User-Email` 头。
- **Cookie 作用域**：`Path` 默认为应用前缀，避免在共享主机上外溢（见陷阱 4）。
- **CORS**：仅回显配置的来源，绝不反射任意 Origin。本部署是同源的，
  CORS 只对故意的跨源客户端生效。
- **服务加固**：systemd 沙箱选项 + 只监听 docker 网桥 + 独立系统用户。

### 密钥轮换

三个密钥都在 hcyj 的 `/data/services/teleport/teleport.env`（600 root:root）。
改完 **必须重启**（`EnvironmentFile` 只在进程启动时读取）：

```bash
ssh hcyj
vim /data/services/teleport/teleport.env    # 改对应那一行
systemctl restart teleport && systemctl is-active teleport
```

| 密钥 | 轮换影响 |
|---|---|
| `ADMIN_PASSWORD_HASH` | 旧口令立即失效。用 `./teleport hash-password '新口令'` 生成新值。**本次部署的口令是随机生成的，请首次登录后更换。** |
| `AGENT_SECRET_KEY` | **root / 应急密钥**，拥有全部权限。轮换只影响 root 用途；普通 agent 用自助申请的密钥，在面板上单独撤销 / 调期即可，不需要改这个环境变量。 |
| `SESSION_SECRET` | 所有已登录面板会话立即失效（签名密钥变了），需重新登录。 |

> 值里若含 `$`，在 systemd 的 `EnvironmentFile` 与 Go 的 `.env` 解析器中都按**字面量**处理，
> 无需转义；但**不要**用 `set -a; . teleport.env` 这种方式读它 —— shell 会展开 `$`，
> 导致哈希被破坏（本项目部署时踩过）。

---

## 后续路线

1. **HTML 安全渲染**：接入净化库 + nonce CSP 后开启 `format: 'html'`。
2. **运维**：给 `share_tokens` 的过期清理加定时任务（当前已有 `CleanupExpired` 逻辑，
   尚未挂调度）。
3. **面板增强**：报告删除、分页、按分类筛选服务端化、新建报告入口。
4. **渲染增强**：代码块复制按钮、Mermaid 图表导出 SVG/PNG、目录（TOC）锚点。
5. **备份**：SQLite 定期快照（当前无自动备份）。

---

## 已验证行为

### 生产环境（2026-10-10 实测）

链路：`https://api.hcyj.xyz/yeciorez/teleport/...` → Caddy → Go(:8788，内嵌前端) → SQLite

**可达性（本次架构改动的验收重点）**

| 观测点 | 结果 |
|---|---|
| 广东佛山电信家宽（`121.9.113.18`） | `GET /` → **200**，DNS 1.7ms / connect 18ms / TLS 36ms / 总 **48ms** |
| 广东佛山电信家宽 `GET /api/health` | **200**（44ms） |
| hcyj 本机（同机房公网出口） | `GET /` → **200**，`GET /api/health` → **200** |
| 旧入口 `https://teleport.zeroicey.me` | 已下线（Worker 删除、DNS 记录移除） |

对比改造前：同一台服务器上的 `api.hcyj.xyz/yeciorez/teleport/api/health` 本来就通，
只有 CF 前置的域名不通——这正是把前端挪过来的依据。

**功能与响应头**

| 探测 | 结果 |
|---|---|
| `GET /` | 200（SPA 外壳，`text/html`，`Cache-Control: no-cache`） |
| `GET /dashboard/reports` | 200（SPA 深链接回退，硬刷新后仍正常） |
| `GET /assets/index-*.js` | 200（`Cache-Control: public, max-age=31536000, immutable`） |
| `GET /assets/share.js` | 200（`Cache-Control: no-cache`）—— 2026-10-11 部署后**实测确认**；此前（`1c5ab91`）线上下发的是 `immutable`，这一行曾一度是错的 |
| `GET /assets/不存在.js` | 404（**不回退**成 HTML，避免误导性的 MIME 报错） |
| `GET /api/health` | 200 `{"status":"ok","environment":"production"}` |
| `POST /api/reports` 带令牌 | 201，返回分享链接 |
| `POST /api/reports` 无令牌 | 401 |
| `GET /api/share/:token` | 200，内容完整回读 |
| `GET /s/:token` | 200，服务端渲染 |
| 分享页响应头 | `Content-Security-Policy: default-src 'none'...`、`Cache-Control: no-store`、`X-Robots-Tag: noindex`、`X-Frame-Options: DENY` |
| `GET /s/未知token` | 404 |
| `GET /s/已吊销token` | 404（与未知一致，不泄漏状态） |
| `GET /s/已过期token` | 410（JSON 与页面均 410） |
| 非法 token 形状（过短） | 404 |
| 空 `title` | 400 `` `title` must be between 1 and 300 characters `` |
| 非法 `category` | 400 `` `category` must match /^[a-z0-9][a-z0-9_-]{0,31}$/ `` |
| 超过 `MAX_CONTENT_BYTES` | 413 |
| 未知 `/api/*` 路径 | JSON 封套 404（**即使 SPA 已接管前缀根**） |
| `POST /api/admin/login` 正确口令 | 200 + `Set-Cookie`（`HttpOnly; Secure; SameSite=Lax; Max-Age=43200`） |
| `POST /api/admin/login` 错误口令 | 401 |
| `GET /api/admin/reports` 无 Cookie | 401 |
| 分享 Token 新建 / 调整 / 禁用 / 恢复 / 删除 | 全部生效，`expiresInHours: 0` → `expires_at: 0`（永不过期） |
| 接口字段形状 | `ReportDetail`（含 `content` + `share_tokens`）与 `ShareToken`（变更接口含 `url`）均与前端类型定义一致 |

**真实浏览器验证**（Playwright + Chromium，从大陆家宽直连，2026-10-10）：

管理面板（19/19 通过）：

| 检查 | 结果 |
|---|---|
| SPA 外壳加载 | URL 保留前缀 `/yeciorez/teleport/dashboard`，`#app` 挂载成功 |
| 未登录跳转 | 自动进入登录路由 |
| 错误口令 | 由真实 401 驱动，界面显示「密码错误」 |
| 正确口令 | 进入 `/dashboard/reports`，报告列表可见 |
| 会话 Cookie | `Path=/yeciorez/teleport`（**限定应用前缀**）、`HttpOnly`、`Secure`、`SameSite=Lax` |
| 硬刷新深链接 | 仍处于登录态，报告列表正常 |
| 报告详情 | 进入 `/dashboard/reports/<uuid>`，分享链接含完整前缀 |
| 控制台错误 / 失败请求 | 均为 0 |

分享页（10/10 通过）：

| 检查 | 结果 |
|---|---|
| HTTP 200 | 未登录可访问 |
| CSP | `default-src 'none'; ...` 生效，**无 CSP 违规** |
| 缓存 | `Cache-Control: no-store, must-revalidate` |
| 反索引 | `X-Robots-Tag: noindex, nofollow, noarchive` |
| Markdown 服务端渲染 | `<h1>` 存在 |
| Mermaid | 由内嵌 bundle 渲染出 `<svg>`（1 个） |
| 脚本同源 | `share.js` 的 src 为 `/yeciorez/teleport/assets/share.js`，满足 `script-src 'self'` |
| 控制台错误 | 0 |

### 测试覆盖

Go 侧单元 / 集成测试（`pnpm run backend:test`，全部通过，含 `-race`）：

| 包 | 覆盖内容 |
|---|---|
| `internal/markdown` | XSS / 原始 HTML / 危险 URL / 扩展渲染 / 代码块 / Mermaid / 转义 / 并发 |
| `internal/store` | 迁移幂等、创建与过期语义、解析状态、吊销、PATCH、计数与删除、列表筛选、`json_valid` 约束、`updated_at` 触发器、清理、外键级联、并发写 |
| `internal/httpx` | 响应封套、错误隐藏、Bearer 变体、Session 往返 / 篡改 / 过期、Cookie 属性、严格 CORS、panic 恢复、请求 ID、真实 IP |
| `internal/validate` | 默认值、空串视作缺省、`autoShareHours: 0` 有效、全部拒绝分支、边界值、UTF-16 emoji 计数、JS `trim()` 语义、请求体解析 |
| `internal/spa` | 构建目录校验、SPA 深链接回退、哈希资源 `immutable`、缺失资源硬 404、`/api`「/`s` 不回退成 HTML、**路径穿越**（`../`、`%2e%2e`、`....//`）、非读方法 405、空前缀挂载 |
| `internal/aidoc` | **无未替换占位符**、占位符确实生效、没有声明却未使用的占位符、文档随配置变化、契约字段齐全（端点/Bearer/`data.share.url`/404/410/401）、**不含密钥**、HTML 页无 `<script>` 且 CSP 无 `script-src`、`llms.txt` 保持为指针（不复制正文）、HTML 渲染开关与文档说法一致的绊线 |
| `internal/api` | 健康检查、鉴权、主流程、校验矩阵、体积上限、公开读取与计数、404/410、分享页渲染与 CSP、面板登录登出、前缀挂载断言、CORS 预检、未知路由（**含 SPA 接管根路径后仍返回 JSON 的回归测试**）、**AI 说明三端点免鉴权且不被 SPA 吞掉** |

前端：`pnpm run typecheck`、`pnpm run build`、`pnpm run db:check-schema` 均通过。

> `TestUnknownRouteWithSPAStaysJSON` 是刻意设计的回归测试：把
> `mux.Handle(p+"/api/", root)` 与 `p+"/s/"` 两行删掉，它会**立刻失败**，
> 因为未知 API 路径会退化成 SPA 的 HTML 404。

> `aidoc` 的 `TestNoUnresolvedPlaceholders` 同理：在 `guide.md` 里写一个没有对应替换器的
> `{{NEW_FACT}}`，它会让构建失败 —— 而不是让线上文档告诉 AI 去调用 `{{NEW_FACT}}/api/reports`。
> `TestGuideContainsNoSecret` 则是被真实险情验证过的：初版文档写了服务器上密钥文件的路径，
> 该测试当场拦下（见 `.ai/pitfalls/cases/2026-10-10-public-doc-disclosed-secret-file-path.md`）。
