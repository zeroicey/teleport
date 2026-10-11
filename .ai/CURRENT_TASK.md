# Current task

**Status:** ✅ **一键交接链接已上线并线上实测（本机真 Chromium 25/25 + 生产 29/29）** · 线上 `60bcbe8` ·
**Updated:** 2026-10-11
**下一步：** 把交接链接**发给真人试用**（用户要的「找一些人来测试一下」——这条功能本身就是邀请方式）

> 本文件是**当前状态**，不是流水账。每一轮的完整叙事在
> `.ai/sessions/*-handoff.md` 与 `.ai/decisions/`、`.ai/pitfalls/` 里。

---

## 本轮：一键交接链接（用户原话）

> 「我直接用管理员账号去申请一个密钥，然后把带密钥的链接（或者一段提示词，告诉他去哪里看怎么发布）
> 直接发给他。他拿到链接后直接就能去弄报告，不需要我再去平台上操作确认，他就可以直接完成所有内容了。
> 反正你看一下怎么做比较好，简单一点就行。」

用户把设计权交给实现方，要求**简单**。裁决见
**`.ai/decisions/2026-10-11-one-step-key-handoff.md`**（✅ ACCEPTED，D1–D11）。

**做法（一句话）**：管理员在面板点「生成交接链接」→ 得到 `…/k/{id}#{claim_secret}` →
转发给使用者（人或 AI）→ 对方打开链接**点一下**就拿到密钥，页面同时给出**可直接粘贴给 AI 的提示词**。

| 关键设计 | 为什么 |
| --- | --- |
| claim secret 放 **URL fragment**（`#` 之后） | fragment **不会**发给服务器：不进访问日志、不进 `Referer`、不进任何中间层 |
| 领取必须**点按钮**（不是打开链接就领） | 聊天软件的链接预览机器人不执行 JS、也拿不到 fragment，**烧不掉**这条链接 |
| 链接里**不放真密钥**，放一次性 claim secret | 真密钥会永久留在聊天记录里；claim secret 只值一次 |
| 领取后 `history.replaceState` 抹掉 fragment + `sessionStorage` 缓存 | 刷新页面不会看起来像「被偷了」，也不会二次领取 |
| 生成时**不建密钥行**，只在领取时建 | 保住不变量：**密钥的寿命从使用者拿到它开始**；也顺手绕开 `KEY_MAX_PENDING` 队列 |
| 链接有效期复用 `KEY_APPLICATION_TTL`（24h） | 不新增旋钮；「申请」与「交接」是同一件事 |
| 404 折叠「已领取/不存在」 | 沿用既有链接语义，不确认 id 是否存在 |

**改到的文件**：`store/keys.go`（`CreateHandoffApplication`，直接插 `approved` 行）、
`api/keys.go`（`handleCreateKeyHandoff` + `handoffURL` + `handoffPrompt`）、
`api/router.go`（`GET /k/{id}` + `GET /k/`、`POST /api/admin/key-handoffs`）、
`views/handoff.go`（新页面，独立 CSP/no-store/noindex）、`web/src/share/main.ts`（`setupKeyHandoff`）、
`web/src/api.ts`、`web/src/views/KeyManagementView.vue`、`spa/spa.go`（`/k/` 不再落到 SPA 兜底）、
`aidoc/guide.md`（2.7 节）、`aidoc/html.go`（llms.txt）、`README.md`。

**真 Chromium 端到端验证：25/25 全绿**（`bin/e2e/verify-handoff.cjs`，本机 127.0.0.1:8799 + 真实
Chrome for Testing 156）：领取按钮出现 → 真点击 → 拿到 43 字符密钥 → fragment 被抹掉 →
同标签刷新仍显示（sessionStorage）→ **换一个浏览器上下文则显示已领取** → 用这把密钥
`POST /api/reports` 201 发布成功 → 面板「生成交接链接」对话框出链接 + 提示词 → 真点击复制
→ 面板生成的链接立刻可领。

**这一步抓出两个 Go 测试永远抓不到的 bug**（详见
`.ai/pitfalls/cases/2026-10-11-handoff-page-contract-drift.md`）：

1. 服务端产出 `#<secret>`，客户端按 `#k=<secret>` 解析 → 领取按钮**永不出现**；
2. 页面属性传的是 API 根（`…/api`）而客户端自己再拼 `/api` → 请求打到 `/api/api/…` **404**，
   而页面把 404 说成「这条链接已经领取过了」——**把 bug 报告成了功能**。

