# teleport — agent instructions

Teleport 是「个人 AI 报告展示 + 时效分享」平台：AI 用一次 `POST /api/reports` 上报 Markdown
报告，拿到一条带有效期的分享链接。**前端、API、分享页全部由同一个 Go 二进制提供**，部署
就是上传一个文件、重启服务。

## 命令

```bash
pnpm install                    # 依赖（pnpm only，仓库禁用 npm/yarn）
pnpm run dev:web                # Vite 开发服务器（HMR，/api 与 /s 代理到 127.0.0.1:8788）
pnpm run build:release          # 发布构建 → bin/teleport-linux-amd64（前端 + 后端单文件）
pnpm run check                  # 闸门：scripts/check.sh（typecheck + gofmt + vet + schema 快照 + sqlc 漂移 + go test）
```

分项与「只跑一条测试」：

```bash
pnpm run typecheck                                    # vue-tsc，前端类型
pnpm run db:gen-sqlc                                  # 重新生成 backend/internal/store/storedb
pnpm run backend:test                                 # cd backend && go test ./...（无 DSN 时 PG 测试 SKIP）
pnpm run backend:test-pg                              # 强制真库闸门（无 TEST_PG_DSN 直接退出）
cd backend && go test ./internal/store/ -run TestResolveShare -v -race   # 单条测试
pnpm run backend:vet                                  # go vet
```

`TEST_PG_DSN` 取法（本机）：

```bash
export TEST_PG_DSN="postgres://teleport:$(sudo -n cat /data/services/teleport/.pgpassword.tmp)@127.0.0.1:5432/teleport_test?sslmode=disable"
```

Go 工具链：`go 1.26` 模块在 `backend/`，本机 `go` 在 `/usr/local/go/bin`，
国内拉包必须 `GOPROXY=https://goproxy.cn,direct`（默认 proxy.golang.org 超时）。

## 目录

| 路径 | 职责 |
| --- | --- |
| `backend/main.go` | 启动 / 优雅退出 / `hash-password` / `migrate` / `version` 子命令 |
| `backend/internal/config/` | 环境变量读取与校验；`Route(suffix)`、`AppBaseURL()` 是前缀的唯一来源 |
| `backend/internal/api/` | 路由挂载（`net/http` ServeMux），前缀 `cfg.RoutePrefix` |
| `backend/internal/httpx/` | 中间件、响应封套、Session 签名、Bearer 鉴权 |
| `backend/internal/store/` | PostgreSQL 访问层（pgx + sqlc）；`//go:embed migrations_pg/*.sql` |
| `backend/internal/store/queries/` | sqlc 查询源（`*.sql`）——SQL 的唯一真源 |
| `backend/internal/store/storedb/` | `sqlc generate` 产物，**勿手改** |
| `backend/internal/store/storetest/` | 每测一个 schema 的 PG 夹具 |
| `backend/internal/markdown/` | goldmark + Chroma 服务端渲染（`html:false`） |
| `backend/internal/views/` | 分享页服务端渲染 + CSP |
| `backend/internal/spa/` | 内嵌前端托管（SPA 回退 / 缓存 / 路径穿越防护） |
| `backend/internal/aidoc/` | 给 AI 读的使用说明（`/ai`、`/ai.md`、`/llms.txt`） |
| `backend/internal/webui/` | `//go:embed` 前端产物（`dist/` 是生成物，勿手改） |
| `web/src/` | Vue 3 面板 + 分享页客户端（Mermaid 懒加载） |
| `scripts/build.sh` | 发布构建：先 Vite 后 Go，顺序不能反 |
| `scripts/check-sqlc-sync.sh` | `sqlc diff`：生成物与 SQL 不得漂移 |
| `backend/sqlc.yaml` | sqlc 配置（engine postgresql、pgx/v5） |
| `backend/internal/store/migrations_pg/` | 合并式 PG 基线（**唯一** schema 真源） |
| `schema.pg.sql` | 由 `migrations_pg/` 生成的 DDL 快照，逐字节一致 |

## 硬约定

- **`pnpm` only。** 不引 npm/yarn；`pnpm-lock.yaml` 是唯一锁文件，`pnpm-workspace.yaml` 不要改。
- **前缀只写一处。** `/yeciorez/teleport` 由 `ROUTE_PREFIX` 推导，Go 路由、`vite.config.ts`
  的 `base`、`web/src/api.ts` 的 `BASE_URL`、分享链接全部从它来。Caddy 必须用 `handle`
  而不是 `handle_path` —— 前缀必须**保留**给后端。
- **markdown 报告永不渲染原始 HTML。** goldmark 不加 `WithUnsafe`，报告里的
  `<script>` 一律被替换为 `<!-- raw HTML omitted -->`。这条不因为下面那条而放宽。
