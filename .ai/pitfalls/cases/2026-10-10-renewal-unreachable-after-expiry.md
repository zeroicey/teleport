# 「续期必须在过期前发起」——批准侧能收、发起侧却拒收

**Status:** 🩹 已修（2026-10-10） · **Related:** `decisions/2026-10-10-agent-key-identity-across-expiry.md`

## 症状

一把 agent 密钥过期后，它就**永久死亡**，而且**没有任何错误会告诉任何人**：

- agent 调用任何接口都是 `401 Invalid agent credentials`；
- 重新申请能得到一把新密钥，但新密钥 `key.id` 不同 →
  **它读不到自己此前发布的任何报告**（归属按 `key_id` 比对，非归属一律 404）；
- 面板上这些报告依然在，只是"没有主人"了。

用户的原话是：「密钥过期了重新签，你怎么知道它是谁呢？」——这就是那个洞。

## 根因：同一件事，两个层给出相反的答案

```
CreateRenewal  ← requireAgent ← Resolve: ... AND (expires_at = 0 OR expires_at > ?)
DecideRenewal  ← 注释原文：An expired key can still be extended here —
                 that is the point, since an agent that noticed its key
                 about to lapse is exactly who files a renewal
```

**批准侧明确写了「过期密钥仍可被延长 —— 那正是重点」，发起侧却在鉴权层把过期密钥拒之门外。**

注释想服务的人（「注意到密钥刚过期」的 agent）恰恰是唯一到不了批准侧的人。
这不是缺功能，是**既有意图不可达**：一条只有一半接通的路。

`DecideRenewal` 确实支持延长已过期密钥（`granted = max(当前到期, now) + 批准时长`，
过期时自然退化为 `now + 时长`），`CreateRenewal` 也不检查过期 —— 唯一的闸门是 auth。
所以修法不是加功能，而是**把那半条路接通**。

## 为什么"重新申请"不能代替"续期"

因为身份是 `key.id`，不是名字：

| | 续期 | 重新申请 |
| --- | --- | --- |
| `key.id` | **不变** | 变 |
| 旧报告归属 | **保持** | **丢失**（全 404） |
| 人类操作 | 批准一次续期 | 批准一次新申请 |
| 可追溯性 | 同一行，`last_used_at` / `request_count` 连续 | 历史断裂 |

名字（`label` / `approved_name`）**不能**当作身份：任何申请人都能自称
`dsh-lead-auditor`，服务端无从分辨。所以身份只能靠 `key.id` 的连续性，
而连续性只能靠「不换行」——也就是续期。

## 修法

**方向：默认严格，例外选择性开启。**

```go
func RequireAgent(cfg, resolver) Middleware            // 拒绝过期密钥，与未知密钥同码 401
func RequireAgentAllowExpired(cfg, resolver) Middleware // 只在续期那一条路由上放行
```

两条实现共用一个未导出的 `requireAgent(cfg, resolver, allowExpired)`，
所以常量时间比较、双路鉴权、`Root=false` 兜底那些细节只有一份。

- store：`ResolveAgentKeyWithinGrace(hash, now, graceMS) (key, expired, err)`，
  `ResolveAgentKey` 退化为 `graceMS = 0` 的调用，避免两条 SQL 漂移。
  比较写成 `expires_at > now - grace` 而不是 `expires_at + grace > now`，**加不溢出**。
- `revoked_at = 0` 始终保留：**撤销优先于宽限**。
- 被拒请求**不带** `X-Teleport-Key-*` 头 —— 否则等于确认"这个猜测是真密钥，只是过期了"，
  会毁掉「未知 / 已撤销 / 已过期一律同码 401」这条契约。

## Guard（每条都反证过）

