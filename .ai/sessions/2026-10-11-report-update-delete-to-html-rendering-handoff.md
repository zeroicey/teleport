# 报告更新/删除上线 → 准备做 HTML 渲染 — handoff

**Date:** 2026-10-11 · **Branch/Commit:** `main @ f6f878b`（已推送）· **线上:** `f609f8a`（二进制 `092d8715…`）

## Where we ended

两件事都已**上线并实测**：

1. **密钥过期后的身份连续性**（上一轮，`711f194`）：过期密钥有 90 天宽限，
   期间**只能**调 `POST /api/agent-keys/renewals`；续期不换 `key_id`，
   所以归属连续、旧报告不丢。
2. **报告的原地更新与删除**（本轮，`f609f8a`）：`PATCH`/`DELETE /api/reports/{id}`
   （agent，归属限定）与 `/api/admin/reports/{id}`（面板 god-view）。

本轮的核心认识：**这两件事都不是新设计，而是接通 schema 里早就写好、却从未生效的意图** ——
`reports.updated_at` + `trg_reports_touch_updated_at`（注释原文
"keep reports.updated_at honest without trusting the caller"，而"报告不可变"时它毫无意义）、
`share_tokens.report_id … ON DELETE CASCADE`、以及分享页**早就渲染**的「更新 / Updated」字段。

用户已定的下一件事：**HTML 渲染**（原话「下一部分我们做 HTML 渲染」）。

## What changed

| File / area | Change | Why |
| --- | --- | --- |
| `store/queries.go` | `UpdateReport`（COALESCE 局部更新）/ `DeleteReport`（级联） | 更新与删除的存储层 |
| `domain/types.go` | `ReportPatch` + `Provided()` | 每个字段是指针，"未提供" ≠ "设为空"；**没有** `OwnerKeyID` 字段 |
| `validate/validate.go` | `ParseUpdateReportInput` | 未知字段 400 / 空 patch 400 / metadata 整体替换 |
| `api/router.go` | 4 条新路由 + `applyReportPatch`/`applyReportDelete` | agent 与面板共用，只把"谁可以动"作为参数传入 |
| `api/helpers.go` | `parseUpdateReport` | 转发到 validate |
| `web/src/api.ts` | `updateReport` / `deleteReport` | 面板客户端 |
| `web/src/views/ReportDetailView.vue` | 编辑卡片 + 删除二次确认 | 确认框**点名**后果：「N 条链接（其中 M 条有效）会一并删除」 |
| `web/src/views/ReportsView.vue` | 「已更新」标记 | `updated_at` 现在会动了 |
| `aidoc/guide.md` `aidoc/html.go` `README.md` | 2.5.2 节 + 端点表 + llms.txt | agent 必须知道 PATCH 是严格的 |

`.ai/decisions/2026-10-11-report-update-and-delete.md`（D1–D6 裁决）
`.ai/pitfalls/cases/2026-10-11-update-delete-and-the-dead-cascade.md`（三个「写了但没生效」的坑）

## Verified / unverified

- [x] `pnpm run check` / `pnpm run backend:vet` / `go test ./... -race -count=1` 全绿
- [x] **6 条守卫逐条反证过**（归属检查挪到写入后 → 红；删未知字段检查 → 红；
      去掉 `foreign_keys(1)` → 红；放行空 patch → 红；改回朴素 `now` → 红）
- [x] 线上实测（真实密钥，测完数据已复原）：同毫秒连续更新严格递增、
      同一链接立刻展示新正文、非归属者双 404 且内容未改、未知字段 400 带白名单、
      删除后 token 1→0 且全库零孤儿、旧链接/读/改/再删/面板读全 404
- [x] 部署前后数据库计数一致（3 reports / 4 tokens / 1 key），`integrity_check` = ok，日志 0 error
- [x] 面板新 UI 确实在线上产物里：`ReportDetailView-2GGomxV1.js`（**懒加载 chunk，不在入口里**）
- [ ] **assumed, NOT tested：面板的编辑/删除 UI 从未在真实浏览器里点过。**
      只做了 `vue-tsc` 类型检查 + 构建产物字符串核对 + 后端接口实测。
      下一轮若动 `sharepage.go` 或面板，建议顺手用 Chromium 截图验一次。
- [ ] **assumed, NOT tested：并发更新同一报告的语义**（最后写入胜出，无乐观锁）。
      实现上没有版本号或 ETag，两个 PATCH 并发时后到的覆盖先到的。当前单人使用场景可接受。

## Loose ends

