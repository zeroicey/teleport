# Current task

**Status:** ✅ **`format:"html"` 原样渲染已上线并线上实测** · 工作区干净 · **线上 `adc2be3`** ·
**Updated:** 2026-10-11
**下一步：** 用户要**先自己发几篇 html 报告试手感**，有反馈再排下一轮（B 路线 TOC/复制/打印 仍未做）

> 本文件是**当前状态**，不是流水账。每一轮的完整叙事在
> `.ai/sessions/*-handoff.md` 与 `.ai/decisions/`、`.ai/pitfalls/` 里。

---

## 本轮：HTML 渲染（用户选了 A，并明确豁免安全顾虑）

用户原话：「就是 ai 自己写 HTML 的代码以及样式 JS 逻辑 就不用太考虑那些安全问题
毕竟我都是给自己用」。裁决与代价见
**`.ai/decisions/2026-10-11-html-report-rendering.md`**（✅ ACCEPTED），
踩到的三个「默认值挡路」见
**`.ai/pitfalls/cases/2026-10-11-framed-report-blocked-by-defaults.md`**。

**做法**：分享页对 `format:"html"` 的报告放一个同源 `<iframe class="report-frame">`，
`src` 指向**新端点** `GET /s/{token}/html`；该端点把 `report.content` **逐字节**返回，
不净化、不加 nonce。子文档有独立 CSP（`views.HTMLReportContentSecurityPolicy`）；
分享页 CSP 只加了 `frame-src 'self'`，`script-src` 仍是 `'self'`。
`views.HTMLMountEnabled` 已删除，换成 `views.HTMLReportsRenderVerbatim = true`。

**已知并接受的代价**：报告里的 JS 与面板同源 —— 读不到 HttpOnly 的 `teleport_session`，
但能带凭据请求 `/api/admin/*`（以查看者身份）。想隔离就设
`views.HTMLReportSandbox`，但必须同时把高度测量从「同源读 `contentDocument`」
换成 `postMessage`（沙箱 iframe 是 opaque origin）。

**线上状态（2026-10-11 11:38 部署，`adc2be3`）**：

| 项 | 值 |
| --- | --- |
| 二进制 sha256 | `bd60a4a1c3aa7a6418c7bdd100ce4ab1141d40b21d913079d970fef83300e8f1`（本地=线上） |
| 回滚二进制 | `/data/services/teleport/teleport.prev-20261011-113817`（= 旧 `f609f8a`） |
| 数据库备份 | `/data/services/teleport/teleport.db.bak-20261011-113817` |
| 库内容 | 3 reports / 4 share_tokens / 1 agent_key，`integrity_check` = ok，零孤儿 |

**线上实测（大陆观测点 koma 腾讯云 + 本机真 Chromium）**：
- 线上发了一篇 html 测试报告，正文 sha256 与本地 payload **跨线一致**（`4035aecd…`）；
  Chromium 实测 iframe 被撑到 `379px`、子页三项 JS 自检全绿
  （截图 `bin/e2e/shot-online-html.png`：深色底 48429 采样、绿 ✓ 371、红 ✗ 0）
- 分享页响应头 `frame-src 'self'` + `XFO: DENY`；子资源**无 XFO**、独立 CSP；
  正文端点 200 且 `content-type: text/html`
- markdown 回归：既有报告 `QDE56Dvt4kRsvCQjyJ6DRA` 线上无 iframe、Mermaid 出 `<svg>`、
  `raw HTML omitted` 仍在；koma 视角 200
- 部署后 10 分钟日志 `error|panic|fatal` = **0**
- 测试数据已复原：删除测试报告 → 分享页与正文端点双双 **404**，计数回到 3/4/1

**本地已验证（真 Chromium，非仅类型检查）**：
- `GET /s/{token}/html` 正文与提交字节**完全一致**（1845 B）；markdown 报告在该路径 **404**
- 分享页响应头 `frame-src 'self'` + `X-Frame-Options: DENY`；子文档**无** `X-Frame-Options`
- Chromium 实测：iframe 高度被 `share.js` 写成 `379px`（证明同源测量生效）；
  子页内联脚本、同源 `fetch`、`localStorage` 三项全部成功（截图里三项均为绿色 ✓、红色 0）
- markdown 回归：无 iframe、Mermaid 仍渲染出 `<svg>`、`<script>` 仍被替换为 `raw HTML omitted`
- 反证 **8 条**：删 `h.Del("X-Frame-Options")` / 转义正文 / 给子资源记一次浏览 /
  去掉 `frame-src` / 放行 markdown 走该端点 / 默认打开 `HTMLReportSandbox` /
  aidoc 绊线的**正反两个方向** —— **每条都能让对应测试变红**

**未做**：B 路线（TOC / 复制按钮 / 打印样式）**没做**，用户选的是 A。

---

## 下一步（唯一一件）

等用户试用手感，**当前无代码工作待办**。候选（按之前讨论）：B 路线 TOC/代码复制/打印样式、
隔离档位、密码保护链接、登录限流。

> 想试手感就直接 `POST /api/reports` 带 `"format":"html"` + 一段完整 HTML 页面，
> 拿 `data.share.url` 打开即可；分享页底部有「在新窗口打开原页面」用于全屏看。