| 测试 | 断言 | 反证方式与结果 |
| --- | --- | --- |
| `TestExpiredKeyMayFileRenewalAndNothingElse` | 过期密钥**只**能调续期；其余 5 个端点全 401 且不带头 | 把共享中间件换成 `AllowExpired`（模拟"忘了保持默认严格"）→ 5 个子测试全红，报 `status = 200/201, want 401` |
| `TestRevokedKeyIsNotRescuedByTheGraceWindow` | 撤销密钥即使在宽限期内也 401；批准侧也拒绝复活 | 从解析 SQL 删掉 `revoked_at = 0` → 红，报 `a revoked key inside the grace window filed a renewal: status = 201` |
| `TestGraceWindowCanBeDisabled` | `KEY_RENEWAL_GRACE=0` 恢复严格 | 见下（fixture 漏设即为红，见"踩到的坑"） |
| `TestExpiredBeyondGraceIsIndistinguishableFromUnknown` | 超窗后与未知密钥**响应形状完全一致**（去掉 `requestId` 后逐字段相同） | — |
| `TestResolveAgentKeyWithinGrace` | 7 个边界：恰好到期、恰好宽限结束、`grace=0`、`expires_at=0` 永不过期 | — |
| `TestApprovedRenewalRestoresAccessAndKeepsTheOldReports` | 续期后**同 token 同 key id**，且**过期前发布的报告仍可读** | — |
| `TestListOwnReportsIsScopedToTheCaller` | A 看不到 B 的，也看不到 root 的（`owner_key_id=''`） | — |

## 踩到的坑（比 bug 本身更值得记）

### 1. 测试夹具漏设新配置项 = 悄悄关掉了被测特性

`api` 的 `testServerWith` **手工构造** `config.Config`（因为 `Load()` 需要环境变量），
所以新加的 `KeyRenewalGrace` 是 Go 零值 `0` —— 宽限窗口被关掉，
而 `Load()` 的默认值是 90 天。测试一开始全红，看起来像实现错了。

**教训**：配置项加默认值时，**「默认值住在哪里」必须只有一个答案**。
现在住在 `Load()`，而手工构造 `Config` 的调用方（测试、未来的嵌入方）不会自动继承。
夹具注释里原本就写着"Production defaults ... set explicitly because a hand-built Config
skips validation" —— 这句话预言了这个坑，但没人把它变成机制。

### 2. 断言"两个响应字节相同"是错的

`TestExpiredBeyondGraceIsIndistinguishableFromUnknown` 最初比较整个 body，
必然失败：**每个响应都带唯一的 `requestId`**。这不是被测行为的问题，是断言写错了对象。
改为「去掉 `requestId` 后逐字段相同」的 `sameEnvelope` 辅助函数。
**"不可区分"要断言的是语义形状，不是字节流** —— 后者会把相关性 id 误判成信息泄漏。

### 3. `time.ParseDuration` 没有 `d` 单位

`KEY_RENEWAL_GRACE=90d` 直接启动失败，而"90 天"是这个变量**最自然的写法**。
现在报错信息里直接给出 `write 90 days as 2160h`。
**拒绝一个值的时候，要顺便告诉对方该写什么** —— 否则每个运维都要去查一次 Go 的 duration 语法。

### 4. `TestMethodNotAllowed` 把「只有 POST」的例子押在 `/api/reports` 上

新增 `GET /api/reports` 后这个测试立刻变红 —— 这是**好事**：它证明测试钉的是真路由。
已把例子换成 `POST /api/agent-keys/applications`，并顺手发现 Go 的 ServeMux 在这里
返回的是 **404 而不是 405**（`/api/` 的 catch-all 对任意方法都匹配，先于方法判定生效），
所以断言也一并改对了 —— 原来那条「不是 200/201」的松断言，实际上在测一个错误的前提。

## 一句话教训

**「过期」不应该等于「失去归属」。** 当同一件事的两个层给出相反答案时，
先别急着加功能 —— 先看那条路是不是只接通了一半。
另外：**「拥有」却「无法枚举」不叫拥有**（`GET /api/reports` 缺了同样久）。