- **`format: "html"` 报告原样渲染**（2026-10-11 用户裁决，见
  `.ai/decisions/2026-10-11-html-report-rendering.md`）：正文**逐字节**作为独立文档
  由 `GET /s/{token}/html` 返回，分享页用一个同源 iframe 套住它；**不净化、不加 nonce**。
  代价（已知并接受）：报告里的 JS 与面板同源，可以对 `/api/admin/*` 以查看者身份发请求。
  想隔离就设 `views.HTMLReportSandbox`，但要同时改分享页的高度测量方式。
  **改这条之前先改决策记录，两个文件的说法必须一致。**
- **时间一律 epoch 毫秒**（`*_at` 列），`expires_at = 0` 表示永不过期。
- **链接语义不许改**：未知 token 与已吊销都返回 **404**（不泄漏状态），仅过期返回 **410**。
- **响应封套不许改**：`{ok:true,data,requestId}` / `{ok:false,error:{code,message},requestId}`。
- **未知 `/api/*` 必须仍是 JSON**，不能退化成 SPA 的 HTML 404。
- **密钥永不入库**：`AGENT_SECRET_KEY`、`SESSION_SECRET`、`ADMIN_PASSWORD_HASH` 只在服务器
  的 `teleport.env`（600 root:root）。仓库里只放 `.dev.vars.example`。
- 改 `backend/internal/store/migrations_pg/`（或 `queries/*.sql`）后必须跑 `pnpm run db:gen-sqlc`
  与 `scripts/gen-schema-pg.sh`；改前端后必须跑 `pnpm run build`（否则 `//go:embed` 打进去的是旧产物）。
- **PostgreSQL 实例是共享的**（clip / registry / serenique，`max_connections=100` 共用）。
  夹具与脚本的连接预算是小整数（`DB_MAX_CONNS` 默认 8，storetest 每测 1+4），
  **不要在循环里开连接池**。
- **不要提交** `bin/`、`backend/data/`、`backend/internal/webui/dist/`（都是生成物，已 gitignore）。
- `git commit` / `git push` / 线上部署前先问人类。

## 完成定义

1. `pnpm run check` 全绿（typecheck + gofmt + vet + schema 快照 + sqlc 漂移 + Go 测试）。
   触碰存储或 SQL 时另需 `pnpm run backend:test-pg`（真库，不可跳过）。
2. `pnpm run backend:vet` 无告警。
3. 涉及线上行为时，从**大陆观测点**验证（境外观测点无法检测大陆封锁，见 `.ai/pitfalls/`）。
4. 行为、决策、陷阱的变更在**同一次改动**里写回 `.ai/`。

_Read by: pi._

<!-- engram:managed -->

<!-- engram:contract:start v1 -->
## Project memory bank (`.ai/`) — required contract

This repository keeps a portable, tool-independent memory bank in `.ai/`. It outranks chat
history and your own recollection. Read it on demand with file tools; never paste it wholesale.

### Read before you act

1. `.ai/CURRENT_TASK.md` — the current goal, in-progress state and blockers. Read this first.
2. Newest-first scan of `.ai/decisions/` for `💭 PROPOSAL` / `✅ ACCEPTED` entries that touch
   your task. An accepted decision is binding; a proposal needs a verdict, not silent adoption.

### Read on demand (trigger → file, not a bulk preload)

| Trigger | Read |
| --- | --- |
| changing architecture, stack, module boundaries | `.ai/ARCHITECTURE.md` |
| deploy, env vars, CI, incidents, ops | `.ai/runbooks/<topic>.md` |
| a bug that feels familiar / "we fixed this before" | `.ai/pitfalls/cases/<case>.md` |
| resuming after context loss, compaction or a long session | newest `.ai/sessions/*-handoff.md` |
| brainstorming a direction, before writing code | `.ai/decisions/` (write a `💭 PROPOSAL`) |

### Write back (not optional)

| Event | Write |
| --- | --- |
| a choice that is expensive to reverse | `.ai/decisions/YYYY-MM-DD-<topic>.md` via `/remember-decision` |
| a pitfall you hit, or a silent failure mode you decoded | `.ai/pitfalls/cases/<case>.md` via `/remember-pitfall` |
| ending a session with unfinished or fragile work | `.ai/sessions/YYYY-MM-DD-<topic>-handoff.md` via `/handoff` |
| any change to goal, state or blockers | `.ai/CURRENT_TASK.md` (same edit turn, never "later") |

Rules of the bank:

- One file per topic, `YYYY-MM-DD-<kebab-topic>.md`, status header on line 1 of the body:
  `💭 PROPOSAL` → `✅ ACCEPTED` → `🪦 REJECTED`. Flip the status, never rewrite history.
- Keep entries short and factual. The bank is an index of decisions, not a diary.
- Canonical skill specs live in `.ai/skills/*.md`. If your tool can load them, load them; if
  it cannot, this table is the fallback contract — follow it literally.
- If the bank and the code disagree, the bank is stale: fix the bank in the same change.
<!-- engram:contract:end -->
