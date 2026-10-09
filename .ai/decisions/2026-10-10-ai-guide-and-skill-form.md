# 给 AI 的说明：服务端渲染文档 + 一个 Agent Skill 载体

**Status:** 🪦 REJECTED（选项 B 部分）· **Date:** 2026-10-10 · **Deciders:** user · **Supersedes:** none

> **裁决结果（2026-10-10，用户原话）**：
> 「你理解错了 算了 **我们只需要做教程吧**，你把教程做成方便 AI 获取就行了，**不要做 skill**。
> 关于 skill，我们就直接让 AI 读这个教程，然后**按照它自己的方式生成它能调用的 skill**。
> 你理解错了，不是生成我们当前项目的 skill。」
>
> 即：**一律选项 A**。本提案的 B（产出 `.agents/skills/teleport/SKILL.md` 载体）被否决，
> 理由不是成本，而是**方向错了** —— 目标不是"给本项目做一个 skill"，而是"把教程做得
> 足够好用，让**任意** agent 读完能自己生成**它自己**的 skill"。由我们代生成，
> 等于把某个特定 agent 的 skill 格式固化进本仓库，还多出一处必然漂移的第二事实源。
>
> 权威记录见 [`2026-10-10-ai-usage-guide-as-server-rendered-route.md`](2026-10-10-ai-usage-guide-as-server-rendered-route.md)（✅ ACCEPTED）。
> 本文件保留为"曾认真考虑过、并被否决"的证据，不删除、不改写推理。

## Context

用户原话：「现在我们都搞好了，还需要一个步骤：需要你单独写一个页面，也就是做一个给 AI
看的教程。我希望给 AI 一个链接，它就能知道当前这个网站怎么用了……目前主要不确定的是，
这个 skills 到底该怎么搞，还是说仅仅只做一个教程就可以了？」

要讲清四件事：网站怎么用、怎么调命令、需要什么密钥以及怎么用、怎么变成 skill 长期复用。

**硬约束（决定了实现形态）**：面板是客户端渲染的 SPA，agent 抓一个 URL 只会拿到空壳
`<div id="app">`。**任何 agent 必须能读到的内容都必须服务端渲染** —— 不能做成 Vue 路由。

另一条：说明文档**不能做成分享链接**。分享链接会过期，一个被 agent 缓存过的说明迟早
410，正好与「长期可引用」的目的相反。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. 只做服务端渲染文档（`/ai`、`/ai.md`、`/llms.txt`） | 任何 agent 一个 URL 就能读；无状态、无过期 | agent 每次都要先抓一次才知道怎么用 | easy |
| B. A + Agent Skill 载体（`.agents/skills/`） | 支持 skill 的 agent 免抓取，直接按触发条件加载 | 需要用户把 skill 装到 agent 里；技能内容会与文档漂移；**且把特定 agent 的格式固化进仓库** | easy |
| C. 只做 skill，不做文档 | 支持 skill 的 agent 最省事 | 不支持 skill 的 agent 完全用不了 | easy |
| D. 教程写成一篇分享链接报告 | 最快 | **会过期**，与目的相反 | easy |

## Decision

**选 A**（用户裁决，见文首）。文档是"任何 agent 都能用"的地板；**不代产 skill** ——
把"如何据此生成 skill"作为文档的一节（第 8 节）写给 agent 自己。

**已落地的部分**：`internal/aidoc` 包 + 服务端渲染的 `/ai`（无脚本 HTML 页）、
`/ai.md`（`text/markdown`，agent 直接读源文）、`/llms.txt`。
文档存在 `guide.md` 里，`{{APP_BASE}}` 等占位符由 **live Config 在请求时替换** ——
所以改了部署前缀或主机名，文档不会继续描述旧地址；未识别的 `{{...}}`
由 `TestNoUnresolvedPlaceholders` 在构建期直接失败。

**不做**：`.agents/skills/teleport/SKILL.md`（以及任何形式的 skill 载体）。

## Consequences

- 文档中明确「不要尝试猜测或爆破 `AGENT_KEY`，也不要调 `/api/admin/*`」；并说明
  **面板的 Session Cookie 不能用来调 `/api/reports`**（那条路径只认 Bearer）。
- `/ai` 页 CSP 比分享页更严：`default-src 'none'` 且**没有 `script-src`**，所以未来
  即使误注入 `<script>` 也会被拦。
- **不存在第二处事实源**：没有 skill 需要与 `guide.md` 同步，文档第 8 节只给"生成 skill
  所需的要点"，由 agent 按自身环境落地。这是选 A 最被低估的收益。
- 代价：每次用都得先抓一次 `/ai.md`。用户接受这个代价（文档很小，11 KB）。

## Revisit when

用户给出裁决；或 agent 反馈「说明不够用/不好用」；或 skill 与 `/ai.md` 出现事实分歧。

