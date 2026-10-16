# Current task

**Status:** 🚧 **P0 ✅ / P1 ✅（附更正）/ P2 🚧 进行中** —— 已交付：每测一个 schema 的 PG 测试
夹具 + 两层闸门（`scripts/test-pg.sh`）；已用**确定性实验**证实「计数—插入」在上限上的竞态
在 PG 里真的存在，并验证了替代它的 advisory lock。store 本体（pgx + sqlc）尚未开写。
**Updated:** 2026-10-16
**目标：** 把 teleport 重写到 Gin + pgx/sqlc + PostgreSQL，**在野契约逐字节不变**，
然后部署到 hpcore（本机），让 hcyj 只剩转发。

---

## 线上现状（2026-10-16 实测）

| 事实 | 值 |
| --- | --- |
| teleport 的公网入口 | 仍是 `https://api.hcyj.xyz/yeciorez/teleport`（hcyj 的 Caddy `handle /yeciorez/teleport*` → `172.17.0.1:8788`，前缀保留） |
| 现在是什么状态 | **502**。hcyj 上 `teleport` unit ：`disabled` + `inactive`；frps 只监听 `8790`（clip），`8788` 无监听。**用户 2026-10-16 再次裁决：继续停着，等新版上线**（不临时恢复） |
| `teleport.hcyj.xyz` | **不存在**。DNS 解析到同一个 IP，但 hcyj 的 Caddyfile 里**没有任何 site block**、也没有证书 → TLS 直接失败（`code=000`，`no peer certificate available`）。全仓库与记忆库里搜不到这个字符串 |
| 有自己独立域名的那个是 | **clip**（`clip.hcyj.xyz`）。Caddyfile 注释里写着 `A whole host of its own, unlike teleport` —— 很可能就是这行造成了「teleport 也有自己的域名」的印象 |
| `https://api.hcyj.xyz/` 的 200 | 那是 Caddy 的 fallback `handle` → `172.17.0.1:3000`（**别的服务**），不是 teleport。链路/TLS 健康不等于 teleport 可用 |

要临时恢复旧版（让 8788 重新有服务）：`ssh hcyj 'systemctl enable --now teleport'`。
**当前裁决是「不恢复」** —— 用户确认无人使用，等重写版就绪再切，所以 502 是预期状态，不是事故。
代价（已接受）：`expires_at=0` 的在野永久链接现在打不开；P5 搬运后无法马上拿旧版做对账。

**新域名 `teleport.hcyj.xyz` 已裁决**（见 `2026-10-16-teleport-own-hostname.md`）：
新版挂在**根路径**，**P6 与切换一起做**（现在不加 Caddy，否则新域名也是 502）。

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
| P2 | `internal/store/` 换成 pgx + sqlc —— **注意：SQLite 的单写者保证在 PG 里消失，计数—插入式的上限必须显式加锁**。**做法已裁决为 B（一次性大爆炸）** | 存储语义最容易在不知不觉中变 |
| ↳ **P2.1** | ✅ 测试夹具 + 闸门：`internal/store/storetest`（每测一个 schema，`search_path` 锁定，用完即拆）、`scripts/test-pg.sh`（无 DSN 拒绝运行 + 自检守卫）、`check.sh` 在跳过 PG 时大声提醒。7 个新测试（3 个夹具守卫 + 3 个上限锁验证 + 1 个旧真库测试改用夹具） | 没有可信的夹具，后面每一次「绿」都不可信 |
| ↳ **P2.2** | ✅ 锁定设计已验证：`pg_advisory_xact_lock` 的三个键（`pg.go`）。确定性实验证明 READ COMMITTED 下 cap=1 能落 2 行（不是假设）；锁能把第二个事务真的擋住并使 cap 精确，24 竞争者×5 轮 → 每轮恰好 3 个赢 | 先有靶子再改实现，否则锁加错地方也看不出来 |
| ↳ **P2.2b** | ✅ 迁移内容哈希守卫：`schema_migrations` 记 sha256，库与文件不符就拒绝启动（含旧迁移器建的库）。4 个测试 + 端到端反证（改文件→拒绝，撤销→通过，文件与 HEAD 逐字节相同） | 把 2026-10-16 那次事故从「靠守卫事后发现」变成「源头不可能发生」 |
| ↳ **P2.3** | ⛔ 待做：`internal/store/`（`queries.go` + `keys.go`）改成 pgx。**执行清单见 `.ai/sessions/2026-10-16-p2-harness-and-locks-handoff.md` 第三节**：方言差异表、13 处原始句柄、要删的 SQLite 残留、sqlc 计划、三处上锁 | P2 的主体，也是最大的一块 |
| P3 | `internal/httpx` → Gin 中间件；`internal/api` 移植 | 封套与状态码必须逐条对齐 |
| P4 | 全链路起新版（Gin + pgx），243 测试 + 浏览器验证 | 切换前的最后一道闸 |
| P5 | SQLite → PG 的数据搬运脚本 + 行数核对（5 报告 / 2 密钥 / 全部分享链接） | 切换当天不能靠猜 |
| P6 | 切到 hpcore（frpc 隧道 8788）。**hcyj 的 Caddy 需要改**：加 `teleport.hcyj.xyz` site block + 证书，并保留原有 `handle /yeciorez/teleport*`（见 `2026-10-16-teleport-own-hostname.md`）。两个入口都必须在同一窗口验证 | 用户要的最终形态：hcyj 只转发；且新域名不再挤在别人的路径下 |

