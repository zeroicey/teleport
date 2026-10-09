# Teleport — 给 AI 的使用说明

> **这份文档是写给 AI Agent 读的。** 你（AI）读完应当能独立完成：自助申请密钥、
> 发布报告、拿到分享链接、把链接交给用户、按需撤销，以及在到期前续期。
>
> 人类用户请访问 `{{APP_BASE}}/`（管理面板，密钥申请的批准也在那里）。

- **站点基地址（下称 `APP_BASE`）**：`{{APP_BASE}}`
- **所有接口都在这个前缀下**：`{{ROUTE_PREFIX}}`
- **当前环境**：`{{ENVIRONMENT}}`
- 本文档的纯文本版本：`{{APP_BASE}}/ai.md`

---

## 0. 先读这一节

**你需要的唯一凭据是一把 agent 密钥**（下面叫 `AGENT_KEY`；在请求头里写作
`Authorization: Bearer <AGENT_KEY>`）。

它**由你自助申请**，不需要向用户索取，也不需要用户手工转交：

```
提交申请  →  人类在管理面板点「批准」  →  你按服务端给的间隔轮询  →  一次性取回明文密钥
```

完整流程与可直接复制的命令在**第 2 节**。拿到之后，最省事的做法是设成环境变量：

```bash
export TELEPORT_KEY='<你申请到的密钥>'
export TELEPORT_BASE='{{APP_BASE}}'
```

然后本文所有命令都可以直接复制粘贴。

**三条硬边界，先记住**：

- 不要猜测、枚举或爆破任何密钥、申请单 ID、`claim_secret`。失败一律是 401 / 404，
  不存在"多试几次就能猜中"的路径。
- 不要调用任何 `/api/admin/*` 接口 —— 那是人类管理面板的 Session Cookie 路径。
- 不要向用户索取 `AGENT_SECRET_KEY`：那是运维持有的 **root / 应急密钥**，能读全部报告，
  不是正常业务凭据，你也不需要它。

管理面板可以登录并查看 / 管理分享链接，但**面板的 Session Cookie 不能用来调用
`/api/reports`** —— 那条路径只认 Bearer 密钥，反过来你的密钥也进不了 `/api/admin/*`。

---

## 1. 这个站点是什么

一个**报告发布与时效分享**服务。典型流程：

1. 你（AI）产出一份报告（Markdown 文本）；
2. 你调用一次 `POST /api/reports`，把报告提交上去；
3. 服务返回一个**分享链接**，形如 `{{APP_BASE}}/s/<token>`；
4. 你把这条链接给用户。用户在浏览器里打开就能看到渲染好的报告
   （Markdown 排版、代码高亮、Mermaid 图表都会正常显示）；
5. 链接**带有效期**，到期自动失效 —— 这是它的核心特性，不是 Bug。

适合发：渗透测试报告、架构设计、开发进度、调研总结 —— 任何「需要给别人看，
但不必永久公开」的长文本。

---

## 2. 凭据：自助申请一把属于你自己的密钥

密钥是**自助申请**的：你提交申请，人类在面板批准，然后你凭申请时拿到的
`claim_secret` 轮询，取回明文密钥。**整个过程不需要人类把密钥复制给你。**

| 项 | 说明 |
| --- | --- |
| 名称 | Agent 密钥（自助申请得到的一把独立密钥；不是 `AGENT_SECRET_KEY`） |
| 形式 | 一个不透明的字符串（服务端保证 256 位熵） |
| 获取方式 | **自助申请**：`POST /api/agent-keys/applications` → 轮询 → 一次性领取 `key.token` |
| 传递方式 | 通过 `Authorization: Bearer <AGENT_KEY>` 请求头发送 |
| 适用接口 | `POST /api/reports` · `GET /api/reports/{id}` · `POST /api/share/{token}/revoke` · `GET /api/agent-keys/me` · `POST /api/agent-keys/renewals` |
| 不适用 | `/api/admin/*`（那是浏览器会话 Cookie 的地盘） |

### 2.1 端点一览

`{P}` 就是本文顶部的前缀 `{{ROUTE_PREFIX}}`。

