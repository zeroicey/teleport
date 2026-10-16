# teleport 重写：Gin + pgx/sqlc + PostgreSQL，在野契约逐字节冻结

**Status:** ✅ ACCEPTED · **Date:** 2026-10-16 · **Deciders:** 用户（"你把 teleport 重写成 gin 那一套就行"）
· **Supersedes:** none · **Related:** clip 仓库的 `2026-10-16-two-repositories.md`

## 背景

teleport 是「AI 报告发布 + 时效分享」服务：AI 用一次 `POST /api/reports` 把 Markdown 上报为
在线网页，拿到一条带有效期的分享链接。它现在**正在生产运行**（hcyj，`api.hcyj.xyz/yeciorez/teleport`，
SQLite + 手写 `net/http`），并且身上压着三类不可再生的资产：

1. **在野分享链接**（`expires_at = 0` 意味着永久有效）；
2. **已发出的 agent 密钥**（token 由 `HMAC(K_derive, "teleport/agent-key/v1|appID|claimSecret")`
   **派生**，`K_derive` 来自 `SESSION_SECRET`）；
3. **243 个测试函数 + 23 条归档 pitfall**，它们是这套系统真正被证明过的部分。

用户要求把它重写到与 clip 相同的那套栈（Gin + pgx/sqlc + PostgreSQL），单独一个仓库。

**这次重写的危险不在于写新代码，而在于悄悄丢掉旧代码里已经证明过的性质。**
所以下面第一张表是冻结清单，它是本 ADR 的核心。

## 一、冻结清单（改一个字节都会造成在野不兼容）

| 冻结项 | 具体值 / 位置 | 改了的后果 |
| --- | --- | --- |
| 密钥派生标签 | `"teleport/derive/agent-key/v1"`、`"teleport/agent-key/v1"`、`"teleport/agent-key/renewal-claim/v1"` | **所有已发出的 agent 密钥当场失效**（token 是派生的，标签是输入） |
| Cookie 名 | `teleport_session` | 所有人被登出 |
| 请求头 | `X-Teleport-Claim`、`X-Teleport-Key-Expires-At`、`X-Teleport-Key-Expired` | 外部 agent 已按 `/ai.md` 里的头名调用 |
| 路由前缀 | `/yeciorez/teleport/*`（含 `/s/`、`/k/`、`/ai`、`/ai.md`、`/llms.txt`） | 在野链接 404；且 `expires_at=0` 让它**永久**不可挽回 |
| 封套 | `{ok:true,data,requestId}` / `{ok:false,error:{code,message},requestId}` | 所有客户端与文档 |
| 命名约定 | **请求体 camelCase、响应 snake_case**；`PATCH` 遇未知字段 400 | 静默丢字段（clip 仓库里刚踩过一次，见其 pitfalls） |
| 链接语义 | 未知 token 与已吊销 → **404**；仅过期 → **410** | 泄漏状态 |
| 归属边界 | 非归属者的读/改/删/撤销一律 **404 而不是 403**，且判定必须在任何写入之前 | 存在性 oracle |
| 时间约定 | 全部 `*_at` 是 epoch **毫秒**，`expires_at = 0` = 永不过期 | 语义漂移 |
| 续期规则 | 只延长、绝不缩短；`expires_at=0` 保持 0；「永不过期」只能显式给出 | 静默缩短用户凭据寿命 |
| markdown 渲染 | goldmark **不加** `WithUnsafe`，原始 HTML 一律 `<!-- raw HTML omitted -->` | XSS |
| `format:"html"` 例外 | 正文逐字节由 `GET /s/{token}/html` 返回、同源 iframe 套住，**不净化**；`aidoc` 有双向绊线测试 | 见其专有决策记录 |
| 一次性凭据 | claim secret 只在 **URL fragment**；领取页无 JS 时惰性（按钮默认 `hidden`）；`HEAD` 领取必须 405 | 聊天工具预览会烧掉一次性链接 |
| 密钥永不入库 | 服务端只存 `sha256(token)`；明文只在领取那一刻派生出来 | 快照泄漏可用凭据 |

