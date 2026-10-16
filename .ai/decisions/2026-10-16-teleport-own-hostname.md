# teleport 拥有自己的域名：teleport.hcyj.xyz，挂在根路径

**Status:** ✅ ACCEPTED · **Date:** 2026-10-16 · **Deciders:** 用户（"挪到 teleport.hcyj.xyz" +
"挂在根路径 /" + "P6 与切换一起做"）
· **Supersedes:** none · **Amends:** `2026-10-16-rewrite-on-gin-and-postgres.md` 的冻结清单里
「路由前缀」那一行，以及该 ADR 第四节 P6 的「Caddy 不需要改」

## 背景

teleport 现在挤在别人的域名下：`https://api.hcyj.xyz/yeciorez/teleport`。那个路径前缀
是**共享主机**的产物 —— `api.hcyj.xyz` 上的 Caddy 用 `handle /yeciorez/teleport*` 把这一段
摘给 teleport，其余落到另一个服务。前缀是冻结清单里的一项，因为它在**每一条已发出的
分享链接**里。

同一台主机上的 clip 有自己的域名（`clip.hcyj.xyz`），它的 Caddyfile 注释写着
`A whole host of its own, unlike teleport`。用户裁决 teleport 也应当如此。

## 已核实的事实（写进决策前实测过，不是推断）

| 事实 | 核实方式 |
| --- | --- |
| `teleport.hcyj.xyz` 目前**不存在** | DNS 解析到 hcyj 同一 IP，但 Caddyfile 无 site block、无证书 → TLS `code=000`、`no peer certificate available`；全仓库与记忆库搜不到该字符串 |
| **agent 密钥不受域名变更影响** | `agentkey.DeriveToken(kDerive, applicationID, claimSecret)` 只吃 `K_derive`（来自 `SESSION_SECRET`）+ 两个标签 + applicationID + claimSecret，**没有 URL、没有 origin**；鉴权侧 `ResolveAgentKey(sha256(token))` 同样不做 origin 绑定 |
| **`expires_at=0` 的分享链接会碎** | 分享 URL 里嵌着前缀，永久链接没有“过期”这条退路 |
| 当前线上是 502 | hcyj 的 `teleport` unit `disabled`+`inactive`；frps 只监听 8790（clip），8788 无监听 |

## 选项

| 选项 | 好处 | 代价 |
| --- | --- | --- |
| **A. 保持 `api.hcyj.xyz/yeciorez/teleport`** | 冻结清单不动；P6 的 Caddy 步骤真的不用改；在野链接与所有文档里的地址不变 | 链接长；共享主机的路由耦合继续存在；`/ai.md` 里给 agent 的地址一直带着别人的域名 |
| **B. 自己的域名 + 根路径**（已选） | 与 clip 形态一致；链接短；前缀这个耦合从架构里消失；`ROUTE_PREFIX` 变成空，配置里少一个可错项 | 动冻结清单；`expires_at=0` 的链接必须靠**永久保留旧前缀**才不碎；`ROUTE_PREFIX`/`PUBLIC_BASE_URL`/Vite `base`/`/ai.md` 里的地址/HTML iframe 同源判定全要跟着改；P6 多一个变更点 |
| **C. 自己的域名 + 保留 `/yeciorez/teleport` 前缀** | 配置几乎不用改，两个入口路径一致 | 白拿了独立域名，链接依然很长；共享主机的路径形态被永久固化 |

## 决策

**选项 B**，并附三条约束：

1. **新入口是根路径**：`https://teleport.hcyj.xyz/`，分享页 `/s/{token}`，agent 文档 `/ai`、
   `/ai.md`、`/llms.txt`，API `/api/*`。即 `ROUTE_PREFIX=""`。
2. **旧前缀 `api.hcyj.xyz/yeciorez/teleport` 永久保留**，不是过渡期。理由只有一个但足够：
   `expires_at=0` 的分享链接**永不过期**，删掉前缀就是永久碎掉它们，而冻结清单存在的全部
   意义就是不碎在野的东西。两个入口长期并存。
3. **域名变更与切换在同一个窗口做（P6）**，不提前。纲领里「重写期间不要改 hcyj 的 Caddy」
   这条纪律继续有效；提前改只会让新域名也返回 502（后端还不存在），多一个未验证变量。

## 后果

- **`PUBLIC_BASE_URL` 决定新链接与 `/ai.md` 广告哪个地址**。旧前缀继续服务，但新发出的
  链接走新域名 —— 两者必须在同一窗口里一起验证，否则会出现“新链接能开、agent 文档却指向
  旧域名”这类不一致。
- **`ROUTE_PREFIX=""` 的具体影响面**（代码里前缀都从 `cfg.RoutePrefix` 推导，但空值是新的
  分支，必须实测而不是假定）：Go 路由挂载、Vite `base`、`web/src/api.ts` 的 `BASE_URL`、
  SPA 回退、`views` 的分享页、`aidoc` 的 `/ai.md`、限流与 CORS 用的 `FRONTEND_ORIGINS`。
- **cookie 的名字不变**（`teleport_session` 是冻结项），但 `CookiePath` 要跟着新路径走；
  两个入口并存时，跨入口的会话不会共享，这是可接受的（面板在哪个入口用就在哪登录）。
- **`format:"html"` 的同源 iframe 判定**要重新确认：它依赖“同源”，新域名下同源关系不变
  （iframe 与页面同在一个 host），但旧入口的分享页必须继续嵌旧入口的 `/html`。
- **冻结清单那一行的含义变了**：不是「前缀不可改」，而是「**旧前缀必须一直有效**」。
  这条比原来更严，不是更松 —— 它从“别动它”变成“两个都要活着”。
- **P6 的范围扩大**：纲领写的「hcyj 的 Caddy 不需要改」不再成立，P6 现在包含
  「加 `teleport.hcyj.xyz` 的 site block + 证书 + 保留原有 `handle /yeciorez/teleport*`」。
  这是**同一台主机、同一份 Caddyfile，同时服务 clip**，改动前必须两边一起想。

## 什么时候重新看

- 如果 P6 当天发现两个入口并存的运维成本高于收益（例如证书/路由出现只有双入口才会有的
  问题），退路是**只保留旧前缀**并把新域名做成 301 到旧前缀 —— 但那会让新链接再次变长，
  属于回退而不是等价方案。
- 如果发现任何**已发出的 agent 密钥**里其实隐含了 origin 依赖（目前核实为没有），
  这条决策必须重开：那意味着域名变更会失效凭据，代价量级完全不同。