| 步骤 | 方法 | 路径 | 认证 | 用途 |
| --- | --- | --- | --- | --- |
| 1 | `POST` | `{P}/api/agent-keys/applications` | 无（公开，限流） | 提交密钥申请，成功 `201` |
| 2 | `GET` | `{P}/api/agent-keys/applications/{id}` | 头 `X-Teleport-Claim: <claim_secret>` | 轮询状态；批准后**一次性**领取 `key.token` |
| 3 | `GET` | `{P}/api/agent-keys/me` | `Authorization: Bearer` | 查自身状态、有效期、用量 |
| 4 | `POST` | `{P}/api/agent-keys/renewals` | `Authorization: Bearer` | 发起续期（**需人类批准**），成功 `201` |
| 5 | `GET` | `{P}/api/agent-keys/renewals/{id}` | 头 `X-Teleport-Claim: <claim_secret>` | 轮询续期结果 |

> ⚠️ **字段命名约定（最容易踩的一条）**：**请求体字段一律 camelCase**
> （`label`、`purpose`、`requestedHours`、`expiresInHours`），
> **响应字段一律 snake_case**（`claim_secret`、`created_at`、`expires_at`、`token_prefix`）。
> 下面所有 `jq` 示例都按响应字段取值。用错命名风格（把响应字段写成 camelCase）
> 会让你拿到 `null`，然后拿着空凭据去轮询、全部 404；2.2 的取值脚本里有一行
> `[ -n "$CLAIM" ]` 就是用来当场卡住这种情况的。拿不到值时先用 `jq .` 打印原始响应。

所有响应都是统一封套（见第 6 节）：

```jsonc
{ "ok": true,  "data": { /* ... */ }, "requestId": "..." }
{ "ok": false, "error": { "code": "not_found", "message": "..." }, "requestId": "..." }
```

### 2.2 第一步：提交申请

```bash
export TELEPORT_BASE='{{APP_BASE}}'

curl -sS -X POST "$TELEPORT_BASE/api/agent-keys/applications" \
  -H 'Content-Type: application/json' \
  -d '{
    "label": "my-agent",
    "purpose": "发布渗透测试与架构报告",
    "requestedHours": 168
  }' | tee application.json
```

请求字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `label` | ✅ | 人类可读的 agent 名称；面板审批队列里靠它认出你 |
| `purpose` | ⭕ | 你打算用这把密钥做什么。写清楚能显著提高被批准的概率 |
| `requestedHours` | ⭕ | 你期望的有效期（小时）。省略或 `0` = **未指定**，由批准人决定，**不等于**永远有效 |

成功响应是 HTTP **201**（`data`）：

```json
{
  "ok": true,
  "data": {
    "id": "<申请单 id>",
    "claim_secret": "<一次性轮询凭据，只在这个响应里出现一次>",
    "status": "pending",
    "created_at": 1791563326606,
    "expires_at": 1791650126606,
    "poll_interval_seconds": 5
  },
  "requestId": "<请求 id>"
}
```

**`claim_secret` 只出现这一次**（服务端只保存它的哈希）。马上把它和 `id` 一起记下来，
注意是 snake_case：

```bash
APP_ID=$(jq -r '.data.id' application.json)
CLAIM=$(jq -r '.data.claim_secret' application.json)
POLL=$(jq -r '.data.poll_interval_seconds' application.json)
[ -n "$CLAIM" ] && [ "$CLAIM" != null ] || { echo "claim_secret 为空，检查响应字段名" >&2; exit 1; }
```

`claim_secret` 丢了就**无法再查询这张申请单**，只能重新申请。
它本身不是密钥，不要拿它调 `/api/reports`。

### 2.3 第二步：按 `poll_interval_seconds` 轮询，批准后一次性领取

人类批准后，**下一次**轮询的响应里才会带上明文 `key.token`。轮询必须带
`X-Teleport-Claim: <claim_secret>` 头。

轮询响应 `data` 的形状：

- 未批准（`status: "pending"` 或 `"rejected"`）：
  `{id, status, label, created_at, expires_at, decided_at}`；**`key` 字段缺席**。
- 已批准且首次领取：
  `{id, status: "approved", label, created_at, expires_at, decided_at, key: {...}}`。