两条都补了守卫，并加进反证脚本（F9/F10）。**反证 10 条全红**（`bin/e2e/falsify-handoff.py`，
每条都让对应测试变红，且还原后文件逐字节一致）。

**独立对抗验证（另一个 agent，只读仓库、只打线上/本地接口）**：结论是「6 条主张未能推翻，
2 处被推翻/存疑」，其中 **1 个真 bug 已修**：

- ✅ **已修（真 bug）**：`HEAD` 请求会**消耗一次性链接并扔掉密钥** —— Go 的 `ServeMux` 把
  `HEAD` 交给 `GET` 模式，而领取处理器会写状态；HEAD 无 body，收件人什么都拿不到。
  已改为 405 + `Allow: GET`（领取与续期两个端点都加），新增
  `TestKeyHandoffClaimRefusesHEAD`（断言「HEAD 之后链接仍可领取」）+ 反证 **F11**。
  详见 `.ai/pitfalls/cases/2026-10-11-head-request-consumed-a-one-time-link.md`。
- 📝 **文档与实现不符（已改文档）**：页面层**是**存在性/状态 oracle（能看到
  `open/claimed/expired/unknown`、label、到期时间），接口层不是（claimed 与 unknown 返回
  逐字节相同的 404）。原 `router.go` 注释与决策 D11 都写成「页面不确认 id 是否存在」，不实。
  裁决：**接受页面多说**（收件人要看得懂「已经领过了」），补 `TestKeyHandoffPageStateVisibility`
  钉住，D11 修订。
- ⚠️ **配置约束（未改代码，已写入 runbook）**：Caddy 默认只脱敏 `Authorization`/`Cookie`，
  **站点级 `log` 会把 `X-Teleport-Claim` 明文写盘**。当前线上无站点级 log（已复核
  `/root/hcyj/caddy/Caddyfile`，`/data/caddy/` 下无 access.log）→ 今天不泄漏，
  但这是一条「靠没人加 log」维持的性质，故写进
  `.ai/runbooks/caddy-routing-on-shared-host.md`（含过滤片段）与决策 D12。
- ℹ️ 已知小差异（不改）：`/k/{id}/extra` 命中 `GET /k/` 子树模式，返回 200「链接无效」页而不是 404 ——
  与「绝不吐 SPA 外壳」的目的一致，无泄漏。

**未做**：用户没有要求密码保护、批量拒绝、限流等。

---

## 线上状态（2026-10-11 15:28 部署，`60bcbe8`）

| 项 | 值 |
| --- | --- |
| 二进制 sha256 | `fa224ae13b16e5db42f5ab8c4d04daed16376bfd58fdfc1afd37b1a75e4634cd`（本地=线上） |
| 回滚二进制 | `/data/services/teleport/teleport.prev-20261011-152828`（= 旧 `adc2be3`） |
| 数据库备份 | `teleport.db.bak-20261011-152828`（部署前冷备）、`teleport.db.bak-cleanup-20261011-*`（清残留前） |
| 库内容 | **3 reports / 4 share_tokens / 1 agent_key / 1 application**，`integrity_check` = ok（与部署前**完全一致**） |
| 部署窗口日志 | `error|panic|fatal` = **0** |

**线上实测（生产脚本走公网 URL，故 Caddy + 前缀 + TLS 全在内）：29/29 全绿**
（`bin/e2e/verify-handoff-online.py`，在 hcyj 上跑）：
生成交接链接 201（fragment 形态、24h 链接、1h 密钥、提示词含 `X-Teleport-Claim`、响应无 hash）→
匿名访问 `/k/{id}` 200 + `no-store` + 非 SPA 外壳 + `data-handoff-spent="0"` + 有收件人名 +
**页面不含 claim secret** + `data-app-base` 不是 API 根 + 严格 CSP →
`X-Teleport-Claim` 领取 200（43 字符）→ **重放 404** → 页面翻到 `spent="1"` →
未知 id 得到同样的说明（不泄漏）→ 用领到的密钥 `POST /api/reports` **201** 且分享链接可读 200 →
测试数据全部复原（报告删除、密钥撤销；**首次崩溃运行留下的残留也一并扫掉**）→
审计：报告数回到基线、无存活测试密钥。

**大陆观测点（koma 腾讯云）**：`/api/health` 200；`/k/`、`/k/{uuid}`、`/k/{uuid}/extra`
全部 **200 且 `id="app"` 出现 0 次**（说明页，不是 SPA 外壳）；缓存头三连查通过
（`share.js` = `no-cache`、入口 = `immutable`）。

