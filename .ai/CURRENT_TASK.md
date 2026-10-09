# Current task

**Status:** 🟡 in progress · **Updated:** 2026-10-10

## Goal

**Agent 自助申请密钥**：agent 自己申请 → 人类在网页批准 → 颁发**有时效 / 可续期 /
可不过期**的密钥，配一个统一管理面板。用户要求「写代码测试审计部署」，并已经给出
四条绑定裁决（见 `decisions/2026-10-10-agent-self-service-keys.md`）：

1. 密钥存储：**只存哈希，仅在批准时显示一次**
2. 续期：**也要人类批准**
3. 申请入口：**完全开放 + 限流**
4. 作用域：**归属模型** —— agent 只能读/撤销**它自己发布**的报告（不是权限位）

## Checklist

- [x] 冻结契约：`.ai/decisions/2026-10-10-agent-self-service-keys.md`（含 4 条用户裁决 + 端点/数据/鉴权模型）
- [x] **T1** `agentkey` 派生包 + 迁移 `0002_agent_keys.sql` + `store/keys.go` + `config` 五个限额 + 测试
- [x] 迁移在**真实生产库副本**上验证（1 报告 / 2 分享链接全存活，旧报告 `owner_key_id=''`）
- [x] **T2** `httpx` 双路鉴权 + `Principal` 注入（两条路都走完，失败形态逐字节一致）
- [x] **T3** `api` 全部端点 + 归属校验（非归属 404）+ 限流 + 路由挂载（20 个测试）
- [x] **T4** Vue 面板 `KeyManagementView.vue`（三区块 + 4 弹窗，headless 实测过泄漏面）
- [x] **T5** `guide.md`（第 2 节整节重写为自助申请）+ README
- [x] 修掉 `check-schema-sync.mjs` 把迁移数量写死的缺陷（守卫曾静默少守 0002）
- [ ] 修 guide.md 响应字段 camelCase → snake_case（**文档曾与实现不符**，见陷阱）
- [ ] 修 🔴 限流绕过：新增 `httpx.ClientIP`，限流从 `RealIP` 切过去
- [ ] **T6** 独立对抗性审计（10 项 + 主动找洞）
- [ ] 全量验收（`pnpm run check` + vet + 真实库迁移 + 面板构建 + 大陆观测点）
- [ ] 部署 hcyj + 提交 + 推送

## Code state

`backend/internal/agentkey/`（派生）、`domain/keys.go`、`store/keys.go` + `migrations/0002_agent_keys.sql`、
`config.go`（`KEY_APPLY_PER_HOUR` / `KEY_APPLICATION_TTL` / `KEY_CLAIM_WINDOW` / `KEY_MAX_PENDING` / `KEY_MAX_ACTIVE`）、
`httpx/auth.go`（`KeyResolver` + `RequireAgent(cfg, resolver)`）、`api/keys.go` + `api/ratelimit.go`、
`web/src/views/KeyManagementView.vue`、`aidoc/guide.md`。

**未部署**：线上仍是 `9e1b2c0a…`（`version` = `868309d`），只有迁移 `0001`。0002 尚未上线。

## Blockers

- none（三处修复在途：guide.md 字段名 / `httpx.ClientIP` / api 调用点适配）

## Pitfall reminders for the current branch

- **`.ai/pitfalls/cases/2026-10-10-schema-guard-hardcoded-single-migration.md`** — 用「硬编码
  数量/单个名字」表达「全部」的守卫，加成员后静默少守。新增成员后要**故意破坏一次**验证。
- **契约里的示例字段名属于规范**：契约正文把响应示例写成 camelCase，docs-dev 照抄，
  实现返回 snake_case → 文档让 agent 静默失败。写示例必须区分「请求体」与「响应字段」。
- **`RealIP` 只用于日志，不得用于安全边界**：对公开端点的限流必须用来源可信的取 IP。
- `.ai/pitfalls/cases/2026-10-10-public-doc-disclosed-secret-file-path.md` — 公开文档不许写出密钥存放路径。
- `.ai/pitfalls/cases/spa-fallback-swallows-api-404.md` — 未知 `/api/*` 必须仍是 JSON。
- `.ai/pitfalls/cases/env-file-dollar-expansion.md` — shell source env 会吃掉哈希里的 `$`。
- `.ai/pitfalls/cases/goldmark-setrenderer-drops-extensions.md` — `SetRenderer` 静默丢扩展。
- **`.ai/` 是共享可写面**：写前先读，别覆盖对方的更新。

## Next action

等三处修复落地 → auditor 出报告 → 按报告修 → 全量验收 → 部署（`bin/teleport-linux-amd64`
→ `scp` 到 `teleport.new` → `chmod 700 && chown teleport:teleport` → 校验 sha256 → `mv`
→ `systemctl restart teleport`）→ 从**大陆观测点**（hcyj 本机 + 家宽）验证 → 提交推送。

遗留（与上一轮相同，未动）：admin 口令仍是部署时的随机值，建议轮换。