> 领取到明文后要立刻**持久化到你自己的密钥存储**（你的凭据管理器 / 本机 600 权限的
> 文件 / 你的环境变量，由你决定）。下面的脚本用 `KEY_FILE` 代表它 ——
> 本文档不规定、也不会告诉你任何服务器上的密钥位置。

```bash
# 你自己的密钥存储位置，先设好（本机、不进 git、不写进报告）
: "${KEY_FILE:?请先把 KEY_FILE 设成你自己的密钥文件路径}"

while :; do
  # 把响应留在 shell 变量里（明文只出现一次的响应不落磁盘）；
  # 最后一行是 HTTP 状态码。
  out=$(curl -sS -w $'\n%{http_code}' \
          "$TELEPORT_BASE/api/agent-keys/applications/$APP_ID" \
          -H "X-Teleport-Claim: $CLAIM")
  code=${out##*$'\n'}
  body=${out%$'\n'*}
  status=$(jq -r '.data.status // empty' <<<"$body")

  case "$code:$status" in
    200:pending)
      sleep "$POLL"                 # 就按服务端给的间隔，别更快
      ;;
    200:approved)
      # 首次领取：明文密钥只在此刻出现一次，申请单随即变成 claimed
      umask 077
      jq -r '.data.key.token' <<<"$body" >"$KEY_FILE"
      chmod 600 "$KEY_FILE"
      echo "密钥已保存到 KEY_FILE"
      break
      ;;
    200:rejected)
      echo "申请被拒绝，需要人类处理：$body" >&2
      break
      ;;
    410)
      echo "申请单已过期，或批准后领取窗口已关闭：$body" >&2
      break
      ;;
    404)
      echo "申请单不存在 / claim_secret 不匹配 / 已经领取过：$body" >&2
      break
      ;;
    409)
      echo "当前活跃密钥数已达上限，需要人类先撤销或等待过期：$body" >&2
      break
      ;;
    429)
      echo "提交/轮询触发限流，退避后再试" >&2
      sleep 300
      ;;
    *)
      echo "未预期的响应（HTTP $code）：$body" >&2
      break
      ;;
  esac
done
```

领取成功后用它发请求：

```bash
export TELEPORT_KEY="$(cat "$KEY_FILE")"
curl -sS "$TELEPORT_BASE/api/agent-keys/me" -H "Authorization: Bearer $TELEPORT_KEY"
```

领取到的 `key` 对象形如：

| 字段 | 说明 |
| --- | --- |
| `key.token` | **明文密钥，仅此一次**。服务端只存哈希，丢了**无法找回**，只能重新申请 |
| `key.id` | 密钥 id（服务端侧标识） |
| `key.name` | 批准人给的名称 |
| `key.token_prefix` | 前几个字符，**仅用于面板辨识，不是密钥**，不要拿它当凭据 |
| `key.expires_at` | 到期时间（epoch 毫秒）；`0` = 永不过期（无界） |

**必须马上存好 `key.token`**（环境变量、本机 600 权限的密钥文件、你的凭据管理器都行）。
不要把它写进报告正文、代码注释、日志，也不要提交进 git —— 报告是会被分享出去的。

### 2.4 失败分支：各自的含义与处理

| 你看到什么 | HTTP | 含义 | 你该怎么做 |
| --- | --- | --- | --- |
| `status: "pending"` | `200` | 还没被人类处理 | 继续按 `poll_interval_seconds` 轮询；不要加速、不要并发轰炸 |
| `status: "approved"` + `key.token` | `200` | 已批准，这是**唯一一次**明文 | 立刻保存 `key.token`（2.3 的脚本已处理） |
| `status: "rejected"` | `200` | 人类拒绝了这次申请 | 停止轮询。把 `id` 与说明转告用户，问清原因后可改 `label` / `purpose` 重新申请 |
| `code: "gone"`（申请单过期） | `410` | 申请单在存活期（服务端当前配置：{{KEY_APPLICATION_TTL}}）内没被批准 | 旧 id 作废，重新走 2.2。**不要**继续轮询旧申请单 |
| `code: "gone"`（领取窗口关闭） | `410` | 已批准，但**超出了领取窗口**（服务端当前配置：{{KEY_CLAIM_WINDOW}}） | 申请单作废，重新走 2.2；下次批准后立刻领取 |
| `code: "not_found"` | `404` | 申请单不存在、`claim_secret` 不匹配，**或你之前已经领取过** | 服务端刻意不区分这些情况。凭据丢失就重新申请，不要去猜 id / secret |
| `code: "too_many_active_keys"` | `409` | 领取时活跃密钥总数已达上限 | 申请单**仍然有效**：让人类撤销无用密钥或等一把过期，然后在窗口内重试领取 |
| `code: "too_many_pending"` | `409` | 提交申请时，待审批队列已满 | 等队列消化后重新提交；不要连发申请 |
| `code: "rate_limited"` | `429` | 提交申请过于频繁：**每个 IP 每小时最多 {{KEY_APPLY_PER_HOUR}} 次申请**。响应头带 `Retry-After`（秒） | 按 `Retry-After` **退避等待**，不要立即重试 —— 立即重试只会继续撞限流；也不要换 IP / 换身份绕过 |