## 下一步（唯一一件）

把交接链接**发给真人试用**。用户明确要求「找一些人来测试一下」，
而这条功能本身就是为了把「发给别人」变成一步，所以**试用就是验收**。
邀请模板与「为什么值得试」见本文件末尾「给人试用怎么说」。

---

## 部署与回滚须知

部署流程见 `.ai/runbooks/deploy-teleport.md`（含 4.1 缓存头三连查、4.2 **停服后**再拷数据库）。
**注意「线上 version ≠ main HEAD」是正常的**：部署后只改文档/记忆库的提交不必重发二进制；
但只要改了 Go 或 `web/src/`，**必须** `pnpm run build:release` 并重新部署（前端 `//go:embed` 进二进制）。

**回滚安全性**：本改动**无 schema 变更**，回滚只需换回二进制并重启
（`cp teleport.prev-20261011-152828 teleport && systemctl restart teleport`）。
旧二进制下 `/k/` 变 404，但**已经生成**的 approved 申请仍可用老端点
（`GET /api/agent-keys/applications/{id}` + `X-Teleport-Claim`）领取 —— 也就是回滚不会
把已经发出去的链接变成废纸。

---

## 已交付（全部已部署并线上实测）

| 轮次 | 内容 | 提交 |
| --- | --- | --- |
| 追加① | `/assets/share.js` 缓存按**文件名形态**判定（原被下发 `immutable`，回访浏览器最长一年用旧副本） | `e5bca8e` |
| 追加② | `/llms.txt` 改指自助申请端点；CSP 常量去重 | `dbdc434` |
| 追加② | 记忆库 + README 同步，删除 CF 遗留 `public/` | `b509960` |
| 追加③ | **密钥过期后 90 天宽限续签**：过期密钥**只能**调续期端点；续期不换 `key_id` 故归属连续；`GET /api/reports` 列出自己的报告；过期可见性响应头 | `0c33125` `711f194` |
| 追加④ | **报告原地更新与删除**：`PATCH`/`DELETE /api/reports/{id}`（agent 归属限定）与 `/api/admin/reports/{id}`（面板）；未知字段/空 patch 一律 400 | `1917768` `f609f8a` |
| 追加⑤ | **`format:"html"` 报告原样渲染**：同源 iframe + 新端点 `GET /s/{token}/html` 逐字节回源；markdown 路径未放宽 | `adc2be3` |
| 追加⑥ | **一键交接链接**：`POST /api/admin/key-handoffs` + `GET /k/{id}#<secret>`，使用者点一下即得密钥与可粘贴提示词 | `60bcbe8` |

**审计四条**（追加②，全部已部署，守卫名在代码里）：

| # | 问题 | 修复 | 守卫 |
| --- | --- | --- | --- |
| ① | `/assets/share.js` 被下发 `immutable`，回访浏览器最长一年用旧副本 | `spa.go` 新增 `isContentHashed()`，按**名字形态**而非目录判定；`stableAssetFiles` 退化为断言目标 | `TestServesStableAssetUncached`、`TestStableAssetListMatchesBuild` |
| ② | `/llms.txt` 教 agent「向用户索要密钥」 | `aidoc/html.go` 的 `LLMSTxt()` 改指自助申请端点 | `TestLLMSTxtDoesNotSendAgentsToTheUserForAKey` |
| ③ | `views.ContentSecurityPolicy` 是死代码，真实 CSP 内联在 `router.go` | `router.go` 改为引用该常量 | `router_test.go` 断言响应头**全串等于**常量 |
| ④ | 仓库根 `public/` 是 CF 拓扑遗留（无引用） | 已删除 | `README.md`「缓存」节改写为按名字分类 |

关键决策：`decisions/2026-10-10-agent-key-identity-across-expiry.md`、
`decisions/2026-10-11-report-update-and-delete.md`、
`decisions/2026-10-11-one-step-key-handoff.md`。
踩坑记录：`pitfalls/cases/2026-10-10-renewal-unreachable-after-expiry.md`、
`pitfalls/cases/2026-10-11-update-delete-and-the-dead-cascade.md`、
`pitfalls/cases/2026-10-10-share-js-cached-immutable-for-a-year.md`、
`pitfalls/cases/2026-10-11-handoff-page-contract-drift.md`。

---

## 未做（按价值排序，都还没开工）

