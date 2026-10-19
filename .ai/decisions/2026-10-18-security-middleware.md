# 加一层传输级滥用防护：分级限流 + 源地址封禁 + 面板 CSP

**Status:** ✅ ACCEPTED · **Date:** 2026-10-18 · **Deciders:** 用户（四项裁决）+ 实施者 · **Supersedes:** none

## Context

服务已在 `https://teleport.hcyj.xyz/` 上公开运行，能到达它的人就能写报告、申请密钥、尝试登录
面板。上线前的防护只有两条：`SecurityHeaders`（响应头）与 **`POST /api/agent-keys/applications`
一个端点的 per-IP 限流**（`KEY_APPLY_PER_HOUR`）。具体缺口：

- **`/api/admin/login` 完全没有限流。** 一个口令保护整个面板，而每次尝试要走
  **600k 迭代的 PBKDF2 验证**（`password.DefaultIterations`）——既是口令爆破目标，也是
  CPU 耗尽向量。两条都要堵，而后者只要「在验证之前拒绝」就没了。
- **公开读没有任何上界。** 分享页/JSON 每次都要查库 + 渲染 markdown，还会 `view_count + 1`。
- **没有封禁机制。** 一个持续滥用的地址除了被 429 之外没有任何后果。
- **SPA 外壳（面板本体）没有任何 CSP**：只有分享页、html 报告、AI 指南带 CSP。

已冻结、不可违反的约束（改动前逐条核对过）：

1. **限流桶键必须用 `httpx.ClientIP`**，绝不用任何 RealIP 式实现。`ClientIP` 只在直连 peer 是
   已知代理时才读转发头，并从 `X-Forwarded-For` **右往左**取第一个不可信地址。
2. 响应封套不许改：`{ok:true,data,requestId}` / `{ok:false,error:{code,message},requestId}`。
3. 未知 `/api/*` 必须仍是 JSON。
4. `format:"html"` 报告逐字节原样渲染是唯一例外，有自己的 CSP。
5. 单二进制、无 Redis、无后台 goroutine（限流状态只能是进程内的）。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A 只加登录爆破防护 | 最小改动，风险最低 | 公开读与写接口仍可被刷 | easy |
| B 分级限流（登录/公开读/写）+ 自动临时封禁 + 手工永久名单 + 面板 CSP | 覆盖全部公开面；封禁自愈；面板补齐 CSP | 需要新的配置面与一套新测试；配置错误会误伤真实用户 | medium |
| C B + 与 IP 无关的全局 QPS 上限 | 有真正的兜底：单一 IPv6 /64 含 2^64 个地址，per-IP 限制对它无效 | 全局上限会牺牲一部分突发吞吐 | medium |
| D 改为逐路由挂中间件 | 实现直接 | **踩本仓库已记录的 pitfall**：漏挂一条路由 = 静默放行，功能测试全绿而安全属性为零 | easy（但危险） |

## Decision

采纳 **B + C 的全局兜底**，并按 A 的谨慎程度划范围：

1. **新的原语放 `internal/httpx/guard.go`**，与既有中间件同包，共用 `ClientIP`：

   - `Limiter`：滑动窗口（`NewLimiter(limit, window)`）。**拒绝也记一次命中**，否则「一直猛打
     已满的桶」会因为最早的命中自然过期而每次免费拿到一个请求。
   - `BanList`：`NewBanList(permanent, threshold, strikeTTL, banTTL)`。同一窗口内被打回
     `threshold` 次即临时封禁 `banTTL`，到期自动解封。
   - **永久名单只来自配置**，绝不自动产生：这是本组里唯一不会自愈的控制，用它来处理已确认的
     惯犯。

2. **封禁永不作用于可信代理/回环地址。** 这是本决策里最重要的一条防御。一旦部署丢了
   `X-Forwarded-For`，`ClientIP` 会把所有客户端塌缩成 Caddy 自己的地址——**封它一条请求就能把
   整个服务打挂**，比它要防的滥用严重得多。此时限流仍然生效（这才是对「缺头」的正确反应），
   并打一行 `SECURITY:` 警告，见下面的「同一次改动里的守卫」。

