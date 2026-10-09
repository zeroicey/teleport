# AI 使用说明作为服务端渲染的稳定路由，而不是 SPA 页面或分享链接

**Status:** ✅ ACCEPTED · **Date:** 2026-10-10 · **Deciders:** 用户 + AI · **Supersedes:** none

## Context

需求：给 AI 一个链接，它读完就知道这个站点怎么用，并能**据此生成自己的 skill**。

三个约束决定了实现形态：

1. **AI 抓取 URL 只拿到服务端发出的内容。** 本项目的门户是 Vue SPA，客户端渲染 ——
   任何 SPA 路由对 AI 都只会返回一个空壳 `<div id="app">`。所以文档**必须服务端渲染**。
2. **文档会长期被分发。** 分享链接（`/s/<token>`）是有有效期的：过期返回 410、被吊销返回
   404。把文档挂成分享链接，等于给每个已缓存的 AI 埋一颗定时炸弹。
3. **文档描述的事实会随配置变化**（前缀、域名、内容上限、默认分享时长）。写死的文档
   会在某次部署后开始撒谎，而且是静默撒谎。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. 放进 SPA 做一页 | 与站点风格统一 | **AI 读不到**（客户端渲染）；需要 JS 才能看到内容 | 易 |
| B. 发成一条分享链接 | 零开发 | 会过期/吊销；基地址写死；无法更新 | 易 |
| C. 服务端渲染的稳定路由 + 编译进二进制 + 从 config 注入事实 | AI 能读；永不失效；改配置即改文档 | 需要新包与新路由 | 易 |
| D. C + 同时提供 llms.txt 与 markdown 版 | 额外覆盖 `llms.txt` 约定；agent 拿纯文本无需剥 HTML | 多两个路由 | 易 |

## Decision

采用 **D**，并确立以下规则：

- 文档正文放在 `backend/internal/aidoc/guide.md`，经 `//go:embed` 编进二进制 ——
  文档与代码**同一个 git revision**，不存在"文档部署了、代码没部署"的错位。
- 三个稳定路由：`{prefix}/ai`（服务端渲染 HTML）、`{prefix}/ai.md`（`text/markdown`）、
  `{prefix}/llms.txt`（约定发现入口）。三者**都无需鉴权** —— 一个 agent 必须在拿到密钥
  *之前* 就能读到"你需要一个密钥"。
- 文档中的事实用 `{{APP_BASE}}` / `{{ROUTE_PREFIX}}` / `{{DEFAULT_SHARE_HOURS}}` /
  `{{MAX_CONTENT_BYTES}}` / `{{ENVIRONMENT}}` 占位符，**每次请求**从 `config.Config` 注入。
- `llms.txt` 只做**指针**，不复述文档正文 —— 单一事实源才不会自我漂移。
- 这三个路由挂在 **`ROUTE_PREFIX` 之下**，不占用 `api.hcyj.xyz` 的根路径：该主机根路径
  属于另一个服务，抢 `/llms.txt` 会劫持别人的路由。

## Consequences

- **变容易**：改 `ROUTE_PREFIX` 或换域名后文档自动跟着走；文档与二进制同版本回滚。
- **变难/需注意**：新增一个占位符时必须同步 `aidoc.go` 的常量、替换器与测试，
  否则 `TestNoUnresolvedPlaceholders` 会让构建失败（这是刻意的）。
- **必须保持为真**：`views.HTMLMountEnabled` 若由 `false` 改为 `true`，
  文档里"HTML 渲染尚未启用"的说法就变成假的 —— `TestHTMLCaveatMatchesImplementation`
  是这条的绊线。
- **文档是公开的**：因此它**不得**包含密钥、也不得写出服务器上密钥的存放路径
  （`TestGuideContainsNoSecret` 守住这条；初版就是因为写了 `teleport.env` 的路径而被该测试拦下）。

## Revisit when

- 用户决定实现「agent 自助申请密钥」的面板：届时文档第 2 节要从"向用户索取"改为
  "如何申请并领取"，并需要新增一条鉴权路径与相应的额度/有效期语义。
- 站点出现第二种报告格式（如 HTML 渲染被启用）时，第 3 节的 `format` 建议需要重写。
