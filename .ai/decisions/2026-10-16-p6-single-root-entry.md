# P6 单一根路径入口：放弃旧前缀（break change）

**Status:** ✅ ACCEPTED · **Date:** 2026-10-16 · **Deciders:** 用户
（"break change 没事的 使用 teleport.hcyj.xyz 然后直接 /链接 就好了"）
· **Supersedes:** `2026-10-16-teleport-own-hostname.md` 里的约束 2（「旧前缀永久保留」）
· **Amends:** `2026-10-16-rewrite-on-gin-and-postgres.md` 冻结清单里的「路由前缀」那一行

## 背景（事实，不是偏好）

纲领的冻结清单里有一项是「路由前缀」，理由是**在野的分享链接**里嵌着它。域名裁决
（`2026-10-16-teleport-own-hostname.md`）在此基础上加了一条约束：新域名挂根路径
（`ROUTE_PREFIX=""`），**而旧前缀 `api.hcyj.xyz/yeciorez/teleport/*` 永久保留** ——
因为 `expires_at=0` 的链接永不过期，删掉前缀就是永久碎掉它们。

为 P6 做只读侦察时发现，那两条**同时**成立是不可能的：

| 事实 | 位置 | 后果 |
| --- | --- | --- |
| `ROUTE_PREFIX` 决定整个应用面 | `internal/api/router.go`（`p := cfg.RoutePrefix`）、`config.Route()`、`AppBaseURL()`、`CookiePath` | 一个进程只有一份前缀 |
| 分享页的资产与 iframe 是**绝对 URL**，指向 `PUBLIC_BASE_URL` | `internal/api/helpers.go` 的 `renderSharePage` 用 `cfg.AssetBaseURL` 与 `cfg.ShareURL(token)+"/html"` | 旧域名上的页面会去新域名取资源 |
| 分享页的 CSP 是 `script-src 'self'` + `frame-src 'self'` | `internal/views/sharepage.go` | 跨源取资源会被**挡掉** |
| **前端前缀是构建期烧进产物的** | `vite.config.ts`：`base = (VITE_ROUTE_PREFIX ?? '/yeciorez/teleport') + "/"`；`web/src/api.ts` 用 `import.meta.env.BASE_URL` | 一个二进制 = 一份烧死的基址 |

所以「旧前缀只是被 Caddy 去前缀转发」这条捷径不成立：旧前缀的分享页仍会去新域名取
JS 与 iframe，被 CSP 挡掉 → **Mermaid 不渲染、`format:"html"` 的报告变成空框**，
而正文文本还在（一种半可见的坏法）。要真正支持双入口，必须走三个方案之一
（旧前缀 301 / 代码支持双前缀 / 两个实例），每个都有真实代价。

## 选项

| 选项 | 好处 | 代价 |
| --- | --- | --- |
| A. 旧前缀：API 就地去前缀转发 + 页面 301 到新根 | 只改 Caddy；旧链接仍能打开 | 多一跳；要破「必须用 `handle` 而不是 `handle_path`」那条纪律；两套前缀的 URL 语义长期并存 |
| B. 代码支持双前缀（`LEGACY_ROUTE_PREFIX`） | 两个入口都完全自洽，一个进程 | 动冻结的「前缀」语义；cookie/限流键/同源判定都要重想；P6 变成 P5.5 |
| C. 两个实例、同一个库，各带一个前缀 | 完全自洽，不改代码 | 两套 unit/隧道/Caddy/构建（前端要构建两次）长期同步 |
| **D. 放弃旧前缀：根路径是唯一入口** | **P6 显著简化**：一个构建、一个实例、一个前缀；`handle_path` 的纪律不动；架构里少一个耦合；前端基址与后端前缀天然一致，没有「半可见坏法」 | **旧长链接全部 404，包括 `expires_at=0` 的永久链接** —— 不可挽回 |

## 决策

**选 D。** `https://teleport.hcyj.xyz/` 是唯一入口（`ROUTE_PREFIX=""`），
`api.hcyj.xyz/yeciorez/teleport/*` **不再保留**：它返回 404（或 Caddy 的兜底上游行为），
`expires_at=0` 的永久链接也随之失效。用户已知情并明确接受。

这是一个**不可逆**的数据面变更，所以它值得单独记一条：此前「旧前缀必须永久有效」
的唯一理由就是那些永久链接，而这条裁决把那个理由取消了。

## 后果

- **P6 变简单了**：不需要 Caddy 的 `handle_path`/`strip_prefix`，不需要第二个实例，
  不需要后端支持双前缀。AGENTS.md 里「Caddy 必须用 `handle` 而不是 `handle_path`」照旧成立
  （因为后端不再需要前缀）。
- **要同批改掉的文档**（否则记忆库与实现互相矛盾）：
  - `AGENTS.md` 的「前缀只写一处」条目（前缀现在默认为空，且不再有第二个入口）；
  - `2026-10-16-teleport-own-hostname.md` 的约束 2（必须标为被取代，不能删除历史）；
  - 本仓库 ADR 冻结清单的「路由前缀」行（已改为指向本记录）；
  - `/ai.md`、`/llms.txt` 里给 agent 的地址（由 `PUBLIC_BASE_URL` 生成，会自动变）。
- **构建必须显式带上前缀**：`VITE_ROUTE_PREFIX=/` 与 `ROUTE_PREFIX=""` 要**成对**出现。
  `scripts/build.sh` 默认仍是 `/yeciorez/teleport`，所以 P6 的构建命令不能照默认跑 ——
  这一点写进了 `.ai/runbooks/p6-cutover.md`，并且是「前端 base 烧死在构建期」的必然结果。
- **旧链接的处理方式要写清楚**：不是「静默 404 让人困惑」，而是**明确弃用**。
  可选的体面做法（**尚未裁决**）：给 Caddy 的旧前缀加一个 `respond 410` 并附一句
  「teleport 已迁到 https://teleport.hcyj.xyz/」—— 一行 Caddy 配置，比 404 更可解释。
  这属于运维礼仪，不影响本决策。
- **已核实不受影响**：agent 密钥由 `SESSION_SECRET` + 两个标签 + applicationID + claimSecret
  派生（`agentkey.DeriveToken`），**不含 URL/origin**；鉴权侧只按 `sha256(token)` 查表。
  所以域名/前缀变更**不失效任何凭据**，只失效旧 URL。已发出的密钥在新域名下继续可用。

## Revisit when

- 如果发现**任何**仍然有效的、重要的外部引用指向旧前缀（例如某个 agent 的配置里写死了
  `api.hcyj.xyz/yeciorez/teleport`，且它不能改），那就需要回到方案 A 或 C —— 这条决策的前提是
  「旧链接的价值低于双入口的成本」，而这个前提可以被一个真实的外部依赖推翻。
- 如果 `expires_at=0` 的链接事后被证明还在被真人使用（日志/访问计数），同样回到上面那条。