---

## 线上状态

| 项 | 值 |
| --- | --- |
| 版本 | `f609f8a`（`frontend: embedded`） |
| 二进制 sha256 | `092d871545726b09c171f36b46271e092c7dc35575939c50d96b078c2e263fde` |
| 回滚二进制 | `/data/services/teleport/teleport.prev-20261011-010239` |
| 数据库备份 | `/data/services/teleport/teleport.db.bak-20261011-010239` |
| 库内容 | 3 reports / 4 share_tokens / 1 agent_key，`integrity_check` = ok |

部署流程见 `.ai/runbooks/deploy-teleport.md`（含 4.1 缓存头三连查、4.2 **停服后**再拷数据库）。
**注意「线上 version ≠ main HEAD」是正常的**：部署后只改文档/记忆库的提交不必重发二进制；
但只要改了 Go 或 `web/src/`，**必须** `pnpm run build:release` 并重新部署（前端 `//go:embed` 进二进制）。

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

**审计四条**（追加②，全部已部署，守卫名在代码里）：

| # | 问题 | 修复 | 守卫 |
| --- | --- | --- | --- |
| ① | `/assets/share.js` 被下发 `immutable`，回访浏览器最长一年用旧副本 | `spa.go` 新增 `isContentHashed()`，按**名字形态**而非目录判定；`stableAssetFiles` 退化为断言目标 | `TestServesStableAssetUncached`、`TestStableAssetListMatchesBuild` |
| ② | `/llms.txt` 教 agent「向用户索要密钥」 | `aidoc/html.go` 的 `LLMSTxt()` 改指自助申请端点 | `TestLLMSTxtDoesNotSendAgentsToTheUserForAKey` |
| ③ | `views.ContentSecurityPolicy` 是死代码，真实 CSP 内联在 `router.go` | `router.go` 改为引用该常量 | `router_test.go` 断言响应头**全串等于**常量 |
| ④ | 仓库根 `public/` 是 CF 拓扑遗留（无引用） | 已删除 | `README.md`「缓存」节改写为按名字分类 |

关键决策：`decisions/2026-10-10-agent-key-identity-across-expiry.md`、
`decisions/2026-10-11-report-update-and-delete.md`。
踩坑记录：`pitfalls/cases/2026-10-10-renewal-unreachable-after-expiry.md`、
`pitfalls/cases/2026-10-11-update-delete-and-the-dead-cascade.md`、
`pitfalls/cases/2026-10-10-share-js-cached-immutable-for-a-year.md`。

---

## 未做（按价值排序，都还没开工）

1. ~~HTML 渲染~~ —— **A 路线本轮已完成（未提交/未部署）**；B 路线（TOC/复制/打印）**仍未做**
2. **HTML 渲染的隔离档位**：打开 `views.HTMLReportSandbox` + `postMessage` 高度上报
3. **轮换 admin 口令**：线上仍是部署时生成的随机值，从未轮换过（运维动作，不是代码）
4. **密码保护分享链接**（链接语义要定：验证失败是 404 还是 401，未裁决）
5. **面板批量拒绝**密钥申请（`KEY_MAX_PENDING=50` 是唯一兜底，
   50 个申请即可堵住公开入口最长 24h；续期端点无独立限流，批量拒绝是缓解手段）
6. **登录限流**：`POST /api/admin/login` 无限流，PBKDF2 600000 轮在 2 vCPU 上是 CPU 耗尽面
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
- **续期只延长、绝不缩短**：`max(当前到期, now) + 批准时长`；`expires_at=0`（无界）保持 0。
  **「永不过期」只能由人类显式给出**：批准端字段缺省 → 24h；显式 `0` 才是永久。
- 密钥永不入库；`git commit`/`push`/部署前先问人类（本会话已获授权，继续沿用）。
- 不要提交 `bin/`、`backend/data/`、`backend/internal/webui/dist/`。

## 验收要点

- **必须从大陆观测点验证**：境外观测点无法检测大陆封锁。
  已知可用观测点：hcyj 本机（阿里云广州 `8.148.233.134`）、
  `bjbuwe`（广东电信 `121.9.113.26`）、`koma`（腾讯云 `124.221.144.97`）——
  三者在 `.ai/pitfalls/cases/2026-10-10-rate-limit-collapsed-to-one-bucket.md` 里有完整记录。
- **涉及前端改动时必须用真实 Chromium 打开分享页核对**（类型检查通过 ≠ 界面能看）。

## 反复踩到的坑（详见 `.ai/pitfalls/cases/`）

- **「代码写了」≠「代码生效了」**：触发器可能永不触发、外键可能从未强制、
  测试可能被无关规则挡住。这三件事都不报错，只让行为与声明悄悄分叉。
  每加一条声明式机制都要有**直接读底层状态**的守卫。
- **为了让断言稳定而加的 `time.Sleep`，常常是在移除被测条件**（曾因此漏掉一个真 bug）。
- **否定型断言必须排除「因为别的原因也返回同样的码」**（曾写出一个空测试）。
- **查前端产物别只看入口 chunk**：面板视图是懒加载的独立 chunk。