**2026-10-16 修正（路由前缀那一项）**：见 `2026-10-16-teleport-own-hostname.md`。
新增 `https://teleport.hcyj.xyz/` 根路径入口（`ROUTE_PREFIX=""`），而**旧前缀
`/yeciorez/teleport/*` 必须永久保留** —— 因为 `expires_at=0` 的链接永不过期，删掉前缀
就是永久碎掉它们。这一项的含义因此从「前缀不可改」变成「**旧前缀必须一直有效**」，
比原来更严，不是更松。已核实：agent 密钥只用 `SESSION_SECRET`+两个标签+applicationID+
claimSecret 派生（`agentkey.DeriveToken`），**与域名/前缀无关**，所以这次变更不失效凭据。

**做法**：先建一张 `contract_test`（或沿用现有测试文件）把这 14 项逐条钉住，
**在重写开始之前**让它对旧实现全绿。这样重写期间每一次「绿」都是同一个判据下的绿。
现有测试里已经覆盖了大部分 —— 先清点，缺的补上，再动手改实现。

## 二、数据库

| 决策 | 值 | 依据 |
| --- | --- | --- |
| 实例 | 与本机现有 Postgres 同实例，**独立 database `teleport`** | 用户裁决；备份/迁移各自独立一份 |
| 角色 | 复用 `relay`（非超级用户），或新角色 `teleport` —— 见下 | 最小权限 |
| 时间列 | `BIGINT` epoch 毫秒（**不是** `timestamptz`） | 冻结清单里的时间约定 |
| 二进制 | `reports.content` 仍为 `TEXT`；`format` 用 `CHECK IN ('markdown','html')` | 与现有 CHECK 一致 |
| 唯一性/并发 | 待批准上限（`KEY_MAX_PENDING`）必须**原子**强制：`SELECT … FOR UPDATE` 或 advisory lock | 现有实现在 SQLite 里靠单写者；Postgres 不再提供这个免费保证 |
| 归属 | `owner_key_id` 列**永不可更新**（加触发器或收紧 GRANT 更可靠） | 归属是安全边界 |

**从 SQLite 搬数据**（5 篇报告 / 2 把密钥 / 若干分享链接）：一次性脚本，
按 `id` 逐行搬运并**核对行数与关键边界**（`owner_key_id=''` 的 root 报告、
`expires_at=0` 的永不过期链接、已吊销的 token）。

## 三、包映射

| 现在（net/http + SQLite） | 重写后（Gin + pgx/sqlc） | 备注 |
| --- | --- | --- |
| `backend/main.go` | `cmd/teleport/main.go` | systemd 子命令（`hash-password`/`migrate`/`version`）保留 |
| `internal/api/router.go`（43k 字符的手搓路由） | `internal/api/router.go`（Gin 路由表） | **`{ok,data,requestId}` 自己写**，不用 Gin 默认 JSON |
| `internal/api/keys.go` 等 handler | 逐个移植到 Gin 的 `HandlerFunc` | 原样保留每个状态码 |
| `internal/store/*`（SQLite + 手写 SQL） | `internal/store/*`（pgx + sqlc 生成） | **条件 UPDATE + `RowsAffected` 的语义必须保留**（所有权判定写在 WHERE 里） |
| `internal/httpx/{errors,auth,middleware}.go` | 同名包，中间件改成 Gin 中间件 | `ClientIP` 保持不变（clip 仓库里已有一份可参照的副本） |
| `internal/views/*`（SSR 分享页/交接页） | 不变（仍是服务端渲染 HTML + 严格 CSP） | 与框架无关，几乎不用改 |
| `internal/markdown/`、`internal/validate/`、`internal/agentkey/`、`internal/password/`、`internal/aidoc/` | **原样保留** | 与 HTTP 框架和数据库都无关 |
| `internal/spa/`（内嵌 Vue 托管 + 缓存头） | 原样保留（`http.FileServer` 与 Gin 可共存） | Vue 前端**不重写** |
| `internal/domain/` | 保留 | 纯类型 |

**`database/sql` → `pgx` 的语义差异是本次重写最危险的地方**，必须逐条处理：

