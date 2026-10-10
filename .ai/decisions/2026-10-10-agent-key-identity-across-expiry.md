# 密钥过期后的身份连续性：让「过期」不等于「失去归属」

**Status:** ✅ ACCEPTED · **Date:** 2026-10-10 · **Deciders:** 用户 · **Supersedes:** none

<!-- Status machine: 💭 PROPOSAL → ✅ ACCEPTED → 🪦 REJECTED.
     Flip the word in the Status line above; never delete the file and never rewrite the reasoning. -->

## Context

### 已被误信的前提（先纠正）

**「我们没有续签功能，只有重新签」——不成立。** 续期功能已实现，且是
`2026-10-10-agent-self-service-keys.md` 中用户第 2 条绑定裁决（「续期也要用户批准」）的直接产物：

| 事实 | 证据 |
| --- | --- |
| 续期端点存在 | `POST /api/agent-keys/renewals`（Bearer）、`GET /api/agent-keys/renewals/{id}`（`X-Teleport-Claim`） |
| 续期是**同一把密钥** | `DecideRenewal` 执行 `UPDATE agent_keys SET expires_at = ? WHERE id = ?` —— **不新建行、不变 `key_id`** |
| 因此归属天然连续 | `reports.owner_key_id` 指向 `agent_keys.id`，`key_id` 没变 → **读/撤销旧报告的权限一直在** |
| 续期只延长不缩短 | `granted = max(当前到期, now) + 批准小时数`；`0` 保持 `0` |
| 「永不过期」受支持 | `expires_at = 0` 即无界；面板批准时**显式**传 `0` 才生效（补正 7 收紧了继承） |

**结论：续期不是「重新签」，它就是同一个身份延期。归属不会丢** —— 只要续期发生过。

### 真正的洞：续期必须在过期**之前**发起

```
CreateRenewal  ← requireAgent  ← Resolve: WHERE token_hash=? AND revoked_at=0
                                        AND (expires_at = 0 OR expires_at > ?)
```

密钥一过期，认证即 401 → **再也无法调用 `POST /api/agent-keys/renewals`** →
该密钥永久死亡 → agent 只能**重新申请** → **新的 `key_id`** →
它对自己此前发布的报告**全部 404**（归属模型按 `key_id` 比对）。

于是存在一条**不对称**，它使文档承诺的意图不可达：

| 层 | 对「已过期」的态度 |
| --- | --- |
| `DecideRenewal`（批准侧） | **明确支持**。注释原文：「An expired key can still be extended here — **that is the point**, since an agent that noticed its key about to lapse is exactly who files a renewal」 |
| `CreateRenewal`（发起侧，经 auth） | **拒绝**（401） |

**「注意到密钥刚过期」的 agent 正是注释想服务的人 —— 而它恰恰是唯一被挡住的人。**
批准侧开放、发起侧关闭，两者之间没有任何可达路径：过期前没提交续期的密钥，
只能靠**人类在面板上 PATCH** 才能被救活。

补充事实：`revoked_at` 是另一条路（人类主动撤销），与本议题无关 —— 撤销后本就应当死亡，
`DecideRenewal` 也拒绝复活它（`revoked_at != 0 → ErrKeyMissing`）。

### 用户的实际诉求

> 「密钥过期了重新签，你怎么知道它是谁呢？你就是要给我解决这个问题，**再加一个会过期的时间机制**。」

即：**过期机制要保留，但身份不能因过期而断。**

### 前提约束

- **不做账号**（用户已明确否决账号形式）。主体是 `agent_keys`，凭证是 bearer token。
- **身份的可信根是人类的「批准」动作**，不是任何可自证的密钥属性 —— 密钥只是 bearer
  token，不携带可被服务端独立验证的「我是谁」。因此身份的延续**只能由人类裁决**。
- 不破坏既有硬约定：迁移需兼容旧行；`owner_key_id = ''`（root/迁移前）契约不变。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| **A. 过期宽限续签**：未撤销的**已过期**密钥仍可认证，但**仅**能调用 `POST /api/agent-keys/renewals`，宽限期 `KEY_RENEWAL_GRACE`（如 90 天） | 补齐既有意图（批准侧本来就支持）；`key_id` 不变 → 归属零成本连续；无新表、无迁移；仍要人类批准，密钥不会被自动复活 | auth 层要开一个例外（须严格限定为单一端点）；被泄漏的过期密钥在最坏情况下只能**制造审批队列噪声**（每钥最多 1 条 pending），不能读/发/撤；宽限期后仍会死亡 | easy |
| **B. 人工「延续」**：批准新申请时可指定它是某旧密钥的延续；归属解析沿延续链回溯 | 覆盖 A 够不到的长尾（撤销后重建、宽限期已过、想要密钥轮换、密钥误删）；纯人类裁决，无密码学新机制 | 需要一列（如 `continues_key_id`）+ 归属判定改为「链上可达」；**人类误操作可把他人报告交出去**（但那是自己的面板）；链深需设上限 | medium |
| **C. 不做（现状）**：重新签 = 新身份，旧报告只有面板能管 | 零成本；面板本就全权，人类侧无实际阻塞 | agent 永久失去自己历史报告的可编程访问（`GET /api/reports/{id}` 全 404）；「谁发的」仍在库里但 agent 侧断裂 | easy |
| **D. 不设过期**：`expires_at = 0` | 已支持；个人长期 agent 最省事；彻底消灭「忘记续期」 | 失去「泄漏的密钥终会失效」这一性质；不适合临时/共享 agent | easy |
| **E. 引入 owner（身份）记录**：`reports.owner_key_id` 改指稳定 owner，密钥挂在 owner 下 | 概念最干净，任意轮换/撤销/重建都不丢归属；是「身份」的真正模型 | 需要**迁移全部已有报告** + 重写归属模型 + 校验路径；等于引入一个（无凭证的）账号实体，与用户「不要账号」的直觉相冲突，收益在当前规模下不成立 | hard |

