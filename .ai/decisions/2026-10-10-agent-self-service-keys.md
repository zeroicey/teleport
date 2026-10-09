# agent 自助申请密钥：申请 → 网页批准 → 颁发可续期密钥

**Status:** ✅ ACCEPTED · **Date:** 2026-10-10 · **Deciders:** 用户 · **Supersedes:** none

## Context

现状：`AGENT_SECRET_KEY` 是**单个**环境变量，全站共用一个凭证，无法撤销、无法过期、
无法区分是哪个 agent，且**任何**持钥者都能读/撤销**任何**报告。

用户要求：agent 向服务端申请 → 用户在网页批准 → 颁发有时效的密钥；过期可续期；
也可设为不限制时间；并有统一的管理面板。

**用户的 4 条绑定裁决**（本节即契约，实现不得偏离）：

| # | 问题 | 裁决 |
|---|---|---|
| 1 | 密钥怎么存 | **只存哈希**，明文仅在颁发那一次出现 |
| 2 | 怎么续期 | **续期也要用户批准**，与首次颁发对称 |
| 3 | 申请入口 | **完全开放 + 限流**（任何 agent 可申请） |
| 4 | 权限范围 | 发报告不限；**读 / 撤销只能操作它自己发布的报告** |

第 4 条是**归属模型**，不是普通权限位：`reports` 要记录"谁发布的"，鉴权时比对归属。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. 服务端生成密钥 + 批准到领取之间**暂存明文** | 实现最直白 | **破了裁决 1**：暂存期间明文落盘，此窗口内的备份/快照即泄漏 | easy |
| B. 让 agent 自己生成密钥、只把哈希发来 | 服务端永不见明文 | **无法约束熵**：agent 可提交 `"1234"` 的哈希，批准后即得弱密钥 | easy |
| C. **服务端派生**：`token = HMAC(K_derive, appID‖claim_secret)` | 熵由服务端保证（256 位）；**零明文落盘**；无需暂存列 | 需要一条派生约定；`SessionSecret` 泄漏则密钥可被重算 | easy |
| D. 保持单个 env 密钥 + 只做面板展示 | 改动最小 | 不满足任何一条裁决 | easy |

## Decision

**选 C。** 并确立以下规则：

- **密钥派生，不落明文。** `claim_secret`（32B 随机，申请时下发给 agent，**只存哈希**）
  与申请单 ID 一起派生：
  `token = base64url(HMAC-SHA256(K_derive, "teleport/agent-key/v1|" + appID + "|" + claimSecret))`
  `K_derive = HMAC-SHA256(SessionSecret, "teleport/derive/agent-key/v1")` —— 用**子密钥**
  避免跨协议复用 `SessionSecret`。
  领取时重算并只存 `sha256(token)`。**任何时刻数据库里都没有明文密钥。**
- **`agent_keys` 只存 `token_hash`（sha256 hex, UNIQUE）**；鉴权时对来钥求哈希再查表，
  用 UNIQUE 索引做一次 O(log n) 查找，**不做逐行常数时间比较**（表大了就是 DoS 面）。
- **归属即权限**：`reports.owner_key_id` 记录发布者；非归属者的读/撤销一律 **404**
  （不暴露"存在但不是你的"，与既有"未知 token 与已吊销都返回 404"一致）。
- **`AGENT_SECRET_KEY` 降级为 root/应急密钥**，保留并拥有全部权限；它发布的报告
  `owner_key_id = ''`，只有 root 与面板能读。**旧的 `AGENT_SECRET_KEY` 与已有数据不破坏。**
- **续期需批准**：agent 用自己密钥发起续期申请 → 用户批准时才落新的 `expires_at`。
- **`expires_at = 0` 表示永不过期**（沿用既有约定）。

## Consequences

- **变容易**：可撤销、可过期、可审计（`last_used_at` / `request_count`）、可识别是哪个 agent；
  权限从"全站共用一把钥匙"收紧为"各管各的报告"。
- **必须保持为真**：`agent_keys.token_hash` 永不出现在任何响应里；
  派生用的 `claim_secret` 一旦领取即失效（申请单转 `claimed`）。
