# Current task

**Status:** 🟡 已部署，第二轮修复在途 · **Updated:** 2026-10-10

## Goal

**Agent 自助申请密钥**：agent 自己申请 → 人类在网页批准 → 颁发**有时效 / 可续期 /
可不过期**的密钥，配一个统一管理面板。用户要求「写代码测试审计部署」，并给出四条绑定
裁决（见 `decisions/2026-10-10-agent-self-service-keys.md`）：

1. 密钥存储：**只存哈希** —— 实为「派生」而非暂存，明文从不落盘
2. 续期：**也要人类批准**
3. 申请入口：**完全开放 + 限流**
4. 作用域：**归属模型** —— agent 只能读/撤销**它自己发布**的报告

## 已上线（`8bdc231`，2026-10-10 03:12，生产 `api.hcyj.xyz`）

- 迁移 `0002_agent_keys` 已在生产库应用（旧数据 1 报告 + 2 分享链接全存活，`owner_key_id=''`）。
- 两个 🔴 认证绕过**已在生产验证关闭**（伪造 `Cf-Access-…` 头 → 401，此前 200）。
- 大陆观测点验证：hcyj 本机（阿里云广州）+ `bjbuwe`（广东电信 121.9.113.26），
  旧页面/分享页/`/ai.md` 全 200，`/api/nope` 仍 JSON 404，新公开端点 201。
- 已 `git push`（`002af35..8bdc231`）。

## Checklist

- [x] T1 `agentkey` 派生 + 迁移 0002 + `store/keys.go` + config 五限额 + 测试
- [x] T2 `httpx` 双路鉴权 + `Principal`（含 🔴 CF Access 修复、🔴 `ClientIP` 限流修复）
- [x] T3 `api` 全部端点 + 归属 404 + 限流（20 测试）
- [x] T4 Vue 面板 `KeyManagementView.vue`
- [x] T5 `guide.md` 第 2 节重写（响应字段 snake_case）
- [x] T6 独立对抗性审计（报告 `/tmp/audit/REPORT.md`，10 项必测 + 主动找洞）
- [x] 部署 + 提交 + 推送 + 生产验证
- [x] 续期语义改为「只延长、绝不缩短」（原实现提前续期会**吞掉**剩余时间）
- [x] 🟠3 并发申请 500（`_txlock=immediate`，226/360 → 0/360）
- [x] 🟠6 `decided_at` 被领取覆盖（人类决策时间被毁）
- [x] 🟠7 `expiresInHours` 溢出静默写负值（加 100 年上限）
- [x] 🟠9 guide.md「已过期密钥无法续期」与实测矛盾
- [x] 🟠10 我自己写错的 Caddy XFF 理由（「追加」实为「覆盖」）
- [ ] T7 api：🟠5 过期批准码 / 🟠8 批准缺省继承 `requested_hours=0` / 🟡1 `/me` 泄漏 note / 🟡2 label 校验
- [ ] T8 httpx：5xx 日志下沉到 `Fail`（现在裸 `Fail` 的 5xx 会静默；健康探针即一例）
- [ ] 第二轮全量验收 + 部署 + 提交推送

## 未修（已判定接受或留待后续）

- 🟡3 `requestId` 反射客户端头：**有意保留**（跨边缘关联追踪），已加 128 字节上限 + 控制字符拒绝。
- 🟡6 `TrustCFAccess` 用包级变量而非 `atomic.Bool`：**有意保留** —— 无运行期写入路径，
  改成 atomic 会**暗示**支持运行期切换，反而诱导后人去改它。
- 🟡5 root 密钥长度时序：实测 z=+0.30 低于噪声，无可用 oracle。
- **具名密钥读不到迁移前 root 发布的报告**（`owner_key_id=''`）：契约默认行为，
  部署说明需向用户交代一句。
- 面板缺**批量拒绝**：`KEY_MAX_PENDING=50` 是唯一兜底，但 50 个申请即可堵住公开入口最长 24h。
- `POST /api/admin/login` 无限流：PBKDF2 600000 轮在 2 vCPU 上是 CPU 耗尽面。
- admin 口令仍是部署时随机值，建议轮换。

## Blockers

- none

## Pitfall reminders

- **`.ai/pitfalls/cases/2026-10-10-trusted-request-header-grants-authority.md`** — 本轮最重：
  同一类错误出现两次（CF Access 头 = 无条件管理员；`RealIP` 被当安全边界）。含**元教训**：
  写安全理由前先打开那个组件的配置文件，别引用记忆。
- **`.ai/pitfalls/cases/2026-10-10-schema-guard-hardcoded-single-migration.md`** — 用硬编码
  数量表达「全部」的守卫，加成员后静默少守。
- **契约示例字段名属于规范**：响应示例写成 camelCase、实现返回 snake_case → 文档让 agent 静默失败。
- **太弱的并发测试会掩盖竞态**：`maxPending` 太小 → 多数 goroutine 在 COUNT 就退出，
  从不尝试写升级 → BUSY_SNAPSHOT 测不出来。要构造「全部走到写路径」的配置。
- `.ai/pitfalls/cases/2026-10-10-public-doc-disclosed-secret-file-path.md` — 公开文档不许写出密钥存放路径。
- `.ai/pitfalls/cases/spa-fallback-swallows-api-404.md` — 未知 `/api/*` 必须仍是 JSON。
- `.ai/pitfalls/cases/env-file-dollar-expansion.md` — shell source env 会吃掉哈希里的 `$`。
- `.ai/pitfalls/cases/goldmark-setrenderer-drops-extensions.md` — `SetRenderer` 静默丢扩展。
- **`.ai/` 是共享可写面**：写前先读，别覆盖对方的更新。

## Next action

等 T7/T8 落地 → 全量验收（`pnpm run check` + vet + gofmt + 真实二进制端到端）→ 部署
（`scp` 到 `teleport.new` → `chmod 700 && chown teleport:teleport` → 校验 sha256 → `mv`
→ `systemctl restart teleport`）→ 大陆观测点验证 → 提交推送。
