# Current task

**Status:** 🚧 **P0 ✅ / P1 ✅（附一处更正）/ P2 待批** —— 契约已钉住（243 → 247 个测试）；
PostgreSQL 库、角色、基线迁移、快照与漂移守卫都已就位。**P2 的测试策略已写成 ADR 提案，
等人类裁决**（`.ai/decisions/2026-10-16-p2-test-strategy.md`）。
**Updated:** 2026-10-16
**目标：** 把 teleport 重写到 Gin + pgx/sqlc + PostgreSQL，**在野契约逐字节不变**，
然后部署到 hpcore（本机），让 hcyj 只剩转发。

---

## 现在这个仓库是什么

2026-10-16 从共用仓库里拆出来：用户裁决「分成两个仓库 不要塞在一个项目里面去了」。
本仓库用 `git archive HEAD` 从共用仓库导入了**线上正在跑的那一版**（提交 `a288b15`），
环境是**忠实副本**，不是重构后的起点 —— 这样 243 个测试与 23 条 pitfall 守卫就是重写的规格说明书。
（clip 在它自己的仓库里，已经上线；两边共享的只有 hcyj 的 Caddy、本机 Postgres 实例、frp 机制。）

**本次导入**：`backend/`（net/http + SQLite）、`web/`（Vue 面板，不重写）、`schema.sql`、
243 个测试函数、`.ai/` 全量记忆库。导入提交 `ef71255`。

## 决策（先读这个）

**`.ai/decisions/2026-10-16-rewrite-on-gin-and-postgres.md`** —— 本仓库的纲领：

1. **第一张表是冻结清单**（14 项在野契约：agentkey 派生标签、cookie/头名、路由前缀、
   封套与命名、404/410 语义、归属边界、epoch 毫秒、续期规则、markdown 与 `format:"html"`、
   一次性凭据的 fragment 规则）。改任意一项都会让**已发出的密钥或在野链接失效**。
2. 数据库：同实例、独立 database `teleport`，时间列仍是 `BIGINT` epoch 毫秒。
3. 包映射表 + `database/sql → pgx` 的四类语义差异（`ErrNoRows`、`OnConflict`、
   SQLite 专有函数、**失去单写者保证后必须显式加锁的三处**）。
4. 分阶段 P0–P6，每阶段结束都要能跑旧测试。
5. 验收：243 个测试对**真库**全绿 + 契约逐条实测 + 真实浏览器 + 大陆观测点 + 数据搬运核对。

## 下一步（严格按顺序）

| # | 任务 | 为什么排在这里 |
| --- | --- | --- |
| **P0** | ✅ 清点完成 + 补上缺口：审计发现 **agentkey 的三个派生标签零测试覆盖**（最危险的一条），已加 4 个守卫（标签字面量 + 黄金字节 + 独立重算 + 续期域分离），并做了两次反证确认会变红。剩下的 11 项契约已由现有测试钉住（见下） | 重写期间的每一次"绿"必须是同一个判据下的绿 |
| **P1** | ✅ 完成（**附更正，见下**）：库/角色 `teleport`（非超级用户）、`migrations_pg/0001_init.sql`（合并式基线）、生成的 `schema.pg.sql`、`MigratePG` 运行器、8 个新测试（在真库 `teleport_test` 上验证 4 处**必须改对**的转换）、两道漂移守卫 | schema 是后面一切的接口 |
| **P1.1** | ✅ 更正：P1 记的「在真库上验证」当时**已经不成立** —— `teleport_test` 是**未提交的中间版本**建的库，合并式基线按文件名记录、永不重放，于是库里留着 `CHECK ((metadata)::json IS NOT NULL)` 而文件里是 `CHECK (metadata IS JSON)`，`TestPGMetadataCheckRejectsInvalidJSON` 实际是红的（期望 23514，得到 22P02）。已 DROP/CREATE 重建 `teleport_test` 并复跑 → 8/8 绿。两道漂移守卫是**文件对文件**，看不到真库。见 pitfall `2026-10-16-merged-baseline-hides-a-stale-test-db.md`。生产意图库 `teleport` 当时仍是空的，P5/P6 未受影响 | 「绿」必须真的对应当前文件；否则后面每一次绿都不可信 |
| P2 | `internal/store/` 逐函数换成 pgx + sqlc（每换一个跑一遍现有 store 测试）—— **注意：SQLite 的单写者保证在 PG 里消失，待批准上限/密钥上限/分享链接计数这三处必须显式加锁**。**开工前先定测试策略**（见 ADR 提案） | 存储语义最容易在不知不觉中变 |
| P3 | `internal/httpx` → Gin 中间件；`internal/api` 移植 | 封套与状态码必须逐条对齐 |
| P4 | 全链路起新版（Gin + pgx），243 测试 + 浏览器验证 | 切换前的最后一道闸 |
| P5 | SQLite → PG 的数据搬运脚本 + 行数核对（5 报告 / 2 密钥 / 全部分享链接） | 切换当天不能靠猜 |
| P6 | 切到 hpcore（frpc 隧道 8788；hcyj 的 Caddy **不需要改**，它本来就指 `172.17.0.1:8788`） | 用户要的最终形态：hcyj 只转发 |

**保持冻结的线上环境**：重写期间**不要**改 hcyj 的 Caddy、不要升级本机 Postgres、
**绝对不要轮换 `SESSION_SECRET`**（它会让所有派生密钥失效）。

## 未做 / 需要人类

- **P2 的测试策略等裁决**：`.ai/decisions/2026-10-16-p2-test-strategy.md`（建议 B：每测一个 schema，从内嵌迁移现建现拆）。P2 的任何代码都排在这个裁决之后。
- 本仓库在 GitHub 上还不存在（本地目录 `/data/dev/teleport-gin`）。
- 线上仍是旧版（hcyj，SQLite + net/http），用户裁决「等重写版就绪再切」。
- 与 clip 共享的基础设施改动要**同时想两边**（hcyj 的 Caddyfile 同时服务两个站点）。

## 长期约束（改动前必须知道）

- 冻结清单（见决策记录第一张表）**不可改**；改之前必须先改决策记录。
- **归属是安全边界**：非归属者的读/改/删一律 404 而不是 403，且判定在任何写入之前。
- **密钥是派生的，不是暂存的**；服务端只存 `sha256`。
- **一次性凭据只走 URL fragment**，页面无 JS 时惰性，`HEAD` 领取必须 405。
- **封套与命名**：`{ok,data,requestId}`；请求 camelCase、响应 snake_case；未知字段 400。
- **时间一律 epoch 毫秒**；`expires_at=0` = 永不过期，且「永不过期」只能由人类显式给出。
- **限流桶键必须用 `httpx.ClientIP`**，绝不用任何 RealIP 式实现。
- **markdown 报告永不渲染原始 HTML**；`format:"html"` 的原样渲染是**唯一的例外**，有双向绊线测试。
- 大陆可达性是硬约束；验证必须用大陆观测点。