- **代价**：多 3 张表 + 一个 `internal/agentkey` 包；`reports` 加一列（迁移需兼容旧行）。
- **`SessionSecret` 的地位上升**：它现在同时是密钥派生根。泄漏后果从"可伪造会话"
  扩大到"可重算所有 agent 密钥"。已记入 `.ai/pitfalls/`。

## Revisit when

- 需要按密钥分权限（只读密钥 / 限定类目）时：本决策的 `Principal` 已预留扩展位。
- agent 数量增长到需要配额（每密钥每日发布上限）时。

---

# 实现契约（冻结）

> 供并行实现使用。**路径、字段名、状态码都不得自行改动**；有异议先回 Lead。

## 包与写作用域（互不重叠）

| 作用域 | 归属 | 内容 |
|---|---|---|
| `backend/internal/agentkey/`、`backend/internal/store/`、`backend/internal/config/`、`backend/internal/domain/`、`schema.sql` | Lead | 派生、持久化、配置、类型 |
| `backend/internal/httpx/` | auth-dev | Principal 注入、双路鉴权 |
| `backend/internal/api/` | api-dev | 全部新端点、归属校验、路由挂载 |
| `web/src/` | web-dev | 面板：申请审批队列 + 密钥管理 |
| `backend/internal/aidoc/guide.md`、`README.md` | docs-dev | 文档 |

## 数据模型（迁移 `0002_agent_keys.sql`）

```sql
ALTER TABLE reports ADD COLUMN owner_key_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_reports_owner_key_id ON reports (owner_key_id);

CREATE TABLE agent_keys (
  id            TEXT PRIMARY KEY,              -- 高熵 id
  name          TEXT NOT NULL,                 -- 人类可读标签
  token_hash    TEXT NOT NULL UNIQUE,          -- sha256 hex
  token_prefix  TEXT NOT NULL,                 -- 前 8 字符，仅供面板辨识
  created_at    INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL DEFAULT 0,    -- 0 = 永不过期
  revoked_at    INTEGER NOT NULL DEFAULT 0,    -- 0 = 有效
  last_used_at  INTEGER NOT NULL DEFAULT 0,
  request_count INTEGER NOT NULL DEFAULT 0,
  note          TEXT NOT NULL DEFAULT ''       -- 批准备注（人类写的）
);

CREATE TABLE key_applications (
  id             TEXT PRIMARY KEY,
  claim_hash     TEXT NOT NULL,                -- sha256(claim_secret)
  label          TEXT NOT NULL,                -- agent 自述名称
  purpose        TEXT NOT NULL DEFAULT '',
  requested_hours INTEGER NOT NULL DEFAULT 0,  -- 0 = 未指定
  status         TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','approved','rejected','claimed','expired')),
  created_at     INTEGER NOT NULL,
  expires_at     INTEGER NOT NULL,             -- 申请单存活期（未批准则自动失效）
  decided_at     INTEGER NOT NULL DEFAULT 0,
  claim_deadline INTEGER NOT NULL DEFAULT 0,   -- 批准后领取截止
  issued_key_id  TEXT NOT NULL DEFAULT '',
  requester_ip   TEXT NOT NULL DEFAULT '',
  user_agent     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE key_renewals (
  id            TEXT PRIMARY KEY,
  key_id        TEXT NOT NULL REFERENCES agent_keys (id) ON DELETE CASCADE,
  requested_hours INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','approved','rejected')),
  created_at    INTEGER NOT NULL,
  decided_at    INTEGER NOT NULL DEFAULT 0,
  granted_expires_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_key_renewals_key_id ON key_renewals (key_id);
```

`schema.sql`（仓库根）必须与迁移逐字节同步 —— `pnpm run db:check-schema` 会失败否则。

## 鉴权模型

```go
// internal/domain
type Principal struct {
    KeyID string // "" 表示 root
    Name  string
    Root  bool   // 来自 AGENT_SECRET_KEY
}
```

```go
// internal/httpx —— auth-dev 实现
type KeyResolver interface {
    // Resolve 返回密钥记录；找不到/已撤销/已过期 一律返回 ok=false。
    Resolve(ctx context.Context, tokenHash string) (domain.Principal, bool)
}
func RequireAgent(cfg AuthConfig, resolver KeyResolver) Middleware
func PrincipalFrom(ctx context.Context) *domain.Principal
```