3. **四类桶，各自独立**：`login`（每小时）、`publicRead`（每分钟）、`write`（每分钟）、
   `global`（每秒）。类之间不共享预算——否则读者翻分享页会把 agent 发布报告要用的预算花光。

   `global` 是**唯一与源地址无关**的桶，而且这一点是靠**实现**保证的，不是靠命名：
   它用**一个常量键**（`globalBucketKey`）而不是 `ClientIP`。这一点被**自己的负向对照抓错过**——
   第一版把它写成 `RateLimit(globalLimiter, banList)`，那它就只是「窗口为 1 秒的另一个
   per-IP 桶」，与「兜住手握大量地址的调用者」正好相反（一个 IPv6 /64 含 2^64 个地址）。
   修正后由 `TestGlobalRateLimitIsSharedAcrossAddresses` 与 httpx 层的
   `TestGlobalBucketIsAddressIndependent` 盯着。

   `global` 还有两条刻意的差异，都有可判定的守卫：

   - **它绝不喂封禁名单。** 全局超限意味着「进程饱和」，不是「这个调用者可恶」。把全局超限
     归因给「恰好下一个请求」的人，会在它本来要防的洪流里把**无辜用户**成批封掉。
     `RateLimitGlobal` 不接受 ban list，所以这一条由签名保证；真正会退化的地方在**装配层**
     （一个 `BanList` 被所有类共用），由 `TestGlobalRateLimitDoesNotBanTheCaller` 盯着
     （`BAN_AFTER_REFUSALS=1`，所以一次错误归因就会整个测试变红）。
   - **它不作用于豁免路径。** `/api/health` 按**完整路径精确匹配**豁免：一个答 429 的存活探针
     会被轮询方读成「服务挂了」，而限流状态是进程内的——一次监督进程重启会**同时清掉窗口与
     封禁**，于是一个能被推成 429 的探针就成了一条「重置计数器」的路子。它的代价只是一次池内
     数据库 ping，所以豁免它无法被放大成写风暴。（`TestGlobalBucketExemptsExactPaths`，
     含「exemption 必须精确匹配」的反证。）

4. **挂载方式**：
   - `BanList.Blocked()` 与 `global` 限流挂在**传输链**上（路由之前），所以被封的地址在
     **任意路径**（包括不存在的路径）都被拒；只拦已知路由等于让扫描器免费探测。
   - 三个分级限流挂**逐路由**（与既有 `limitKeyApplications` 同一手法）。
   - `/api/health` **刻意不在任何限流类里**：它是 systemd / Caddy / 大陆观测点在轮询的东西，
     限流它会让服务「看起来死了」——用限流造成一次拒绝服务。它仍过封禁中间件（被封的地址照样
     在它上面 403），也仍在传输链里，只是不受配额限制。

5. **默认值按环境分流**：`ENVIRONMENT=production` 默认全开，其余环境默认全关
   （`SECURITY_MIDDLEWARE` 可显式覆盖两个方向）。理由是两个受众要的正好相反：线上要全开；
   而 91 项契约脚本与浏览器脚本会在几秒内从同一个地址打满整个面，那里的 429 会被读成回归。

6. **面板 CSP 先进 Report-Only**，字段拼写为 `DashboardCSPEnforce`（默认 **false**）。
   **极性是刻意的**：report-only 是安全态（策略写错只产生一条 console 违反 + 一个能用的页面），
   而 enforce 写错是「面板坏了且没有可操作的错误」。用这个拼法，**零值 = 安全态**，
   与 `DashboardCSPReportOnly`（零值 = 开始强制执行）相反——后者让一个「没人配置过的 Config」
   静默进入强制执行。这是实测发现的：手写的测试 Config 就是这种 Config。

7. **揭穿/合并**：`ClientIP` 拆出 `ClientIPAddrOK(r) (netip.Addr, bool)`，`ClientIP` 变成它的
   字符串包装。**限流与封禁必须由同一个函数决定「你是谁」**，各自重写一遍转发头遍历逻辑，
   就是其中一条在重构后开始相信伪造头的方式（已有负向对照盯着这一点）。

## Consequences

**变得容易：**

- 登录爆破既被限速又不花 PBKDF2；封禁让持续滥用有代价且**自愈**。
- 面板补齐了 CSP（report-only）、`Permissions-Policy`、
  `X-Permitted-Cross-Domain-Policies`，且**不动**分享页/html 报告各自更严的策略。
- 误配 `X-Forwarded-For` 的失败模式是**吵的**（一行警告 + 全局共享桶的 429），不是静默放行。