- `sql.ErrNoRows` → `pgx.ErrNoRows`（漏掉就会把「不存在」当成 500）；
- `ON CONFLICT DO NOTHING` + `RowsAffected==0` 判「已存在」：Postgres 里**同样成立**，
  但要注意 `DO NOTHING` 在并发下会静默跳过，而 SQLite 的 `_txlock=immediate` 曾让这种竞态消失；
- SQLite 的 `max(...)`、`json_valid()`、`PRAGMA` 全部要重新表达（`GREATEST`、`CHECK`、去掉 PRAGMA）；
- 事务隔离：SQLite 的单写者保证没了 → **待批准上限、密钥上限、分享链接计数**这三处必须显式加锁。

## 四、分阶段（每一阶段结束都要能跑起旧测试）

| 阶段 | 内容 | 完成判据 |
| --- | --- | --- |
| P0 | 建 `contract_test`，把 14 项冻结契约逐条钉住（对旧实现先跑绿） | `go test ./...` 全绿，契约条目清点完整 |
| P1 | 建 Postgres database/角色 + 迁移目录 + `schema.sql` 的 PG 等价物 + `db:check-schema` 的替代守卫 | 空库可迁移、可回滚；`sqlc generate` 通过 |
| P2 | `internal/store/` 换成 pgx + sqlc（**逐函数**，每个函数跑一遍现有 store 测试） | `internal/store` 测试全绿（对着真库） |
| P3 | `internal/httpx` → Gin 中间件；`internal/api` 路由与 handler 移植 | `internal/api` 测试全绿 |
| P4 | 全链路：`cmd/teleport` 起 Gin + pgx，跑 243 个测试 + 真实浏览器验证 | `scripts/check.sh` 全绿 + 浏览器 27/27 级别的验证 |
| P5 | 数据搬运（SQLite → PG）脚本 + 行数核对 | 5 报告 / 2 密钥 / 所有分享链接可读 |
| P6 | 切换：hpcore 起新版 + frpc 隧道 8788 + hcyj Caddy ~~不变（它本来就指 `172.17.0.1:8788`）~~ **需要改**：加 `teleport.hcyj.xyz` site block + 证书，并保留原有 `handle /yeciorez/teleport*`（见 `2026-10-16-teleport-own-hostname.md`） | 公网 URL 200、分享链接可读、agent 密钥可用、**两个入口都活着** |

**P6 之后 hcyj 上只剩转发**，与 clip 的形态一致。

> **2026-10-16 修正**：「切换前旧版一直服务」与实情不符 —— 旧版已在 hcyj 上
> `disabled`+`inactive`，公网入口 502（用户当时确认无人使用，并再次裁决「继续停着等新版上线」）。
> 后果：`expires_at=0` 的在野链接**现在就是打不开的**；P5 搬运后无法马上拿旧版做对账。
> 回滚材料（旧二进制 + SQLite + `teleport.env`）仍原样留在 `/data/services/teleport/`。

## 五、验收（不能只靠单元测试）

1. **243 个测试全绿**（对真 Postgres，不是 mock —— mock 会让所有权/配额/并发守卫形同虚设）。
2. **契约清单逐条实测**：用一个真 agent 密钥 `POST /api/reports`、读分享页、
   `PATCH` 改、`DELETE`、过期链接 410、未知 token 404、`HEAD` 领取 405。
3. **真实浏览器**跑面板与分享页（沿用 `bin/e2e/` 里的 CDP 脚本模式）。
4. **大陆观测点**测公网 URL（hcyj 本机 / bjbuwe / koma）。
5. **数据搬运后逐项核对**：报告数、密钥数、`expires_at=0` 的链接仍永久、已吊销仍 404。

## 六、已知风险

- **`SESSION_SECRET` 与 agent 密钥耦合**：轮换它会让所有派生密钥失效 —— 重写期间**不要碰它**。
- **归属语义的坑**：具名密钥**读不到**迁移前 root 发布的报告（`owner_key_id=''`）。
  这是默认拒绝的正确方向，不是 bug，但搬运时要保持（并保持可解释）。
- **`CleanupExpired` 从未排期**：重写时顺手要决定它到底跑不跑（现在只是没做）。
- **重写期间的线上变更冻结**：不要同时改 hcyj 的 Caddy 或 Postgres 版本，
  否则出问题时无法区分是重写还是环境。