## Decision

**选 A + 过期可见性 + agent 侧报告列表。B 暂不做，E 不做。**

**用户的绑定裁决**（本节即契约）：

| # | 裁决 |
| --- | --- |
| K1 | **做宽限续签，期限 90 天**（`KEY_RENEWAL_GRACE`，默认 `2160h`） |
| K2 | 宽限期内过期密钥**仅**能调 `POST /api/agent-keys/renewals`，其余一律 401 |
| K3 | **已撤销的密钥不享受宽限**；不批准就保持死亡 |
| K4 | 不引入账号；主体仍是 `agent_keys`，密钥即身份 |

落地规则：

- **默认严格，例外选择性开启。** `httpx.RequireAgent` 拒绝过期密钥（与未知密钥同码 401），
  新增 `httpx.RequireAgentAllowExpired` 只在续期那一条路由上放行。
  方向很重要：漏维护的后果是「续期用不了」（可见的 401），而不是「过期密钥全权可用」
  （静默的越权）。这与 `isContentHashed` 的方向选择是同一条原则 —— **默认落在安全一侧**。
- **撤销优先于宽限**：解析 SQL 始终带 `revoked_at = 0`。
- **拒绝时不得泄漏**：被拒请求不带 `X-Teleport-*` 头，否则等于告诉攻击者
  「这个猜测是真密钥，只是过期了」，会毁掉同码 401 的性质。
- **续期不换 `key_id`**，归属因此连续；`expires_at = 0`（无界）永不被降级。
- 过期可见性：每个 agent 响应带 `X-Teleport-Key-Expires-At`（Unix 秒，不出现 = 无界）
  与 `X-Teleport-Key-Expired: true`（仅在宽限期内）。
- 补 `GET /api/reports`（按 `owner_key_id` 过滤，root 见全部），
  因为「拥有」却「无法枚举」不叫拥有：读别人的 id 返回 404，忘了 id 就等于永久丢失。
- 面板 `GET /api/admin/reports` 补 `owner_key_id` + `owner_name`
  （`ListReports` 本来就查出了 `owner_key_id`，是组装响应时丢掉的）。

## Consequences

- **变容易**：agent 忘记续期不再等于丧失历史报告；「续期」成为它唯一需要记住的事。
- **必须保持为真**：
  - 宽限期内的过期密钥**只能**触达续期申请端点，读/写/撤销一律 401；
  - 续期仍是**申请**，人类不批准则密钥保持死亡；
  - `revoked_at != 0` 的密钥**不享受**宽限；
  - 归属判定不得因本机制而放宽 —— `owner_key_id` 与 `key_id` 的比对语义不变；
  - 被拒请求不得携带任何 `X-Teleport-Key-*` 头。
- **代价**：auth 多一条分支与一条路由例外（各有专门测试）；
  配对包 `.ai`、`/ai.md`、`/llms.txt` 说明新语义。

### 已知残留（不阻塞，记录在案）

- **限流的缺失**：续期端点是**已认证**的，没有独立限流。噪声有天然上限 ——
  `CreateRenewal` 保证每把密钥最多 1 条 pending，且只有「过期但未撤销且仍在宽限期内」
  的密钥能提交。但若历史密钥数量很大，审批队列理论上可被灌入与密钥数量同阶的条目。
  缓解手段：面板批量拒绝（待办）。**未加全局上限，因为当前规模下不值得多一个可调旋钮。**
- **前端不知道宽限期长度**：`KEY_RENEWAL_GRACE` 没有对面板暴露（避免为「显示一个数字」
  新开信息面）。面板对已过期密钥只标注「仍可申请续期」，由服务端裁决是否真的受理。
- **`Config` 手工构造处需要同步**：`api` 测试的 fixture 曾因没设 `KeyRenewalGrace`
  而默认为 0（Go 零值），把宽限关掉。已补，但这是「新配置项默认值只在 `Load()` 里」
  这一结构的固有脆弱点 —— 与 `.engram/BOOTSTRAP.md` 无关，属于 config 包的设计债。

### 配套（无论选哪个都值得做，且彼此独立）

1. **过期可见性**：agent 现在只能通过 `GET /api/agent-keys/me` 主动查询自己的 `expires_at`。
   应让每个 agent 响应都携带到期信息（如响应封套内的 `key` 块或 `X-Teleport-Key-Expires-At` 头），
   并在 `/ai.md`、`/llms.txt` 中要求 agent **提前**续期。这是**最便宜**的一条，
   它把「身份断裂」的概率从「取决于 agent 是否记得轮询 `/me`」降到接近零。
2. **agent 侧报告列表**（与身份问题同源但独立）：`GET /api/reports` 目前不存在，
   agent 必须自己记住每个 `report_id`，否则连**自己**发过的报告都找不回。
   归属隔离要求这个端点按 `principal.KeyID` 过滤。
3. **面板显示发布者**：`ListReports` 已 select `owner_key_id`，但 `handleListReports`
   组装响应时丢弃了它 —— 「不知道谁发的」是**一个字段的缺失**，不是设计缺陷。

## Revisit when

- 出现**密钥轮换**的真实需求（主动换钥而非被动过期）→ 届时 B 升级为主线，或重启 E。
- 具名密钥数量增长到宽限期例外带来的审批队列噪声可观测（比如噪声占队列 > 30%）。
- 有 agent 需要**跨密钥**访问同一批报告（多 agent 协作同一个主题）→ 那是 E 的信号。