> 上表中的申请单存活期与领取窗口是**服务端当前配置值**（本文档每次请求都会注入最新值）；
> 某张申请单的确切失效时刻，永远以响应里的 `expires_at` 为准。
>
> 还有一个通用建议：**一次只推进一张申请单**。反复提交新申请既不会加速，
> 也会撞上 `rate_limited` 或 `too_many_pending`。

### 2.5 日常：查状态（`/me`）与续期（`/renewals`）

**查自己**：

```bash
curl -sS "$TELEPORT_BASE/api/agent-keys/me" \
  -H "Authorization: Bearer $TELEPORT_KEY"
```

返回 `data`（snake_case）：

```json
{
  "id": "<密钥 id>",
  "name": "<批准人给的名称>",
  "token_prefix": "<前几个字符，仅供面板辨识>",
  "created_at": 1791563326606,
  "expires_at": 1792168126606,
  "revoked_at": 0,
  "last_used_at": 1791570000000,
  "request_count": 12,
  "note": "<批准备注，可能为空>",
  "root": false
}
```

- `expires_at`：到期时间（epoch 毫秒）；**`0` 表示永不过期（无界），不是「1970 年就已经过期」**
  —— 读到 `0` 请一律理解成「永不」，不要对它做减法或当成异常值。
- `root`：正常 agent 密钥恒为 `false`。如果你用 `AGENT_SECRET_KEY`（root / 应急密钥）
  调这个接口，会得到 `"root": true` —— 那是运维凭据，你没有也不需要它。
- 一旦密钥**已过期或被撤销**，所有请求（含 `/me`）都会变成 **401**。
  已撤销的密钥只能重新申请；已过期的密钥若在过期前提交过续期，仍可能被批准后恢复
  （见下面的「关键规矩」）。

**发起续期**（成功返回 **201**）：

```bash
curl -sS -X POST "$TELEPORT_BASE/api/agent-keys/renewals" \
  -H "Authorization: Bearer $TELEPORT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"requestedHours": 720}' | tee renewal.json
```

返回 `data`：`{id, key_id, status:"pending", requested_hours, created_at, claim_secret}` ——
续期申请也有**自己的** `claim_secret`，同样只在这一次响应里出现（它由服务端按
`id + key_id` 现场重算，连哈希都不落库）。用它轮询结果：

```bash
RENEWAL_ID=$(jq -r '.data.id' renewal.json)
RENEWAL_CLAIM=$(jq -r '.data.claim_secret' renewal.json)

curl -sS "$TELEPORT_BASE/api/agent-keys/renewals/$RENEWAL_ID" \
  -H "X-Teleport-Claim: $RENEWAL_CLAIM"
```

返回 `data`：`{id, key_id, requested_hours, status, created_at, decided_at, granted_expires_at}`。
`status` 为 `pending` / `approved` / `rejected`；**续期不换密钥**：原 `key.token` 继续使用。
批准后 `granted_expires_at` 是延长后的新到期时间（epoch 毫秒），规则是：

```
granted_expires_at = max(当前 expires_at, 批准时刻) + 批准时长
```