0. **给交接端点加限流**：领取端点无独立限流（256 位 secret 使暴力破解无意义，
   但「生成」端点也没有上限 —— 不过它是 Session 门禁的）。优先级低。
1. **反向代理日志里的 `X-Teleport-Claim`**：见上，已记录约束；若哪天要给该站点开访问日志，
   必须同时加过滤片段（runbook 里有现成的）。

1. **HTML 渲染的隔离档位**：打开 `views.HTMLReportSandbox` + `postMessage` 高度上报
2. **轮换 admin 口令**：线上仍是部署时生成的随机值，从未轮换过（运维动作，不是代码）
3. **密码保护分享链接**（链接语义要定：验证失败是 404 还是 401，未裁决）
4. **面板批量拒绝**密钥申请（`KEY_MAX_PENDING=50` 是唯一兜底，
   50 个申请即可堵住公开入口最长 24h；续期端点无独立限流，批量拒绝是缓解手段）
5. **登录限流**：`POST /api/admin/login` 无限流，PBKDF2 600000 轮在 2 vCPU 上是 CPU 耗尽面
6. **交接链接的独立有效期旋钮**：目前固定复用 `KEY_APPLICATION_TTL`（24h），
   要更长只能改 env（故意不给 UI 旋钮，避免又多一个要解释的东西）
7. **SQLite 定时备份**（目前只有部署前的手工备份）；`CleanupExpired` 未排期
8. **`SESSION_SECRET` 轮换与 agent 密钥的耦合**（轮换是否会让所有密钥失效？**未验证**）

**需要向使用者交代的契约行为**：具名密钥**读不到**迁移前 root 发布的报告
（`owner_key_id=''`，只有 root 与面板可见）。这是默认拒绝的正确方向，不是 bug。

---

## 长期约束（改动前必须知道）

- **markdown 报告永不渲染原始 HTML**：goldmark 不加 `WithUnsafe`，报告里的 `<script>`
  一律被替换为 `<!-- raw HTML omitted -->`。这条不因下面那条而放宽。
- **`format:"html"` 报告原样渲染**（2026-10-11 用户裁决）：正文由 `GET /s/{token}/html`
  逐字节返回，分享页用同源 iframe 套住，**不净化、不加 nonce**。分享页 CSP 只多了
  `frame-src 'self'`；子文档有独立 CSP。**绊线是双向的**：
  `aidoc/aidoc_test.go` 的 `TestHTMLCaveatMatchesImplementation` 断言 guide 的说法
  与 `views.HTMLReportsRenderVerbatim` 一致（任一方向漂移都会红）。
  **改这条前先改决策记录。**
- **`pnpm` only**；前缀只写一处（`ROUTE_PREFIX`）；Caddy 必须用 `handle` 不是 `handle_path`。
- **时间一律 epoch 毫秒**，`expires_at = 0` = 永不过期。
- **链接语义不许改**：未知 token 与已吊销都 **404**，仅过期 **410**。
- **响应封套不许改**：`{ok,data,requestId}`；请求体 camelCase、响应 snake_case
  （**这是最常见的 agent 错误**，所以 `PATCH` 对未知字段直接 400）。
- **归属是安全边界**：非归属者的读/改/删/撤销都是 **404 而不是 403**，
  且判定必须发生在**任何写入之前**。`owner_key_id` 永远不可更新。
- **默认严格，例外选择性开启**：漏维护时应落在「功能不可用」（可见的 401/400），
  绝不落在「权限静默放宽」。
- **限流桶键必须用 `httpx.ClientIP`，绝不用 `RealIP`**（`RealIP` 可被请求头伪造）。
- **密钥是派生的，不是暂存的**：`token = base64url(HMAC(K_derive, "teleport/agent-key/v1|appID|claimSecret"))`，
  `K_derive = HMAC(SessionSecret, "teleport/derive/agent-key/v1")`，服务端只存 `sha256(token)`。
  推导需要 `claimSecret`，所以**密钥在「领取」时才存在** ——
  **一把密钥的寿命从 agent 拿到它开始，不是从人类点批准开始**。
  **交接链接只是把「人类点批准」换成了「人类点生成」**，这条不变量没变。
- **claim secret 只出现在 URL fragment 里**：`/k/{id}#<secret>`，**裸值不带 `k=` 前缀**。
  服务端与分享页客户端各自独立解析这一个字符串，两处都有守卫
  （`TestKeyHandoffLinkCarriesTheSecretOnlyInTheFragment` + 反证 F9），
  **改形状前先看 pitfalls/cases/2026-10-11-handoff-page-contract-drift.md**。