`RequireAgent` 必须**两条路都走完**再判定，避免用响应时间区分"root 不匹配"与"表里没有"：
1. `subtle.ConstantTimeCompare(provided, cfg.AgentSecretKey)` → root
2. 否则 `sha256(provided)` 交给 `resolver.Resolve`
3. 都失败 → 401（消息与其他失败一致，不区分原因）

## 端点契约

`{P}` = `cfg.RoutePrefix`（如 `/yeciorez/teleport`）。响应封套不变。

**字段命名约定（本仓既有约定，不是本特性新定）：请求体 camelCase，响应字段 snake_case。**

对照既有端点即可验证：`POST /api/reports` 的请求是 `autoShareHours`，响应是
`created_at` / `updated_at` / `view_count`。下面的响应示例按此写 snake_case。

> ⚠️ 本条是**事后补正**。本文件最初把响应示例写成了 camelCase，导致 docs-dev 依此把
> guide.md 的响应字段全写成 camelCase，而实现（按后续裁定）返回 snake_case —— 文档
> 会让 agent 的 `jq -r '.data.claimSecret'` 拿到 `null` 并静默失败。根因是**契约正文
> 的示例没有区分"请求体"与"响应字段"**。契约里的示例属于规范，写错示例等同于写错规范。

### 公开（限流）

| 方法 | 路径 | 请求 | 成功响应 `data` |
|---|---|---|---|
| `POST` | `{P}/api/agent-keys/applications` | `{label, purpose?, requestedHours?}` | `{id, claim_secret, status:"pending", created_at, expires_at, poll_interval_seconds}` |
| `GET` | `{P}/api/agent-keys/applications/{id}` | 头 `X-Teleport-Claim: <claimSecret>` | `{id, status, label, created_at, expires_at, decided_at?, key?:{id,name,token_prefix,expires_at,expires_in_hours}}` |
| `GET` | `{P}/api/agent-keys/renewals/{id}` | 头 `X-Teleport-Claim: <claimSecret>` | `{id, status, key_id, created_at, decided_at?, granted_expires_at?}` |

- `claim_secret` **只在 `POST` 响应里出现一次**，之后只能凭它轮询；服务端只存其哈希。
- `GET .../applications/{id}`：`status=approved` 且**首次**领取时返回 `key.token`（明文，
  仅此一次），并把申请单置为 `claimed`；再次领取 → **404**（不泄漏"已领取过"）。
  超过 `claim_deadline` → **410**；未批准 → `key` 字段缺席；被拒 → `status:"rejected"`。
- 申请单不存在 / claimSecret 不匹配 → **404**（统一，不区分）。

### agent 自有（`Authorization: Bearer <其密钥>`）

| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| `GET` | `{P}/api/agent-keys/me` | — | `{id, name, tokenPrefix, createdAt, expiresAt, lastUsedAt, requestCount}` |
| `POST` | `{P}/api/agent-keys/renewals` | `{requestedHours?}` | `{id, claim_secret, status:"pending", created_at}`；已有 pending 时返回 **409** |

### 面板（Session）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `{P}/api/admin/key-applications?status=pending` | 列表（默认 `pending`），按 `created_at` 倒序 |
| `POST` | `{P}/api/admin/key-applications/{id}/approve` | 体 `{name?, expiresInHours?, note?}`；`expiresInHours=0` → 永不过期 |
| `POST` | `{P}/api/admin/key-applications/{id}/reject` | 体 `{reason?}` |
| `GET` | `{P}/api/admin/keys` | 全部密钥（**绝不含 `token_hash` 与明文**） |
| `POST` | `{P}/api/admin/keys` | 体 `{name, expiresInHours}`；**手动转交路径**：响应含 `token` 明文一次 |
| `PATCH` | `{P}/api/admin/keys/{id}` | 体 `{name?, expiresInHours?, revoked?}`；改 `expiresInHours` 即**用户直接续期** |
| `GET` | `{P}/api/admin/key-renewals?status=pending` | 续期申请列表 |
| `POST` | `{P}/api/admin/key-renewals/{id}/approve` | 体 `{expiresInHours?}`；缺省则用申请里的 `requestedHours` |
| `POST` | `{P}/api/admin/key-renewals/{id}/reject` | — |