也就是说 **续期是在现有到期时间上「叠加」，不是「重设」**：提前续期**不会浪费**剩余时间，
剩余时间会完整保留并叠加新时长。例如一把还剩 24 小时的密钥，续期 168 小时被批准后，
到期时间变成「现在 + 24h + 168h」（净增 168 小时），而**不是**「现在 + 168h」。

两个永远保持「不过期」的边界（都不会被降级）：

- 密钥本来就是 `expires_at = 0`（无界）→ 续期后仍是 `0`；
- 人类批准的时长 ≤ 0（显式授予「永不过期」）→ `0`。

**续期绝不会缩短一把密钥的授权**；要缩短只能由人类在面板上显式操作。
想确认最终到期时间，批准后再调一次 `/me`（`/me` 是权威值）。

关键规矩：

- **续期需要人类批准，和首次颁发一样。** 审批可能隔夜甚至更久才有结果，
  所以**提前数天发起**（例如有效期还剩一周、或剩余时间不足总量的三分之一时），
  **不要卡着到期时间才续**。因为续期是**叠加**而不是重设，提前发起**不会损失**任何剩余时间
  —— 早续只赚不赔。真正的风险不是「审批慢」，而是**拖到密钥已经过期才去提交**：
  那一刻起你连提交续期的机会都没有了（401），只能重新申请。
- 已经有一张待批准的续期申请时，再发起会返回 **409**（`code: "renewal_exists"`）。
  这时不要重复提交，先按上面的 `GET /api/agent-keys/renewals/{id}` 轮询已有那张。
- 续期申请同样只在提交响应里给你一次 `claim_secret`；丢了就查不到结果
  （但密钥本身不受影响），可以重新发起。
- **续期只看「提交时刻」和「是否被撤销」**，请按这三条理解：
  - 在密钥**过期前**提交的续期，即使审批拖到密钥已经过期之后才批准，**仍然生效** ——
    密钥会恢复可用（新的 `expires_at` 从批准时刻起算并叠加）。这也是「提前数天发起」
    依然重要的原因：卡点提交不会白费，只要**提交那一刻**密钥还没过期。
  - 密钥**一旦过期**，你就**无法再新提交**续期：`/me` 与 `POST /api/agent-keys/renewals`
    都会返回 **401**（Bearer 已失效）。此时只能重新走申请流程（第 2.2 节）。
  - 已**撤销**的密钥**永远不会**被续期复活：批准针对它的续期会返回 **404**
    （服务端把已撤销的密钥当作不存在）。撤销是人类的最终决定，不要试图绕过。

### 2.6 权限边界：你只能管你自己发布的报告

这是**归属模型**，不是普通的权限位。`POST /api/reports` 会把发布者记在报告上，
之后所有读写都按归属判定：

- 你的密钥**只能读、只能撤销它自己发布的报告**。
- 读**别人的**报告、撤销**别人的**分享链接 → **404**（而不是 403）。
  服务端刻意不暴露"这个 id 存在，只是不属于你"，与"未知 token 与已吊销都返回 404"
  是同一套语义。**不要把 404 当成故障，也不要拿它去探测别人有哪些报告。**
- 你的密钥发布报告**不受归属限制**（可以发任意多篇）；归属约束的是读与撤销，
  以及别人的报告对你不可见。
- `AGENT_SECRET_KEY`（root / 应急密钥）能读全部报告，那是运维的凭据，你没有也不需要它。
- `/api/admin/*` 全部走人类的 Session Cookie，你的 Bearer 密钥在这里无效；
  反过来面板的 Session Cookie 也不能调 `/api/reports`。

安全要求：

- **不要把密钥写进报告正文、代码注释、日志或任何会被分享出去的内容里。**
- 不要把密钥提交进 git 仓库；用环境变量或本机 600 权限的密钥文件。
- 未拿到密钥时，**只读的公开接口仍然可用**（见第 4 节），不要为此去尝试绕过鉴权。
- 不要猜测 / 爆破密钥、申请单 id、`claim_secret`：所有失败都统一成 401 / 404，
  爆破只会触发限流。

---

## 3. 核心操作：发布一份报告

成功返回 **201**（请求体 camelCase、响应 snake_case，见 2.1 的约定）：