**保持冻结的线上环境**：重写期间**不要**改 hcyj 的 Caddy、不要升级本机 Postgres、
**绝对不要轮换 `SESSION_SECRET`**（它会让所有派生密钥失效）。

## 未做 / 需要人类

- **P2 测试策略已裁决**（`.ai/decisions/2026-10-16-p2-test-strategy.md`）：B —— 每测一个 schema，
  从内嵌迁移现建现拆；闸门两层（`check.sh` 可跳过 DB，`test-pg.sh` 强制）。
- **P2.3 做法已裁决**（`.ai/decisions/2026-10-16-p2-store-port-technique.md`）：**B 一次性大爆炸**
  （A「接口 + 双实现交叉验证」是 AI 的建议，用户选了 B）。B 必须做的 5 项缓解写在文件里，
  其中最关键的一条是：**测试断言冻结、不许为了变绿而重写**。
- **域名已裁决**（`.ai/decisions/2026-10-16-teleport-own-hostname.md`）：teleport 将拥有
  `https://teleport.hcyj.xyz/`（**根路径**，`ROUTE_PREFIX=""`），**P6 与切换一起做**，
  且**旧前缀 `api.hcyj.xyz/yeciorez/teleport` 必须永久保留**（`expires_at=0` 的永久链接）。
  已核实 agent 密钥与域名无关（`DeriveToken` 不吃 URL），不会失效凭据。
  影响面：`ROUTE_PREFIX`、`PUBLIC_BASE_URL`、`FRONTEND_ORIGINS`、`CookiePath`、Vite `base`、
  `/ai.md` 与 `/llms.txt` 里的地址、`format:"html"` 的同源 iframe 判定。
- **更正交接文档里的一处计数**：需要显式加锁的「计数—插入」实际上有 **3 处**，不是文档写的
  「待批准上限 / 密钥上限 / 分享链接计数」：① `CreateApplication` 的 `KEY_MAX_PENDING`，
  ② `ClaimApplication` 与 `CreateManualKey` 共用的 `KEY_MAX_ACTIVE`，③ `CreateRenewal` 的
  「每把密钥至多一个待批续期」。**分享链接没有任何数量上限**（`RecordView` 只是
  `view_count = view_count + 1`，在 PG 里本来就是原子的），照字面找会白找。
- 本仓库在 GitHub 上还不存在（本地目录 `/data/dev/teleport`，remote 名为 `teleport`）。
- 线上仍是旧版（hcyj，SQLite + net/http）。**但旧版目前已停掉（disabled+inactive），公网入口 502** —— 用户裁决「等重写版就绪再切」。
- 与 clip 共享的基础设施改动要**同时想两边**（hcyj 的 Caddyfile 同时服务两个站点）。

## 长期约束（改动前必须知道）

- 冻结清单（见决策记录第一张表）**不可改**；改之前必须先改决策记录。
  **路由前缀那一项已于 2026-10-16 修正**：不是「前缀不可改」，而是「**旧前缀必须一直有效**」
  —— 新增 `teleport.hcyj.xyz` 根路径入口，旧前缀永久保留（见其专有决策记录）。
- **归属是安全边界**：非归属者的读/改/删一律 404 而不是 403，且判定在任何写入之前。
- **密钥是派生的，不是暂存的**；服务端只存 `sha256`。
- **一次性凭据只走 URL fragment**，页面无 JS 时惰性，`HEAD` 领取必须 405。
- **封套与命名**：`{ok,data,requestId}`；请求 camelCase、响应 snake_case；未知字段 400。
- **时间一律 epoch 毫秒**；`expires_at=0` = 永不过期，且「永不过期」只能由人类显式给出。
- **限流桶键必须用 `httpx.ClientIP`**，绝不用任何 RealIP 式实现。
- **markdown 报告永不渲染原始 HTML**；`format:"html"` 的原样渲染是**唯一的例外**，有双向绊线测试。
- 大陆可达性是硬约束；验证必须用大陆观测点。
- **Postgres 实例是与 clip / registry / serenique 共享的**（`max_connections = 100` 是所有人
  共用的），不是 teleport 的预算。任何夹具/脚本/压测都不得按「整台实例都是我的」来开连接：
  一个循环里开连接池会瞬间吃掉 100 个槽位，把别人的服务弄成「连不上」而不是「测试失败」。
  见 pitfall `2026-10-16-test-harness-exhausted-shared-postgres-connections.md`。
