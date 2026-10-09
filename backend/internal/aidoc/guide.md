# Teleport — 给 AI 的使用说明

> **这份文档是写给 AI Agent 读的。** 你（AI）读完应当能独立完成：发布报告、拿到分享链接、
> 把链接交给用户、以及按需撤销。
>
> 人类用户请访问 `{{APP_BASE}}/`（管理面板）。

- **站点基地址（下称 `APP_BASE`）**：`{{APP_BASE}}`
- **所有接口都在这个前缀下**：`{{ROUTE_PREFIX}}`
- **当前环境**：`{{ENVIRONMENT}}`
- 本文档的纯文本版本：`{{APP_BASE}}/ai.md`

---

## 0. 先读这一节

**你需要的唯一凭据是一个 Bearer Token**（下面叫 `AGENT_KEY`）。它由用户提供，
本文档不包含它，也不会通过任何接口泄露它。

拿到 `AGENT_KEY` 后，最省事的做法是设成环境变量：

```bash
export TELEPORT_KEY='<用户给你的那串密钥>'
export TELEPORT_BASE='{{APP_BASE}}'
```

然后所有命令都可以直接复制粘贴。

**如果用户没有给你 `AGENT_KEY`**：不要尝试猜测或爆破，也不要调用任何
`/api/admin/*` 接口。直接告诉用户你需要一个 agent 密钥，并让他把
「本文档的地址 + 密钥」一起给你。管理面板可以登录并查看/管理分享链接，但
**面板的会话 Cookie 不能用来调用 `/api/reports`** —— 那条路径只认 Bearer Token。

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

## 2. 凭据：怎么拿到、怎么用

| 项 | 说明 |
| --- | --- |
| 名称 | Agent 密钥（`AGENT_SECRET_KEY`） |
| 形式 | 一个不透明的字符串（base64 风格，约 44 字符） |
| 获取方式 | **向用户索取**。用户从服务器配置中取出后交给你 |
| 传递方式 | 通过 `Authorization: Bearer <AGENT_KEY>` 请求头发送 |
| 适用接口 | `POST /api/reports` · `GET /api/reports/{id}` · `POST /api/share/{token}/revoke` |
| 不适用 | `/api/admin/*`（那是浏览器会话 Cookie 的地盘） |

安全要求：

- **不要把密钥写进报告正文、代码注释或任何会被分享出去的内容里。**
- 不要把密钥提交进 git 仓库；用环境变量或本机密钥文件。
- 你没拿到密钥时，**只读接口仍然可用**（见第 4 节），不要为此去尝试绕过鉴权。

---

## 3. 核心操作：发布一份报告

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

### 读取一份报告（需要 Bearer）

```bash
curl -sS "$TELEPORT_BASE/api/reports/<id>" \
  -H "Authorization: Bearer $TELEPORT_KEY"
```

### 读取分享内容（公开，无需鉴权）

```bash
curl -sS "$TELEPORT_BASE/api/share/<token>"
```

返回 `{ "data": { "report": {...}, "share": {...} } }`。这条**不需要密钥**，
所以你可以放心用它来验证一条分享链接是否还能打开。

### 撤销一条分享链接（需要 Bearer）

```bash
curl -sS -X POST "$TELEPORT_BASE/api/share/<token>/revoke" \
  -H "Authorization: Bearer $TELEPORT_KEY"
```

撤销后链接**立即失效且不可恢复**。只有用户明确要求时才做。

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
| `404` | **未知** *或* **已被撤销** | 「链接不存在或已被撤销」——服务端刻意不区分这两者，以免泄露某条链接是否曾经存在 |
| `410` | **已过期** | 「链接已过期，需要重新生成」 |

**不要把 404 当成"系统故障"**。多数情况下是用户拿了一条旧链接。

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
| `401` | `unauthorized` | 密钥缺失或不正确。**不要重试**，去问用户要正确密钥 |
| `404` | `not_found` | 见第 5 节 |
| `410` | `gone` | 分享链接已过期，需重新创建 |
| `500` | `internal_error` | 服务端问题。可以稍后重试一次，仍失败则报告用户 |

**报错时把 `requestId` 一并带上**，用户排查问题需要它。

`401` 和 `400` 是两类完全不同的失败，不要混为一谈：
前者是「你没被授权」，后者是「你发的数据不对」。

---

## 7. 完整示例：发布并交付

```bash
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
2. **前置条件** —— 需要 `AGENT_KEY`。没有就问用户要，不要试图绕过。
3. **基地址** —— `{{APP_BASE}}`，所有接口前缀 `{{ROUTE_PREFIX}}`。
4. **一个核心动作** —— `POST {{ROUTE_PREFIX}}/api/reports`，
   字段与约束照第 3 节。**`format` 固定用 `markdown`。**
5. **交付物** —— 把响应里的 `data.share.url` 原样给用户。
6. **错误处理** —— `401` 去要密钥；`404/410` 是链接失效而非故障。

建议在 skill 里**内联**这份契约的要点（而不是每次运行时都来抓这个页面），
同时保留本文档地址作为「需要细节时去查」的入口：

```
{{APP_BASE}}/ai.md      # 纯 Markdown，最适合直接读取
{{APP_BASE}}/ai         # 同一份内容的网页版
```

**注意时效性**：`category` 的取值范围、内容上限等以本文档为准；如果调用时
发现行为与文档不符，请以接口实际返回的错误信息为准，并提示用户文档可能需要更新。

---

## 9. 给人类的附录（AI 可跳过）

- 管理面板：`{{APP_BASE}}/`
- 本文档是**公开可读**的，因此**这里不会写任何密钥、也不会写服务器上密钥存在哪里**。
  需要 agent 密钥时，请运维方从服务端配置中取出后私下转交。
- 密钥轮换后旧密钥立即失效，所有正在使用它的 agent 都需要重新获取。
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