```bash
curl -sS -X POST "$TELEPORT_BASE/api/reports" \
  -H "Authorization: Bearer $TELEPORT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "title": "报告标题",
    "category": "architecture",
    "format": "markdown",
    "content": "# 标题\n\n正文内容……",
    "autoShareHours": 168
  }'
```

**注意 `content` 里的换行**：JSON 字符串里必须写成 `\n`。多行内容建议用
文件 + `jq` 避免转义出错：

```bash
jq -n --rawfile c report.md \
  '{title:"报告标题", category:"architecture", format:"markdown", content:$c, autoShareHours:168}' \
  > payload.json

curl -sS -X POST "$TELEPORT_BASE/api/reports" \
  -H "Authorization: Bearer $TELEPORT_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @payload.json
```

### 请求字段

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `title` | ✅ | 1–300 字符 |
| `content` | ✅ | 报告正文，不能为空；上限 {{MAX_CONTENT_BYTES}} **字节**（约 340 KB 中文） |
| `format` | ⭕ | `"markdown"`（默认，**推荐**）或 `"html"` |
| `category` | ⭕ | 只允许 `^[a-z0-9][a-z0-9_-]{0,31}$`；省略则为 `general` |
| `metadata` | ⭕ | 任意 JSON 对象，原样存下来，用于附加信息 |
| `autoShareHours` | ⭕ | 自动创建分享链接的有效期（小时）。省略则用服务端默认 **{{DEFAULT_SHARE_HOURS}} 小时**。`0` = 永不过期。支持小数 |

> ⚠️ **`format` 一定用 `"markdown"`。** `"html"` 虽然会被接受并保存，
> 但**服务端不渲染 HTML** —— 分享页会显示一句「HTML 渲染尚未启用」的提示，
> 用户的报告正文根本看不到。这是刻意的安全取舍，不是临时故障。

### 返回

```json
{
  "ok": true,
  "data": {
    "id": "03cba87f-f14e-4959-b0dd-66313ee27077",
    "title": "报告标题",
    "category": "architecture",
    "format": "markdown",
    "created_at": 1791563326606,
    "updated_at": 1791563326606,
    "share": {
      "token": "QDE56Dvt4kRsvCQjyJ6DRA",
      "url": "{{APP_BASE}}/s/QDE56Dvt4kRsvCQjyJ6DRA",
      "created_at": 1791563326606,
      "expires_at": 1792168126607,
      "is_active": true,
      "view_count": 0
    }
  },
  "requestId": "582e2b759ed2711d7a50db6671227169"
}
```

**你要交给用户的就是 `data.share.url`。** 直接复制这一整条，不要自己拼接。

所有 `*_at` 时间都是 **UNIX epoch 毫秒**（不是秒）。`expires_at` 为 `0` 表示永不过期。

---

## 4. 其余接口

### 读取一份报告（需要 Bearer，只能读自己的）

```bash
curl -sS "$TELEPORT_BASE/api/reports/<id>" \
  -H "Authorization: Bearer $TELEPORT_KEY"
```

只能读**你自己发布的**报告；别人的报告返回 **404**（见 2.6），不要据此判断报告是否存在。

### 读取分享内容（公开，无需鉴权）

```bash
curl -sS "$TELEPORT_BASE/api/share/<token>"
```

返回 `{ "data": { "report": {...}, "share": {...} } }`。这条**不需要密钥**，
所以你可以放心用它来验证一条分享链接是否还能打开 —— 但注意：公开读取**绕过了归属检查**，
任何拿到 token 的人都能读；token 泄露等于内容泄露。

### 撤销一条分享链接（需要 Bearer，只能撤销自己的）

```bash
curl -sS -X POST "$TELEPORT_BASE/api/share/<token>/revoke" \
  -H "Authorization: Bearer $TELEPORT_KEY"
```

撤销后链接**立即失效且不可恢复**。只有用户明确要求时才做。
按该 token 所属报告的归属判定：撤销**别人的**链接返回 **404**。

### 健康检查（公开）

```bash
curl -sS "$TELEPORT_BASE/api/health"
```

---

## 5. 失效语义：404 与 410 的区别

这个区别很重要，请照此判断并向用户解释：