- **续期只延长、绝不缩短**：`max(当前到期, now) + 批准时长`；`expires_at=0`（无界）保持 0。
  **「永不过期」只能由人类显式给出**：批准端字段缺省 → 24h；显式 `0` 才是永久。
  交接端同样：`expiresInHours` 缺省 → 24h，显式 `0` 才是永久。
- 密钥永不入库；`git commit`/`push`/部署前先问人类（本会话已获授权，继续沿用）。
- 不要提交 `bin/`、`backend/data/`、`backend/internal/webui/dist/`。

## 验收要点

- **必须从大陆观测点验证**：境外观测点无法检测大陆封锁。
  已知可用观测点：hcyj 本机（阿里云广州 `8.148.233.134`）、
  `bjbuwe`（广东电信 `121.9.113.26`）、`koma`（腾讯云 `124.221.144.97`）——
  三者在 `.ai/pitfalls/cases/2026-10-10-rate-limit-collapsed-to-one-bucket.md` 里有完整记录。
- **涉及前端改动时必须用真实 Chromium 打开页面核对**（类型检查通过 ≠ 界面能看）。
  **而且必须真点击（`Input.dispatchMouseEvent`）、必须看网络响应** ——
  「点一下就能领」这条链路上，`element.click()` 不产生 user activation，
  只看 DOM 文字会把「路径写错」误判成「已领取」。

## 反复踩到的坑（详见 `.ai/pitfalls/cases/`）

- **「代码写了」≠「代码生效了」**：触发器可能永不触发、外键可能从未强制、
  测试可能被无关规则挡住。这三件事都不报错，只让行为与声明悄悄分叉。
  每加一条声明式机制都要有**直接读底层状态**的守卫。
- **为了让断言稳定而加的 `time.Sleep`，常常是在移除被测条件**（曾因此漏掉一个真 bug）。
- **否定型断言必须排除「因为别的原因也返回同样的码」**（曾写出一个空测试）。
- **查前端产物别只看入口 chunk**：面板视图是懒加载的独立 chunk。
- **跨语言的字符串契约必须有「消费方视角」的测试**：生产方自测只证明它产出了自己以为的东西。

---

## 给人试用怎么说（用户要的「找人测试」）

这条功能**本身就是邀请方式**，所以「找人测试」= 生成一条交接链接发出去。
管理员侧只做一步：面板 → 密钥管理 → **「生成交接链接」** → 填个名字 → 复制链接转发。

**可以直接转发的话术**（链接替换成自己刚生成的那条）：

> 我给你开了个写报告的入口，点开这个链接，点一下「领取密钥」，
> 页面会给你一段提示词，直接复制粘贴给你的 AI 就行。密钥只显示这一次，记得先存下来。
> 有啥想问的直接问我。
> 链接：`https://api.hcyj.xyz/yeciorez/teleport/k/<id>#<secret>`

**对方拿到之后会发生什么**（可以照着讲）：点一下 → 拿到密钥 + 一段提示词 →
把提示词丢给 AI → AI 读 `…/ai.md` 学会接口 → 用密钥 `POST /api/reports` 发布 →
得到分享链接 → 发回给你看。

**适合找谁试**（按能立刻验证多少东西排序）：
1. **本来就有报告要给 AI 写的人** —— 一次就能跑通全链路（申请→发布→分享→看）。
2. **用 Claude Code / Cursor / 其他 agent 的人** —— 顺手验证「提示词能不能被别的 AI 读懂」，
   这是最可能出问题的一环（我们的提示词是写给 AI 看的，人读着可能觉得啰嗦）。
3. **完全不懂技术的人** —— 验证「点一下」是不是真的够傻瓜。

**试的时候请他们反馈三件事**（这三件是设计里最可能不合适的）：
1. 那条链接在**微信/QQ/飞书里点开**是否正常（聊天软件可能截断长链接；
   `/k/` 已兜底出说明页，但值得实测）。
2. 提示词贴给 AI 后，AI 是否**一次就懂**、有没有多问废话。
3. 报告发出去后，分享页在**手机上**看着如何（本轮没做移动端适配）。

**注意**：交接链接**只值一次**（领取即失效），默认 24 小时内有效；
密钥默认 24 小时过期（生成时可以选时长）。发错了就重新生成一条，
已发出的那条在领取前一直是有效的 —— 这一点在给别人之前值得想一秒。