## 归属规则（api-dev 必须逐条实现）

| 端点 | 规则 |
|---|---|
| `POST /api/reports` | 落 `owner_key_id = principal.KeyID`（root 则 `''`） |
| `GET /api/reports/{id}` | `principal.Root` 或 `report.owner_key_id == principal.KeyID`，否则 **404** |
| `POST /api/share/{token}/revoke` | 同上，按该 token 所属报告的归属判定，否则 **404** |
| 全部 `/api/admin/*` | Session 鉴权，不受归属限制（人能管全部） |

## 限流与上限（config，含默认值）

| 环境变量 | 默认 | 含义 |
|---|---|---|
| `KEY_APPLY_PER_HOUR` | `5` | 每 IP 每小时可提交的申请数 |
| `KEY_APPLICATION_TTL` | `24h` | 申请单存活期 |
| `KEY_CLAIM_WINDOW` | `30m` | 批准后的领取窗口 |
| `KEY_MAX_PENDING` | `50` | 待批准申请上限（防灌爆审批队列） |
| `KEY_MAX_ACTIVE` | `100` | 有效密钥上限 |

限流为**进程内**计数（单二进制），重启即清零 —— 已知取舍，够用。

## 非目标（本决策不做）

- 按密钥分权限位（只读 / 限定类目）—— 只做归属模型。
- agent 自助删除自己发布的报告（当前没有删除报告的 API）。
- 邮件/IM 通知（审批只在面板里看）。

## 测试义务

- store：派生确定性、哈希查找、过期/撤销判定、归属过滤、申请状态机、续期状态机、
  并发领取只有一次成功（**必须有并发测试**）。
- api：未鉴权 401、错误密钥 401、跨密钥读别家报告 404、root 可读全部、
  领取二次 404、超窗 410、限流 429、响应里绝不出现 `token_hash`。
- 命名/格式：沿用既有 `httpx.OK` / `httpx.WriteError` 封套与 `*_at` 毫秒约定。

---

## 补正记录（实现期产生，2026-10-10）

冻结契约在实现中被四处修正。按记忆库规则：**翻状态、不重写历史**，故以补正段落追加。
每条都是「实现发现了契约的缺陷」，不是实现跑偏。

### 1. 密钥在**领取**时创建，不在批准时

契约原本假设批准即产生密钥。但推导式（选项 C）需要 `claimSecret` 才能算出 token，
而**批准时服务端只有它的哈希** —— 服务器无法在批准时构造出密钥。

于是 `key_applications` 增加三列，把批准决定**停放**下来：

| 列 | 含义 |
| --- | --- |
| `approved_name` | 人类在批准时指定的密钥名 |
| `approved_hours` | 人类批准的时长 |
| `approved_note` | 备注 |

领取时（`ClaimApplication`）用这三列 + `claimSecret` 生成 `agent_keys` 行。

**由此产生一条对使用者可见的语义，必须写进文档**：一把密钥的**寿命从 agent 领取那一刻
开始算**，而不是从人类点"批准"那一刻。面板上"批准后密钥列表不增加"是正确行为。

### 2. 续期凭据由 `K_derive` 确定性派生，不落 `claim_hash` 列

`key_renewals` 没有 claim secret 列，而续期轮询同样要求 `X-Teleport-Claim`。
实现改为：

```
renewal_claim = base64url(HMAC-SHA256(K_derive, "teleport/agent-key/renewal-claim/v1|" + renewalID + "|" + keyID))
```

服务端可随时重算，故无需存储；缺失或不匹配一律 404。安全性依据：`K_derive` 仅服务端持有，
renewalID / keyID 即使泄漏也推不出该值；且创建续期本身已要求 Bearer 鉴权。

### 3. `KEY_MAX_ACTIVE` 在领取事务内强制

原设计把上限放在调用方预检 —— 那是 check-then-act，N 个并发领取会各自读到低于上限的
计数然后全部插入。现改为 `ClaimApplication(..., maxActive, ...)`，在**同一个已持有写锁的
事务内**计数。