| 状态码 | 含义 | 你该怎么回应用户 |
| --- | --- | --- |
| `200` | 链接有效 | 正常 |
| `404` | **未知**、**已被撤销**，或**不属于你** | 「链接不存在、已被撤销，或这份报告不属于当前密钥」——服务端刻意不区分这些情况，以免泄露某条链接 / 某份报告是否存在 |
| `410` | **已过期** | 「链接已过期，需要重新生成」 |

**不要把 404 当成"系统故障"**。多数情况下是用户拿了一条旧链接；
如果你用 `/api/reports/{id}` 读到 404，也可能是因为那份报告不是你发布的。

---

## 6. 错误处理

所有响应都是同一个封套，`ok: false` 时错误信息在 `error` 里：

```json
{
  "ok": false,
  "error": {
    "code": "bad_request",
    "message": "`title` must be between 1 and 300 characters",
    "details": { "field": "title" }
  },
  "requestId": "43c0721fa8586cf13f1b4f017498ffd8"
}
```

| 状态码 | `code` | 常见原因 / 处理 |
| --- | --- | --- |
| `400` | `bad_request` | 字段不合法。看 `error.message`（会明确指出哪个字段）和 `details.field`，改正后重试 |
| `401` | `unauthorized` | 密钥缺失、不正确、**已过期或已被撤销**。**不要反复重试、更不要爆破**：若你有一张在过期前提交的续期，等它被批准即可恢复（见 2.5）；否则只能重新申请（过期后就无法再提交续期了） |
| `404` | `not_found` | 见第 5 节（也可能是"这份报告不属于你"） |
| `409` | `renewal_exists` · `too_many_pending` · `too_many_active_keys` | 状态冲突：已有待批准续期 / 待审批队列满 / 活跃密钥数满。见 2.4、2.5 |
| `410` | `gone` | 分享链接已过期，或申请单过期 / 领取窗口已关闭（见 2.4） |
| `429` | `rate_limited` | 限流（申请接口**每个 IP 每小时最多 {{KEY_APPLY_PER_HOUR}} 次申请**，响应头带 `Retry-After` 秒数）。**退避等待**后再试，不要立即重试 |
| `500` | `internal_error` | 服务端问题。可以稍后重试一次，仍失败则报告用户 |

**报错时把 `requestId` 一并带上**，用户排查问题需要它。

`401` 和 `400` 是两类完全不同的失败，不要混为一谈：
前者是「你没被授权」，后者是「你发的数据不对」。

---

## 7. 完整示例：发布并交付

```bash
# 0. 若还没有密钥：先按第 2 节自助申请，并导出 TELEPORT_KEY / TELEPORT_BASE
#    （本节假设二者已就绪）

# 1. 准备报告正文
cat > report.md <<'EOF'
# 登录接口渗透测试

## 结论
发现 2 个中危问题。

## 详情
1. 无速率限制
2. 错误信息区分账号是否存在
EOF

# 2. 组装请求体（避免手工转义）
jq -n --rawfile c report.md \
  '{title:"登录接口渗透测试", category:"pentest", format:"markdown", content:$c, autoShareHours:72}' \
  > payload.json

# 3. 发布，取出分享链接
curl -sS -X POST "$TELEPORT_BASE/api/reports" \
  -H "Authorization: Bearer $TELEPORT_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @payload.json \
  | tee resp.json | jq -r '.data.share.url'

# 4. 自检：确认链接真的能打开（无需密钥）
URL=$(jq -r '.data.share.url' resp.json)
curl -sS -o /dev/null -w '%{http_code}\n' "$URL"     # 期望 200
```

把第 3 步输出的 URL 给用户即可。

---

## 8. 把它变成你自己的 skill

这份文档的目标就是让你能**一次性**生成一个可长期复用的能力。不同 Agent/工具的
skill 机制不一样，你应当按**你自己所处的环境**的规范来生成，而不是照抄某个模板。

无论哪种形式，一个够用的 skill 只需包含这几件事：

1. **触发条件** —— 什么时候用：用户说「发个报告」「把这份总结发布出去」
   「生成一个分享链接」「这份东西给对方看一下」等。
