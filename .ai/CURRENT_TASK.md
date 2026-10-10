# Current task

**Status:** ✅ 已完成并上线 · **Updated:** 2026-10-10

## 2026-10-10 追加②：审计发现的四条已**全部修复**（未部署）

`pnpm run check` + `pnpm run backend:vet` + `go test ./... -race -count=1` 全绿。

| # | 问题 | 修复 | 守卫 |
| --- | --- | --- | --- |
| ① | `/assets/share.js` 被下发 `immutable`（应为 `no-cache`），回访浏览器最长一年用旧副本 | `spa.go` 新增 `isContentHashed()` + `stableAssetFiles` 例外表，按**名字**而非目录判定 | `TestServesStableAssetUncached` + `TestStableAssetListMatchesBuild`；已**反证**（改回旧行为即红） |
| ② | `/llms.txt` 教 agent「向用户索要密钥」 | `aidoc/html.go` 的 `LLMSTxt()` 改为指向自助申请端点 | `TestLLMSTxtDoesNotSendAgentsToTheUserForAKey` + 端点断言 |
| ③ | `views.ContentSecurityPolicy` 死代码，真实 CSP 内联在 `router.go` | `router.go` 改为引用该常量（补 `views` import） | `router_test.go` 断言响应头**全串等于**常量 |
| ④ | 仓库根 `public/` 是 CF 拓扑遗留（gitignore、无引用） | 已删除；其 `_headers` 里那条**正确的**缓存规则注释已抄进踩坑记录 | `README.md`「缓存」节改写为按名字分类 |

> ① 的关键线索在 `public/_headers` 里：CF 时代**明确写过** `/assets/share.js` 必须
> `no-cache` 并且警告「不要 optimize 成 immutable」。迁移到 Go 托管时被重新实现成
> 目录级判断，规则丢了、陷阱重踩。已记
> `pitfalls/cases/2026-10-10-share-js-cached-immutable-for-a-year.md`。
>
> **待办**：① 的修复只在源码里，**线上仍是旧二进制**（`1c5ab91`）。需要
> `pnpm run build:release` + 上传重启后才生效 —— 部署前先问人类。

## 2026-10-10 追加①：一次外部审计 + 发布了一份项目全景报告

应项目所有者要求，从**线上平台自身**审计并发布了一份《Teleport 项目全景报告》
（架构 / 功能 / 运作与使用）。发布走的是**自助密钥全流程**（申请 → 人类面板批准 →
轮询领取 → `POST /api/reports`），即对 `decisions/2026-10-10-agent-self-service-keys.md`
的一次真实端到端演练。

- 报告分享链接（`expires_at = 0`，永不过期）：`/s/Du01mf3JsO9utq69xtKcBw`（**v2，含修复**；
  v1 `/s/jOrlqf4gcF7lDUDrkUjxmg` 已撤销 —— 它的附录 B 把四条写成「未修」，与代码矛盾）
- 发布用密钥：`dsh-lead-auditor`（30 天，非 root）
- **本轮发现 1 处线上缺陷 + 3 处轻微漂移**，详见报告附录 B。**四条均已在同一轮修复**，
  修复清单与守卫见上面的「追加②」；线上二进制尚未更新。

## Goal

**Agent 自助申请密钥**：agent 自己申请 → 人类在网页批准 → 颁发**有时效 / 可续期 /
可不过期**的密钥，配一个统一管理面板。用户四条绑定裁决（见
`decisions/2026-10-10-agent-self-service-keys.md`）：只存哈希（实为**派生**，明文从不落盘）、
续期也要人类批准、申请入口完全开放 + 限流、作用域为**归属模型**。

## 已上线

线上 `1c5ab91`（2026-10-10 03:52），`api.hcyj.xyz/yeciorez/teleport`，迁移 `0002` 已应用。
三个提交都已推送，工作区干净。

- `8bdc231` 特性 + 修两个生产可利用的认证绕过
- `063c5e6` 修独立审计发现的 8 处缺陷
- `1c5ab91` 修限流桶键塌缩（部署验收时抓到，审计未覆盖）

**大陆观测点验证**：hcyj 本机（阿里云广州 8.148.233.134）、`bjbuwe`（广东电信
121.9.113.26）、`koma`（腾讯云 124.221.144.97）—— 旧页面/分享页/`/ai.md` 全 200、
`/api/nope` 仍 JSON 404、伪造 `Cf-Access-…` 头 401、三个 IP **各自分桶**且
`requester_ip` 记录真实 IP。