- [ ] 密码保护分享链接
- [ ] **HTML 渲染（下一轮的主题，见下）**
- [ ] 面板批量拒绝密钥申请 + 登录限流
- [ ] SQLite 定时备份（目前只有部署前的手工备份）
- [ ] `SESSION_SECRET` 轮换与 agent 密钥的耦合（轮换会让所有密钥失效吗？未验证）
- [ ] 报告**列表**里没有删除入口（删除只在详情页）——刻意的，那里能看到链接数

## Do not repeat

- **为了让断言"稳定"而加的 `time.Sleep`，常常是在移除被测条件。** 我在
  `TestUpdateReportIsInPlaceAndVisibleThroughTheExistingLink` 里加了 `time.Sleep(3ms)`
  以避免"同毫秒导致断言无意义"，结果**正好把 `updated_at` 倒流的 bug 藏起来了**。
  bug 是反证时读"故意搞坏"的失败输出发现的（响应体里 `updated_at=…3000` 比
  `created_at=…3897` 早 897ms）。教训：让**实现**保证不变量，测试不加 sleep。
- **不要用"响应字节相同"断言不可区分性。** 每个响应都带唯一 `requestId`，必然失败。
  要比语义形状（去掉 `requestId` 后逐字段比）。
- **否定型断言必须排除"因为别的原因也返回同样的码"。** 未知字段测试第一版每个 case
  只发一个坏字段，被"空 patch → 400"那条规则也拦住了，删掉未知字段检查它照样绿 ——
  **空测试**。改成每个 case 再带一个合法字段，并断言合法字段也未被应用。
- **查前端产物别只看入口 chunk。** 面板视图是懒加载的独立 chunk；只搜 `index-*.js`
  会得出"新 UI 没进产物"的错误结论（这次先误判了一次）。
- **`time.ParseDuration` 没有 `d` 单位**，90 天要写 `2160h`（`KEY_RENEWAL_GRACE`）。
- **部署前必须停服再拷数据库**：WAL 模式下热拷 `.db` 可能拿到不一致快照；
  该服务器**没有 sqlite3 CLI**，用 `python3` 的 `sqlite3` 模块。
- **`git push` 到 github 会偶发 SSL EOF / 连接超时**，重试 2–3 次即可（这次第 3 次成功）。

## Next step：HTML 渲染

### 先澄清"HTML 渲染"指哪一件事（两条路，成本差一个量级）

| | A. 渲染 `format: "html"` 的报告 | B. 渲染增强（markdown 报告的表现力） |
| --- | --- | --- |
| 含义 | 允许用户直接上报 HTML 并由分享页渲染 | 给现有 markdown 分享页加 TOC、代码复制按钮、打印样式 |
| 现状 | **完全未实现**，见下 | **完全未实现**，见下 |
| 主要成本 | 安全：净化器 + nonce CSP，**并且与仓库硬约定冲突** | 纯前端 + CSS，无安全面变化 |
| 建议 | 先做 B，A 单独决策 | ✅ |

**B 很可能才是用户想要的**：报告正文里已经列了「渲染增强：代码块复制按钮、
Mermaid 导出、TOC + 锚点、打印样式」作为待办，而用户说过「界面优化先不着急，
先把基础功能做好」——但 TOC/复制/打印属于"报告好不好用"，不是"面板好不好看"。

**问用户一句就够**：你要的是"能直接上报 HTML 并渲染"，还是"给现在的报告页加
目录/复制按钮/打印样式"？

### 现状（已核实，不必重新调研）

1. **`format: "html"` 的报告当前显示一句提示**：`sharepage.go` 的
   `bodyHTML := '<p class="notice">此报告为 HTML 格式，但 HTML 渲染尚未启用。</p>'`。
   ⚠️ 紧跟的 `else if HTMLMountEnabled { bodyHTML = '<p class="notice">HTML 渲染尚未启用。</p>' }`
   是**死代码** —— 它设的是同一句提示，而且 `HTMLMountEnabled` 恒为 `false`。
   真要开启时这段必须重写，不能"把 flag 改成 true"了事。
2. **仓库硬约定**（`AGENTS.md`）：**原始 HTML 不渲染。goldmark 不加 `WithUnsafe`；
   `views.HTMLMountEnabled` 保持 `false`。开启前必须先上 DOMPurify + nonce CSP。**
   `markdown` 包的包注释里也写了同样的安全模型（unsafe=false 会把 HTML 块替换成
   omitted 注释；危险 URL scheme 由 goldmark 拒绝）。
3. **`HTMLMountEnabled` 有一条既存绊线**：`aidoc/aidoc_test.go:135` 断言
   「guide 说 HTML 渲染未启用」与 flag 一致。**开启 flag 必须同一次改动里改 guide**，
   否则测试红 —— 这是设计好的，不是障碍。
