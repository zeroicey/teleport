# Current task

**Status:** 🟢 done · **Updated:** 2026-10-10

## Goal

给 AI 一份可长期引用的使用说明：agent 拿到一个链接就能独立发布报告、取得分享链接，
并能据此生成**它自己**的 skill。按用户裁决：**不做**本项目的 skill 包，只做教程。

## Checklist

- [x] 从 Cloudflare 全家桶迁移到国内服务器单二进制（Go + SQLite + 内嵌 Vue）
- [x] 拒绝将 CF 免费版 IP 作为大陆入口（实测被 TCP 层封锁），前端一并迁到 hcyj
- [x] `aidoc` 包 + `guide.md`（占位符由 live Config 替换，不是写死的字符串）
- [x] 服务端渲染的 `/ai`（无脚本页）与 `/ai.md`（纯 Markdown）、`/llms.txt`
- [x] `aidoc` 单测（16 条）：占位符全部替换、无未用占位符、文档随配置变化、不泄漏密钥
- [x] `router.go` / `router_test.go` 的 aidoc 路由接线（4 条路由级测试）
- [x] 大陆观测点验收新路由（hcyj 本机 + 家宽，三端点全 200）
- [x] 照抄文档第 7 节配方跑通端到端（发布 → 取链接 → 自检 200 → 清理）
- [x] 用 `engram` 初始化本项目记忆库，工具集只选 `dsh`（`AGENTS.md` + `.agents/skills/`）
- [x] 从历史会话固化知识库：6 篇历史决策、11 条陷阱、2 份 runbook、架构与 handoff

## Code state

分支 `main` @ `868309d`（**已提交并推送**）。AI 说明页与 engram 初始化在两次提交中：
`868309d` 为 aidoc 代码 + 路由 + README（已上线）。
新增 `backend/internal/aidoc/`：`aidoc.go`（占位符替换）、`html.go`（HTML 页 + `llms.txt` +
CSP）、`guide.md`（正文）、`aidoc_test.go` 与 `html_test.go`。
`backend/internal/api/router.go` 挂三个 `GET` 路由 + `router_test.go` 四条测试。

新增 engram 产物（本项目自用）：`AGENTS.md`、`.agents/skills/<name>/SKILL.md`（4 条，与
`.ai/skills/` 同源生成）、`.engram/config.json`（`tools: ["dsh"]`）、`.ai/` 知识库。

线上 `9e1b2c0a…`（`version` = `868309d`，`--frontend` = `embedded`）已验证：
`/ai` 200（23 KB，CSP `default-src 'none'`，0 个 `<script>`）、
`/ai.md` 200（`text/markdown`，11 KB）、`/llms.txt` 200（858 B）。
生产库为 1 报告 / 2 分享（架构报告及其链接），验证数据已清理。

## Blockers

- none

## Pitfall reminders for the current branch

- `.ai/pitfalls/cases/2026-10-10-public-doc-disclosed-secret-file-path.md` — 公开文档
  不许写出服务器上密钥的存放路径。
- `.ai/pitfalls/cases/spa-fallback-swallows-api-404.md` — 未知 `/api/*` 必须仍是 JSON。
- `.ai/pitfalls/cases/env-file-dollar-expansion.md` — 用 shell source env 会吃掉哈希里的 `$`。
- `.ai/pitfalls/cases/goldmark-setrenderer-drops-extensions.md` — `SetRenderer` 静默丢扩展。
- **`.ai/` 是共享可写面**：多个 agent 同时写时，先读 `CURRENT_TASK.md` 再写，别覆盖对方的更新
  （2026-10-10 实际发生过，见 `.ai/sessions/2026-10-10-ai-guide-and-kb-bootstrap-handoff.md`）。

## Next action

用户已确认收尾。可选的下一步：

1. 轮换 admin 口令（仍是部署时的随机值）：
   `ssh hcyj '/data/services/teleport/teleport hash-password "新口令"'` → 改
   `teleport.env` → `systemctl restart teleport`。
2. 用户提到的**后续方向**（未开工）：agent 自助申请密钥 —— agent 向服务端申请，用户在
   网页上批准后颁发**有有效期、可设永不过期、也可续期**的密钥，并有一个统一管理面板。
   届时的改动点见 `.ai/decisions/2026-10-10-ai-usage-guide-as-server-rendered-route.md`
   的 "Revisit when"（文档第 2 节要从"向用户索取"改为"如何申请"）。