## 关键设计（勿改）

- **密钥是派生的，不是暂存的**：`token = base64url(HMAC(K_derive, "teleport/agent-key/v1|appID|claimSecret"))`，
  `K_derive = HMAC(SessionSecret, "teleport/derive/agent-key/v1")`。服务端只存 `sha256(token)`。
  因为推导需要 `claimSecret`，**密钥在「领取」时才存在** —— 批准只停放 `approved_*` 元数据。
  **一把密钥的寿命从 agent 拿到它开始，不是从人类点批准开始。**
- **归属即权限**：`reports.owner_key_id`；非归属的读/撤销一律 **404**（不是 403）。
  `AGENT_SECRET_KEY` 降级为 root/应急，其报告 `owner_key_id=''`。
- **续期只延长、绝不缩短**：`max(当前到期, now) + 批准时长`；`expires_at=0`（无界）保持 0。
- **「永不过期」只能由人类显式给出**：批准端字段缺省 → 24h；显式 `0` 才是永久。
- **限流桶键必须用 `httpx.ClientIP`**，绝不用 `RealIP`。
- 时间一律 epoch **毫秒**；`expires_at=0` 表示无界（**不是"1970 年就过期了"**）。
- 请求体 **camelCase**，响应字段 **snake_case**。

## 上线后仍未处理（建议按序）

1. **轮换 admin 口令**（仍是部署时的随机值）。
2. **面板缺批量拒绝**：`KEY_MAX_PENDING=50` 是唯一兜底，50 个申请即可堵住公开入口最长 24h。
3. **`POST /api/admin/login` 无限流**：PBKDF2 600000 轮在 2 vCPU 上是 CPU 耗尽面。
4. 具名密钥**读不到迁移前 root 发布的报告**（`owner_key_id=''`）：契约默认行为，需向使用者交代。
5. `CleanupExpired` 未排期；无删除报告 API；无 SQLite 自动备份。
6. 已判定**接受**的：`requestId` 反射客户端头（有意，已净化+限长）、
   `TrustCFAccess` 用包级变量而非 `atomic.Bool`（无运行期写入路径，改 atomic 反而暗示可变）、
   root 密钥长度时序（实测 z=+0.30 低于噪声）。

## Pitfall reminders（本轮新增 3 条，都值得复读）

- **`2026-10-10-trusted-request-header-grants-authority.md`** — 同一类错误出现两次：
  `Cf-Access-…` 头 = 无条件管理员；`RealIP` 被当安全边界。含**元教训**：
  写安全**理由**前先打开那个组件的配置文件（我曾把 Caddy 的**覆盖**写成"追加"）。
- **`2026-10-10-rate-limit-collapsed-to-one-bucket.md`** — 模拟外部组件时用了它**不会产生**的
  格式（Caddy `{remote}` 是 `host:port`，测试却用裸 IP）。反直觉后果：**跳过不可解析项**
  一旦丢掉真实条目，遍历会继续向左并**采信攻击者伪造的值** → 同一 bug 同时表现为
  过度限制**与**可绕过。另含：`sed -i` 换 inode 会**静默**弄断 docker 单文件 bind mount。
- **`2026-10-10-leak-assertion-matches-own-fixture.md`** — 「输出不含 X」的断言被自己的
  测试夹具打红；更危险的是**假绿**。
- **`2026-10-10-schema-guard-hardcoded-single-migration.md`** — 用硬编码数量表达"全部"的守卫。
- **太弱的并发测试会掩盖竞态**：`maxPending` 太小 → 多数 goroutine 在 COUNT 就退出，
  从不尝试写升级 → `BUSY_SNAPSHOT` 测不出来。要构造"全部走到写路径"的配置。
- **契约示例字段名属于规范**：响应示例写成 camelCase、实现返回 snake_case → 文档让 agent 静默失败。
- 其余：`public-doc-disclosed-secret-file-path`、`spa-fallback-swallows-api-404`、
  `env-file-dollar-expansion`、`goldmark-setrenderer-drops-extensions`。
- **`.ai/` 是共享可写面**：写前先读。

## Next action

无阻塞。若要继续：轮换 admin 口令 → 给登录端点加限流 → 面板加批量拒绝。