2. **初始化：先自助申请密钥** —— 这是 skill 的第一步，**不要**假设用户手里有密钥、
   也不要向用户索取 `AGENT_SECRET_KEY`。照第 2 节实现：
   `POST /api/agent-keys/applications` → 保存 `id` / `claim_secret` →
   按 `poll_interval_seconds` 轮询（头 `X-Teleport-Claim`）→ 批准后**一次性**
   保存明文 `key.token` → 之后一律 `Authorization: Bearer <token>`。
   密钥存进环境变量或本机 600 权限的密钥文件，**永不写进报告或仓库**。
   注意申请单有存活期（当前 {{KEY_APPLICATION_TTL}}）：超时未获批即作废，要重新申请。
3. **生命周期** —— 用 `GET /api/agent-keys/me` 读 `expires_at`（`0` = 永不过期/无界）；
   有效期剩几天时用 `POST /api/agent-keys/renewals` 发起续期并按同法轮询结果。
   **续期需人类批准**，所以要在到期前**数天**发起；续期是在现有到期时间上**叠加**
   （早续不损失剩余时间），已有 pending 时返回 409，别重复提交。
4. **基地址** —— `{{APP_BASE}}`，所有接口前缀 `{{ROUTE_PREFIX}}`。
5. **一个核心动作** —— `POST {{ROUTE_PREFIX}}/api/reports`，
   字段与约束照第 3 节。**`format` 固定用 `markdown`。**
6. **交付物** —— 把响应里的 `data.share.url` 原样给用户。
7. **权限边界** —— 只能读 / 撤销**自己发布的**报告；读别人的返回 404（不是 403）；
   不调 `/api/admin/*`；面板 Session Cookie 与 Bearer 密钥不可互换。
8. **错误处理** —— `401` 是密钥问题（过期 / 撤销）：过期前提交的续期获批后可恢复，
   否则重新申请（**过期后就无法再提交续期**，撤销的密钥永远不会被续期复活）；
   `404 / 410` 是链接或报告失效，而非系统故障；
   `429` 是申请限流（每小时最多 {{KEY_APPLY_PER_HOUR}} 次申请），**退避等待**而不是立即重试。

建议在 skill 里**内联**这份契约的要点（而不是每次运行时都来抓这个页面），
同时保留本文档地址作为「需要细节时去查」的入口：

```
{{APP_BASE}}/ai.md      # 纯 Markdown，最适合直接读取
{{APP_BASE}}/ai         # 同一份内容的网页版
```

**注意时效性**：`category` 的取值范围、内容上限、申请单存活期与领取窗口等以本文档
与接口实际返回为准；如果调用时发现行为与文档不符，请以接口返回的错误信息为准，
并提示用户文档可能需要更新。

---

## 9. 给人类的附录（AI 可跳过）

- 管理面板：`{{APP_BASE}}/`。agent 的密钥申请会出现在面板的审批队列里，
  由你点「批准」或「拒绝」；批准后 agent 自己领取，**你不需要复制任何密钥给 agent**。
- 本文档是**公开可读**的，因此**这里不会写任何密钥、也不会写服务器上密钥存在哪里**。
  需要 agent 密钥时，让 agent 自助申请（第 2 节），或由你在面板里手动新建并私下转交。
- 每把密钥独立：面板上可以撤销单把、调整它的有效期；撤销或过期只影响这一把，
  其它 agent 不受影响。agent 的密钥到期前应自行发起续期，**续期同样需要你批准**；
  批准续期是在它**现有到期时间上叠加**（只延长、不缩短），要缩短请用面板的 PATCH 显式操作。
  两条批准时的语义：**过期前**提交的续期，即使你批准时密钥已过期，也会把它**恢复可用**；
  而**已撤销**的密钥不会被续期复活 —— 批准针对它的续期会返回 **404**，撤销是最终决定。
- 门户与分享页都是 `noindex`，不会被搜索引擎收录。

---

## 附：文档元信息

| 项 | 值 |
| --- | --- |
| 文档版本 | 随二进制一起发布（同一 git revision） |
| 基地址 | `{{APP_BASE}}` |
| 路径前缀 | `{{ROUTE_PREFIX}}` |
| 环境 | `{{ENVIRONMENT}}` |
| 默认分享时长 | {{DEFAULT_SHARE_HOURS}} 小时 |
| 内容上限 | {{MAX_CONTENT_BYTES}} 字节 |
