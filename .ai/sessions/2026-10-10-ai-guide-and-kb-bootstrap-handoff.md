# AI 使用说明 + 知识库初始化 — handoff

**Date:** 2026-10-10 · **Branch/Commit:** `main` @ `6ca3b24`（工作区有未提交改动） · **Duration:** 跨 3 个会话

## Where we ended

项目已从 Cloudflare 全家桶迁到 hcyj 单机单二进制（Go + SQLite + 内嵌 Vue），生产可用。
「给 AI 的使用说明」**已完成并上线验证**：`internal/aidoc` 包 + `/ai`（无脚本 HTML）、
`/ai.md`（`text/markdown`）、`/llms.txt`，路由已接线并有 4 条路由级测试，
线上 `83d9e8bc…` 三端点全 200。skill 形态**已裁决：只做教程，不代产 skill 包**。

本次同时用 `engram` 初始化了本项目自己的知识库（`.ai/`、`AGENTS.md`、`.agents/skills/`、
`.engram/config.json`，工具集只选 `dsh`），并把前几个会话的历史结论固化进
decisions / pitfalls / runbooks。

本次同时用 `engram` 初始化了本项目自己的知识库（`.ai/`、`AGENTS.md`、`.agents/skills/`、
`.engram/config.json`，工具集只选 `dsh`），并把前几个会话的历史结论固化进
decisions / pitfalls / runbooks。

## What changed

| File / area | Change | Why |
| --- | --- | --- |
| `.ai/ARCHITECTURE.md` | 从「单文件模板」改成真实架构（栈、边界、10 条不变量、已知债） | 原文件是 engram 骨架占位，全是注释 |
| `.ai/CURRENT_TASK.md` | 写入当前目标、清单、阻塞、下一步 | 每次会话开始读的第一个文件 |
| `.ai/decisions/` | 7 篇：CF 全站(🪦)、CF Worker 反代(🪦)、大陆可达性单入口(✅)、Go+SQLite(✅)、单二进制(✅)、AI 说明形态(🪦，被并行会话裁决)、AI 说明服务端渲染(✅，并行会话) | 把「为什么变成现在这样」固化，避免重新论证 |
| `.ai/pitfalls/cases/` | 11 篇：CF 大陆封锁、docker exec /tmp、handle_path、空导入、vite base、SPA 吞 404、goldmark SetRenderer、env `$` 展开、鉴权漏挂、第三方 Cookie、公开文档泄漏密钥路径(并行会话) | 每个都是排查过并解开的失败模式 |
| `.ai/runbooks/` | `deploy-teleport.md`、`caddy-routing-on-shared-host.md` | 部署与改 Caddy 是高风险、会重复发生的操作 |
| `AGENTS.md` | 从 engram 占位块换成真实项目说明（命令、目录、硬约定、完成定义） | dsh 会把它加载进每次会话 |
| `.ai/`、`.agents/`、`.engram/` | 新增（engram 初始化产物） | 项目自身记忆，跨工具 |

## Verified / unverified

- [x] 迁移后的架构与线上行为：README「已验证行为」有生产实测表 + 真实浏览器表
- [x] `aidoc` 单测通过：占位符全部替换、无未使用占位符、文档随配置变化、不泄漏密钥
- [x] 大陆封锁结论：两个大陆观测点 + 换 IP 对照实验（决定性证据）
- [x] `pnpm run check` 全绿（Go 测试含 `-race`）
- [x] **`/ai`、`/ai.md`、`/llms.txt` 接线完成**（受 `router_test.go` 四条路由级测试保护），
      线上 `83d9e8bc…` 已验证：`/ai` 200（CSP `default-src 'none'`、0 个 `<script>`）、
      `/ai.md` 200（`text/markdown`）、`/llms.txt` 200
- [x] 新路由的**大陆可达性**：hcyj 本机 + 家宽双观测点，三端点全 200
- [x] skill 形态**已裁决**：只做教程，不代产 skill 包 —— 见
      `.ai/decisions/2026-10-10-ai-guide-and-skill-form.md`（🪦 REJECTED）与
      `.ai/decisions/2026-10-10-ai-usage-guide-as-server-rendered-route.md`（✅ ACCEPTED）

## Loose ends

- [x] `router.go` / `router_test.go` 接线完成（**由并行会话完成**，见下方「并行写入冲突」）
- [x] 大陆观测点验收三端点（同上）
- [x] 用户裁决 skill 形态（同上）
- [ ] README 里的「已知陷阱」7 条与 `.ai/pitfalls/cases/` 内容重叠，可考虑互相引用而不是各写一份
- [ ] 未做：`format:'html'`、`CleanupExpired` 定时任务、删除报告 API、SQLite 自动备份

## 并行写入冲突（重要）

本仓库当时**同时有两个 dsh 会话**在 `main` 上工作：一个做 `backend/internal/aidoc` 与
`internal/api/router.go`，一个做 engram/dsh 初始化与知识库。两者都按 `.ai/` 契约写回，
因此在 `.ai/` 下发生了真实写入冲突（同一文件被两次覆盖，`CURRENT_TASK.md` 与
`2026-10-10-ai-guide-and-skill-form.md` 都被改写）。

处理方式：**不回滚对方的改动，只做事实对齐** —— 保留更接近现实的那一版
（`CURRENT_TASK.md` 取并行会话的版本，因为它的路由/线上验证数据更新），
被覆盖的提案保留为 🪦 REJECTED 并指向权威决策。

教训：`.ai/` 是**共享可写面**，不是私有暂存区。多个 agent 同时写时，
`CURRENT_TASK.md` 这类"单一活动面"文件必须先看再写，且写回内容要能吸收对方的更新
而不是覆盖它。

## Do not repeat

- **用境外观测点判断大陆可达性。** 曾用 Azure 香港得到 300/300 全通，据此误判为
  「本地运营商偶发抖动」，浪费一轮排查。必须用大陆观测点且多点交叉。
- **用 `docker exec caddy caddy validate --config /tmp/x` 校验新配置。** 它校验的是
  容器内的旧文件，会返回 `Valid configuration` 而实际没生效。必须 `docker cp` + 比对
  md5 + `caddy adapt | grep` 断言新路由存在。
- **把 CF「优选 IP」当成大陆可达的解。** 它要求 DNS 移出 CF，Worker 随即失效，方案自毁。
- **`scp` 直接覆盖正在运行的二进制。** 用 `.new` + `mv` 原子替换。
- **`set -a; . ./env` 加载含 `$` 的密钥。** PBKDF2 哈希会被 shell 展开成垃圾。
- **`goldmark` 用 `SetRenderer` 注册自定义渲染器。** 会静默丢掉 Table/Strikethrough/
  Linkify 扩展。
- **删掉 `store.go` 里 `_ "modernc.org/sqlite"`。** 编译、vet 都过，运行时才
  `unknown driver`。
- **改前端后忘记 `pnpm run build`。** `//go:embed` 会打进旧产物。

## Next step

1. `pnpm run check` 复跑一次（两个会话的改动合并后确认闸门仍绿）。
2. 按 `CURRENT_TASK.md` 的 Next action 走：轮换 admin 口令（仍是部署时的随机值）。
3. 用户的**后续方向**（未开工）：agent 自助申请密钥 —— agent 向服务端申请，
   用户在网页上批准后颁发有有效期/可续期的密钥。改动点见
   `.ai/decisions/2026-10-10-ai-usage-guide-as-server-rendered-route.md` 的 "Revisit when"。