超限返回 `ErrMaxActive` 并**回滚状态翻转**，所以申请单不被消耗：运维腾出名额后，
同一张申请单 + 同一个 claim secret 仍可领取。

### 4. 🔴 限流不能建在 `httpx.RealIP` 上（本特性的安全缺陷）

`POST /api/agent-keys/applications` 是**公开**端点，限流是它唯一的减速手段。但它原本挂了
在 `RealIP` 上，而这个函数的注释自己写着「仅用于日志，绝不用于授权」。

可复现的绕过（已实测确认）：

- `dig api.hcyj.xyz` → `8.148.233.134`（Alibaba 广州，**非 Cloudflare 段**）
- 响应头只有 `via: 1.1 Caddy`，无 `cf-ray`
- 后端只监听 `172.17.0.1:8788`（仅 Caddy 可达）

而 `RealIP` **无条件**信任 `Cf-Connecting-Ip`。因此攻击者只要发
`Cf-Connecting-Ip: <随机值>`，每次请求都是新桶 —— **一个请求头即可完全绕过限流**。
次之，Caddy `reverse_proxy` 是**追加** XFF，而 `RealIP` 取**最左**项，即取攻击者控的值。

修法：新增 `httpx.ClientIP(r)`（**peer 不可信则完全忽略所有请求头**；peer 可信时取 XFF
**最右不可信**项；`Cf-Connecting-Ip` 默认完全不看）。限流必须改用它。

**并且必须承认它的局限**：单机内存限流对换 IP 的分布式攻击者无效。
**真正的兜底是 `KEY_MAX_PENDING`（全局上限，与 IP 无关）。** 限流只是减速带，
文档与注释都不得把它描述成一道墙。

### 5. 文档实跑校正（docs-dev 真执行文档命令后发现）

把文档命令**真的跑一遍**才发现的不一致。教训：文档里的命令属于规范的一部分，
不执行就等于没验证（`jq -r '.data.claimSecret'` 拿 `null` 就是这类静默失败）。

| 项 | 契约原写法（错） | 实际行为 |
| --- | --- | --- |
| 申请单过期 | `200` + `status:"expired"` | **`410 gone`**（领取窗口关闭同为 410，文案不同） |
| 创建类端点 | 未规定 | **`201`**（applications / renewals / admin keys / reports） |
| 续期 claim secret | 「同样只存哈希」 | **完全不落库**：`renewal_id`+`key_id` 由 `K_derive` 当场重算，连哈希都没有 |
| 错误码 | 未列全 | `too_many_pending` / `too_many_active_keys` / `renewal_exists` / `already_decided`；429 = `rate_limited` + `Retry-After` |

另外 `/me` 实际还返回 `revoked_at` / `note` / `root`；`POST renewals` 响应还含
`key_id` / `requested_hours`；`share` 对象含 `created_at`。

**附带修正一条验证配方的坑**（与产品代码无关，但会浪费下一个人半小时）：
`teleport hash-password` 输出的是整行 `ADMIN_PASSWORD_HASH=pbkdf2$…`。
`| tail -1` 直接塞进环境变量会让 admin 登录 **401**（变量名被当成哈希的一部分）。
必须 `| sed 's/^ADMIN_PASSWORD_HASH=//'`。

### 6. 续期是「延长」，不是「重设」

原实现把批准后的到期时间设为 `now + grantedHours`。实测暴露问题：一把还剩 24h 的密钥
申请续期 168h，结果只**净增 144h** —— 因为那 24h 被静默丢掉了。

这与文档自相矛盾：guide.md 明确建议「**提前数天**发起续期，不要卡到期」，
而按原语义，提前续期恰恰是**损失**剩余时间。代码注释甚至写着「an agent that renews
early should gain the full period」——注释的意图与代码的效果相反。

改为**只延长、绝不缩短**：

```
granted = max(当前到期, now) + grantedHours
```

两个永远保持「不过期」的边界（都不会被降级）：

- 密钥本来就是 `expires_at = 0`（0 表示**无界**，不是"已过期"）→ 续期后仍为 `0`；
- 人类批准的时长 ≤ 0（显式授予"永不过期"）→ `0`。

不变式：**续期不能减少一把密钥的授权**。（要缩短只能由人类在面板上用 PATCH 显式操作。）
