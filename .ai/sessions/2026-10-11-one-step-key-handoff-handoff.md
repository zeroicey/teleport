# 一键交接链接（管理员一步发密钥）— handoff

**Date:** 2026-10-11 · **Branch/Commit:** `main @ 60bcbe8` · **线上:** `60bcbe8`（已部署并实测）

## Where we ended

用户上一轮选 A（`format:"html"` 原样渲染）并已部署（`adc2be3`）。本轮用户提出新需求，
原话：

> 「我直接用管理员账号去申请一个密钥，然后把带密钥的链接（或者一段提示词，告诉他去哪里看怎么发布）
> 直接发给他。他拿到链接后直接就能去弄报告，不需要我再去平台上操作确认，他就可以直接完成所有内容了。
> 反正你看一下怎么做比较好，简单一点就行。」

设计权交给实现方，要求**简单**。同时用户给了标准授权：

> 「如果你自己测试成功没问题的话，就该提交提交、该部署部署、该推送推送，找一些人来测试一下。」

即：自测通过 → 提交 + 部署 + **push**（上一轮 push 因 HTTPS 黑洞改为 SSH remote）+ 找人试用。

**做完了**：代码 + 文档 + 记忆库 + 反证 10 条 + **真 Chromium 端到端 25/25**。
线上部署与「找人测试」见 CURRENT_TASK 的下一步。

## What changed

| File / area | Change | Why |
| --- | --- | --- |
| `store/keys.go` | 新增 `HandoffInput` + `CreateHandoffApplication`：**直接插一行 `approved`**（`decided_at=now`、`expires_at=claim_deadline=now+LinkTTL`），审批元数据停在 `approved_*` | 复用「申请」这张表，不新增 schema；**跳过 pending 阶段**，所以不占 `KEY_MAX_PENDING` 队列 |
| `api/keys.go` | `handleCreateKeyHandoff`（Session 门禁）、`handoffURL`、`handoffPrompt` | 生成链接与「可直接粘贴给 AI 的提示词」 |
| `api/router.go` | `POST /api/admin/key-handoffs`、`GET /k/{id}`、**`GET /k/`**、`handleKeyHandoffPage`、`humanizeHours` | `/k/` 单挂一条：链接被聊天软件截断时也要出**说明页**而不是掉进 SPA 兜底 |
| `views/handoff.go`（新） | 独立页面，四态 `open/claimed/expired/unknown`；`no-store`、`noindex`、`referrer: no-referrer`、内联 CSS；`data-handoff-id`、`data-app-base`、`data-handoff-spent`；按钮**默认 `hidden`** | 无 JS 时页面是**惰性**的（预览机器人烧不掉链接） |
| `web/src/share/main.ts` | `setupKeyHandoff()`：读 fragment → 有 `sessionStorage` 缓存就用缓存 → 否则真点击才 `X-Teleport-Claim` 领取 → `history.replaceState` 抹掉 fragment → 本地拼出提示词 | 领取是**客户端 + 点击触发**：fragment 从不发给服务器，机器人也不执行 JS |
| `web/src/api.ts` `web/src/views/KeyManagementView.vue` | `createKeyHandoff` + 「生成交接链接」两态弹窗（表单 → 结果：复制链接 / 复制提示词 + 「# 后面是密码」警示） | 管理员一步拿到可转发的东西 |
| `spa/spa.go` | 兜底判定加 `k/`（`api/ || s/ || k/ || rel == "k"`） | `/k/...` 未命中时给 JSON/404，**不能**吐 SPA 外壳 |
| `aidoc/guide.md` `aidoc/html.go` `README.md` | 新增 2.7 节（链接 vs 提示词、`HANDOFF_CLAIM` 领取配方、「只值一次」）+ llms.txt 指针 + 路由表 | agent 侧也要能读懂「有人给了我一条交接链接」 |
| `.ai/decisions/2026-10-11-one-step-key-handoff.md`（新） | D1–D11 + 被否决的方案 A（链接里直接放真密钥）+ 回滚说明 | 决策入库 |
| `.ai/pitfalls/cases/2026-10-11-handoff-page-contract-drift.md`（新） | 两个 bug 的根因与防线 | 见下 |
| `bin/e2e/verify-handoff.cjs`（gitignored） | 真 Chromium 端到端 25 条 | 「点一下就能领」只能这样验 |
| `bin/e2e/falsify-handoff.py` | 反证 **10** 条（新增 F9/F10） | 每条都能让对应测试变红，还原后逐字节一致 |

## 两个只有真浏览器能抓到的 bug（本轮最大收获）