4. **标题 id 已经存在，但对中文基本没用**（本轮实测，重要）：
   `markdown/render.go:89` 已有 `parser.WithAutoHeadingID()`。实测产出：
   ```
   # 架构总览        → id="heading"
   ## 一、前端       → id="heading-1"
   ## 一、前端       → id="heading-3"     ← 重复标题拿到 -3，无规律
   ## 1. 部署        → id="1-"
   ## API / 认证     → id="api--"
   ```
   线上真实报告（`/s/Du01mf3JsO9utq69xtKcBw`，39 个标题）产出 `id="0-"`、`id="1-"`、
   `id="21-"`、`id="41-ai-agent-"`、`id="teleport-"`。**纯中文标题退化成 `heading` /
   `heading-N`，序号取决于全文标题数，增删一节就会全部漂移。**
   → 做 TOC **必须自己生成稳定 id**（标题文本 → 自有 slug + 去重后缀），
   不能依赖 goldmark 的默认 id。这是本轮最重要的前置发现。
5. **分享页是 Go 服务端渲染**：`views/sharepage.go`（258 行，内联 `<style>`，
   **无 `@media print`**）。CSS 选择器现状只有：`.badge .chroma .code-block .expiry
   .eyebrow .mermaid .mermaid-error .mermaid-wrap .meta .notice .wrap`。
   线上实测分享页：`<nav>` 0 个、`class="toc"` 0 个、`<button>` 0 个、`<script>` 1 个。
   → TOC / 复制按钮 / 打印样式**都是全新**的。
6. **分享页客户端**只有 `web/src/share/main.ts`（111 行，只做 Mermaid 懒加载）。
7. **CSP 常量**在 `views/sharepage.go` 顶部：`default-src 'none'; script-src 'self';
   style-src 'self' 'unsafe-inline'; img-src 'self' data:; …`。
   `'unsafe-inline'` 只给了 style（Mermaid 运行时注入 `<style>`，无法预哈希）；
   **script 是 `'self'`，没有 `'unsafe-inline'`** → 想加内联脚本必须用 nonce 或走外部文件。
8. **新增稳定文件名资源要同步白名单**：`spa/spa.go` 的 `stableAssetFiles`
   目前只有 `assets/share.js`，并有 `TestStableAssetListMatchesBuild` 钉住。
   加 `print.css` 之类**非哈希名**资源必须同步该表（否则会被当哈希资源下发一年 immutable）。
   缓存判定看**名字形态**（`isContentHashed`），所以漏加是"缓存错"而不是"404"。
9. **DOMPurify 不在顶层依赖里**（只有 Mermaid 内部自带一份）。
   若要走 A 路线，需要显式加依赖并想清在哪一层净化：Go 侧（bluemonday 等）
   还是客户端。**服务端净化更合适**——分享页要能被无 JS 环境阅读。

### 建议的下一步（B 路线）

1. 先问用户那句澄清（A 还是 B），**不要默认开工**。
2. 若 B：给 `markdown.Renderer` 增加"标题 slug 化"——在渲染时给每个 heading 写
   `id`（基于标题文本的自有 slug + 稳定去重），因为默认 id 对中文不可用（第 4 点）。
   这是 TOC 与锚点跳转的共同基础，**先做它，再做 UI**。
3. 然后 TOC：服务端从标题树生成 `<nav class="toc">`（或客户端构建）。服务端更稳
   （无 JS 也能用），但要注意 `RenderResult` 需要多返回一份标题列表。
4. 复制按钮：`web/src/share/main.ts` 里加，注意 CSP `script-src 'self'` 下
   外部文件没问题；`clipboard.writeText` 需要安全上下文（生产是 https，OK）。
5. 打印样式：`sharepage.go` 内联 `<style>` 加 `@media print`（隐藏 TOC 的交互控件、
   去掉背景色、`page-break-inside: avoid` 给 `.code-block` / `.mermaid-wrap`）。
6. 同一次改动里：`aidoc/guide.md`（第 529 行附近讲 HTML 的那段）、`llms.txt`
   （`html.go:139` "HTML is stored but not rendered"）、`README.md`、
   以及 `.ai/decisions/` 一条新决策。
7. 验收：`pnpm run check` + `backend:vet` + `go test -race`；
   **用真实 Chromium 打开分享页截图核对**（本轮的 UI 就没在浏览器里验过，见上）；
   从大陆观测点 curl 线上分享页确认 TOC/锚点真的在。

### 起手命令

```bash
cd /data/dev/teleport
export PATH=$PATH:/usr/local/go/bin GOPROXY=https://goproxy.cn,direct
pnpm run check                      # 应先全绿
sed -n '55,70p' backend/internal/views/sharepage.go   # 那段死代码
sed -n '80,95p' backend/internal/markdown/render.go   # WithAutoHeadingID
```