**本次实现里被自己的对抗性验证抓到的两处真缺陷（不是推理出来的）：**

- 🔴 **「全局」桶第一版是 per-IP 的。** 文档写的是「与 IP 无关」，实现却是
  `RateLimit(globalLimiter, banList)` ——那只是一个窗口为 1 秒的 per-IP 桶，而 IPv6 /64 让
  per-IP 限制形同虚设，所以这个兜底当时等于不存在。
  **是负向对照（把桶键改回 `ClientIP`、以及把 global 改回 `RateLimit`）抓到的**，
  不是普通测试抓到的：普通测试当时全绿，因为我为「per-IP」写的断言对两者都成立。
  教训与 `.ai/pitfalls/cases/2026-10-16-verification-that-cannot-fail.md` 同族：
  **新控制的第一版必须逐个做负向对照，否则「绿」可能只是断言选错了。**
- 🔴 **`upgrade-insecure-requests` 在 report-only 策略里是无意义且会报 console error 的指令**
  （见 `.ai/pitfalls/cases/2026-10-18-report-only-csp-ignored-directive.md`）。
  这是 `verify-p4.sh` 的**真实浏览器**阶段抓到的——单元测试、`curl -D-`、CSP violation 报告
  全都看不到它。

**变得容易：**

- 登录爆破既被限速又不花 PBKDF2；封禁让持续滥用有代价且**自愈**。
- 面板补齐了 CSP（report-only）、`Permissions-Policy`、
  `X-Permitted-Cross-Domain-Policies`，且**不动**分享页/html 报告各自更严的策略。
- 误配 `X-Forwarded-For` 的失败模式是**吵的**（一行警告 + 全局共享桶的 429），不是静默放行。

**变得更难 / 必须成立的事：**

- **新增配置面**：`SECURITY_MIDDLEWARE`、`LOGIN_RATE_LIMIT`、`PUBLIC_READ_RATE_LIMIT`、
  `WRITE_RATE_LIMIT`、`GLOBAL_RATE_LIMIT`、`BAN_AFTER_REFUSALS`、`BAN_STRIKE_TTL`、
  `BAN_TTL`、`BLOCKED_IPS`、`DASHBOARD_CSP_ENFORCE`。全部有文档与校验；
  `BLOCKED_IPS` 里写错一个值**拒绝启动**（不然就是一个「什么都不封」的封禁名单）。
- **`spa.Handler` 的响应头是「提交时」才写**（`headerWriter`），不是在进入 `ServeHTTP` 时。
  进入时就写会让安全头覆盖 handler 自己的 `Content-Type`（实测：`text/plain` 的 CSP 打在 JS
  bundle 上）。这正是新测试 `TestSecurityHeadersDoNotOverwriteTheHandlersOwn` 抓到的东西。
- **状态是进程内的**，重启清零。对单二进制是可接受的取舍（攻击者无法重启进程），但不要把它
  当成持久配额。
- **per-IP 限制仍然是「减速带」而不是防御**：一个 IPv6 /64 含 2^64 个地址。真正的兜底是
  `GLOBAL_RATE_LIMIT` 与既有的 `KEY_MAX_PENDING`。

**下一步（未做，需要人类批准）：**

- 面板 CSP 要**在真实浏览器里跑一遍零违反**，才把 `DASHBOARD_CSP_ENFORCE` 翻成 true。
  证据由 `scripts/verify-p4.sh` 的浏览器部分提供（它会把任何 CSP 违反收成 console error）。
- 部署：`teleport.env` 需按需加上述变量；默认值已可用于生产。**部署前先问人类。**

## Revisit when

- 面板接入任何**内联样式或内联脚本**（`:style` 绑定、运行时 `<style>` 注入、CDN 脚本）——
  report-only 会先报出来；那时要么改写前端，要么**先改本记录**再放宽 `style-src`。
- 面板 CSP 已在真实浏览器零违反 → 把 `DASHBOARD_CSP_ENFORCE=true`，并在本记录补上证据。
- 任何 endpoint 出现**合法的**突发流量（批量导入、压测、监控轮询）被 429 →
  先判断是「该端点属于另一个类」，而不是直接调大全局数字。
- 需要**跨进程/跨重启**的封禁或配额（多实例、或要封住「重启即解封」）→ 进程内状态不再够用，
  那时才引入外部存储，并重新评估共享 Postgres 的连接预算。