Go 测试全绿、HTTP 层全对，但真实 Chromium 里功能**完全不能用**：

1. **fragment 形状漂移**：服务端产出 `#<secret>`，客户端按 `#k=<secret>` 解析
   → 判定「链接不完整」→ **领取按钮永不出现**。
2. **base URL 漂移**：页面属性传的是 API 根（`…/api`），客户端自己再拼 `/api/...`
   → `/api/api/agent-keys/applications/{id}` **404**，而页面把 404 折叠成
   「这条链接已经领取过了」——**把 bug 报告成了功能**，会把人引向完全错误的动作。

修法：客户端以裸 fragment 为准（兼容 `k=` 前缀）；`APIBaseURL` 改名 `AppBaseURL`
（**名字就是契约**，叫 APIBase 就会有人塞 `/api` 进去）；两条 Go 守卫 + 反证 F9/F10；
验证脚本**监听网络响应**确认没有 `/api/api/`。

详见 `.ai/pitfalls/cases/2026-10-11-handoff-page-contract-drift.md`。

## Verification performed

- `pnpm run check` EXIT=0（typecheck + schema 同步 + Go 测试）、`go vet ./...` 干净、
  `go test -race` 三个包全过。
- **真 Chromium 25/25**（`bin/e2e/verify-handoff.cjs`，Chrome for Testing 156，headless）：
  链接 fragment 形状 → 页面渲染收件人名 → 按钮在 fragment 存在时才出现 → 真点击领到 43 字符密钥
  → fragment 被抹掉 → 提示词里含该密钥 → 同标签刷新仍显示（sessionStorage）→
  **换浏览器上下文显示已领取** → 用这把密钥 `POST /api/reports` **201** →
  面板登录 → 「生成交接链接」→ 出链接 + 提示词 → **真点击复制**按钮变「✓ 已复制链接」→
  面板生成的链接立刻可领 → **密钥行数在生成前后不变**（`1 -> 1`，证明领取前不建密钥）。
- **反证 10 条全红**（`bin/e2e/falsify-handoff.py`）：secret 移出 fragment / 页面变可缓存 /
  按钮默认可见 / 回到 pending 队列 / 端点丢 Session 门禁 / `/k/` 掉进 SPA /
  guide 删整节 / fragment 变 `k=v` / 传 API 根 / 生成时就建密钥行 —— 每条都能让测试变红。

**环境坑（值得记）**：本机每次 bash 调用是独立 PID namespace，**看不到也杀不掉**上一次留下的
Chrome；Chrome 的 profile 是**单例**，第二个实例会静默交接给第一个然后退出（调试端口永不打开）。
所以验证脚本改成**每次跑随机端口 + 每端口独立 profile**。另外 headless 页面 `document.hasFocus()`
为 false，Clipboard API 会合法拒绝 —— 要 `Emulation.setFocusEmulationEnabled`，
且**合成点击没有 user activation**，必须 `Input.dispatchMouseEvent` 才是真实路径。

## Deployment and online verification (done)

- 构建 `60bcbe8`，sha256 `fa224ae1…`；`scp` → `mv` 原子替换 → `systemctl restart`；
  回滚材料 `/data/services/teleport/teleport.prev-20261011-152828` +
  冷备 `teleport.db.bak-20261011-152828`。部署窗口 `error|panic|fatal` = **0**。
- **生产端到端 29/29**（`bin/e2e/verify-handoff-online.py` 在 hcyj 上跑，走**公网 URL**）：
  创建 → 匿名读页（fragment 不进服务器、页面无 secret、`data-app-base` 正确、`no-store`、
  非 SPA 外壳）→ 领取 200 → **重放 404** → 页面翻 `spent="1"` → 未知 id 同样说明 →
  发布报告 201 → 分享可读 → 复原。
- **大陆观测点（koma）**：`/api/health` 200；`/k/`、`/k/{uuid}`、`/k/{uuid}/extra` 全 200 且
  `id="app"` 0 次；缓存头三连查通过。
- **生产已复原到部署前状态**：3 reports / 4 share_tokens / 1 agent_key / 1 application，
  `integrity_check` = ok。验证过程中产生的密钥与申请行已清除
  （`bin/e2e/verify-handoff-online.py` 会按 label 清扫自己，包括**上一次崩溃运行**留下的残留）。

## Next session: start here

把交接链接发给真人试用 —— 这是用户要的「找人测试」，也是这条功能的验收方式。
话术与「请对方反馈哪三件事」写在 `.ai/CURRENT_TASK.md` 末尾「给人试用怎么说」。
