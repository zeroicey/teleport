# Current task

**Status:** 🚧 **P0 ✅ / P1 ✅（附更正）/ P2 ✅（P2.1–P2.3）/ P3 ✅ / P4 ✅ / P5 ✅** —— store 是 **pgx + sqlc**，
HTTP 层是 **Gin**，**全链路已在真二进制上验过**：HTTP 契约 **64/64** + 真实浏览器 **15/15**
（`scripts/verify-p4.sh`，第三档闸门），并做了 **旧树 vs 新树的差分跑：108 请求只有 1 处已知差异**。
**数据搬运已在临时 schema 上彩排通过（`scripts/verify-p5.sh`，15/15 + 负向对照）。**
四档闸门全绿。**P6（切换）已完成并线上验证**：`https://teleport.hcyj.xyz/`（根路径）
由 hpcore 的新版服务 + frpc 隧道 + hcyj 的 Caddy 提供服务，数据来自 PostgreSQL 生产库。
**线上不再是旧版，也不再是 502。** 切换记录见 `.ai/runbooks/p6-cutover.md` 的「执行记录」。

**P2.3 交付物**：`internal/store/` 的 `queries.go` + `keys.go` 改为 pgx；新增
`internal/store/queries/*.sql`（sqlc 源）+ `internal/store/storedb/`（生成物）+ `backend/sqlc.yaml` +
`scripts/check-sqlc-sync.sh`（`sqlc diff` 漂移闸门）；三处上限上限用 `pg_advisory_xact_lock`
（`pg.go` 的 `lockCap`）；`Store` 改为持 `*pgxpool.Pool` + `storedb.Queries`（`New(pool)`、
`Pool()`、`Close()`）；`config.DBPath` → `config.DatabaseURL`（env `DATABASE_URL`，**必填**）
+ `DB_MAX_CONNS`（默认 8）；`main.go` 改用 `store.OpenPG`。

**P2.3 期间发现并修的真 bug**：`ClaimApplication` 失败分支里的 `diagnoseClaim` 在事务未结束时
又朝**连接池**要了一条连接 → 小池（测试用 4 条）× 8 竞争者 = 永久死锁（无超时）。已改为用事务自己的
query set 读取。见 pitfall `2026-10-16-store-nested-pool-acquire-deadlocks.md`。
**P2.3 复审（4 个只读子代理：语义等价 / 锁 / 测试完整性 / 架构）发现并已修**：
- 🔴 **真 bug：`DecideRenewal` 一次续期发两次展期**。旧的 `DecideRenewal` 是「先读 status 再写」，
  那条 `SELECT` **不加锁**；SQLite 的 `BEGIN IMMEDIATE` 曾免费保证原子性，PG 不保证。两个并发决定
  都读到 `pending`、都扩了期，最后那条带 `status='pending'` 的守卫 UPDATE 才拒绝第二个 —— 但
  拒绝发生在扩展**之后**，且事务照样提交。已改为 `SELECT ... FOR UPDATE` + **检查守卫 UPDATE 的
  行数并在 0 时回滚**（后者才是真正阻止泄漏的机制，前者是提前失败）。决定性反证：改造前的代码
  在 `TestDecideRenewalRefusedDecisionLeavesNoExtension` 上**每次**都失败；而 8 竞争者的统计测试
  对旧代码 **5/5 全过**。见 pitfall `2026-10-16-decide-renewal-granted-twice.md`。
- 🟠 **三个上限的「锁被删掉」原先只有统计型守卫**（会漏）。已加 5 个**决定性**守卫：
  另一事务持有某把锁时，对应 store 方法必须阻塞（顾问锁 × 4 + 续期行锁 × 1），并用
  `FOR UPDATE NOWAIT` 探针精确判定「它在拿到行锁前没有写过 agent_keys」。
  **先反证再加**：删掉 `lockCap` 后这 4 个必红；删掉 `FOR UPDATE` 后行锁守卫必红。
  （第一版行锁守卫只断言「它会阻塞」，对删掉 `FOR UPDATE` 的变体**照样通过** —— 已改成可判定的版本。）
- 🟠 **P1 遗留（不是 P2.3 引入）：`share_tokens.view_count` 被窄化成 INTEGER（int32）**，
  而 SQLite 的 INTEGER 是 64 位、`request_count` 也是 BIGINT。已改回 BIGINT（含基线、快照、
  sqlc 生成物），并加 `TestPGCountersAre64Bit`（反证：改回 INTEGER 即红）。
  注意：`teleport_test.public` 与两个残留 `t_*` schema 里的旧 checksum 记录已清理（那是 P1 的
  stale 测试库残骸，正是 pitfall 说的那类）。
- 🟠 **锁等待原先无上界**：`OpenPG` 未设任何 timeout，`callCtx` 是 `context.Background()`，
  服务端 `lock_timeout=0` → 一个卡住的锁持有者会让所有发凭据的请求**永久挂住**（不是报错）。
  已设 `lock_timeout=10s` + `idle_in_transaction_session_timeout=60s`（DSN 可覆盖），
  并加 `TestOpenPGBoundsLockWaits`（行为断言：持锁时返回 55P03，而不是挂住）。
- 🟠 **`scripts/check.sh` 会在跳过 ~143 个测试的情况下打印「gate passed」**。现在结尾会打印
  `gate passed — WITHOUT DATABASE-BACKED TESTS: N test(s) skipped`。这是「同一次绿」原则的要害：
  无库档几乎不验证任何冻结契约。
- 文档漂移已同批修：`.ai/ARCHITECTURE.md`（SQLite 条目）、`.ai/runbooks/deploy-teleport.md`
  （零处提到 PostgreSQL，备份步骤仍是 sqlite3）、`README.md`、`.engram/BOOTSTRAP.md`、
  `backend/deploy/teleport.env.example`（仍写 `DB_PATH`，照它部署**启动即失败**）、
  若干测试注释与 `.gitignore`。

**P3 交付物（HTTP 层 → Gin）**：
- `internal/api/router.go` 的 `New` 改成 **Gin 路由表**（`gin.New()`，关掉
  `RedirectTrailingSlash`/`RedirectFixedPath`/`HandleMethodNotAllowed`，否则会出现
  ServeMux 从未有过的 301/307/405）；39 个 handler 变成 `gin.HandlerFunc`，用 `c.Param`
  取代 `PathValue`。
- `internal/httpx/gin.go`：`GinMiddleware(mw)` 把现有的 net/http 中间件适配成 Gin 中间件，
  并把中间件塞进 request context 的 principal/session/requestId 回写到 `c.Request`，
  所以下游（含服务端渲染的交接页）读法一字未变。拒绝时 `c.Abort()`。
- **信封、session 加密、`ClientIP` 全部保持纯函数**，仍以 `(w, r)` 调用
  （`httpx.OK(c.Writer, c.Request, ...)`），所以信封逐字节不变、其测试零改动。
- 传输链（Recover/RequestID/LogRequests/SecurityHeaders/CORS/Compress）**仍包在 Gin 引擎外面**，
  理由见决策记录：`Compress` 靠包装**原始** ResponseWriter 才正确，`CORS` 必须在路由**之前**
  处理 OPTIONS（都有冻结测试盯着），在 Gin 里重建这两个是纯粹的净损失。
- **HEAD 必须显式注册**（`get` helper = GET+HEAD）：Gin 不会把 HEAD 路由给 GET handler，
  而冻结契约里唯一一处真 405 正是「HEAD 领取一次性凭据」。
  新增 `TestHeadIsRoutedLikeGet` 并**反证**过（删掉 HEAD 注册 → 它与
  `TestKeyHandoffClaimRefusesHEAD` 一起变红，405 退化成 404）。
- **gin 版本钉在 v1.10.1**：实测 v1.11.0 起 `gin/binding` 会链入 `mongo-driver/v2/bson`、
  `gin` 会链入 `quic-go/http3`（v1.11.0 有 15 个这类包，v1.12.0 有 22 个，v1.10.1 是 0）。
  一个手写信封、自己解析 body、只跑 HTTP/1.1 的服务不该链进 BSON 编解码器和 HTTP/3 栈。

**P3 已验证 / 未验证**：
- ✅ `internal/api`（47 个测试）与 `internal/httpx` **零测试改动**通过 —— 这是 ADR 的完成判据。
- ✅ 逐个跑过 Gin 默认值会破坏的冻结行为：`TestMethodNotAllowed`、`TestKeyHandoffClaimRefusesHEAD`、
  `TestCORSPreflightOnRealRoute`、`TestUnknownRouteWithSPAStaysJSON`、
  `TestNewRoutesAreNotSwallowedBySPA`、`TestRoutesAreMountedUnderPrefix`、`TestAIGuideRejectsNonGET`。
- ✅ 两档闸门绿；`internal/api` + `internal/httpx` 的 `-race` 干净。
- ✅ **P4 已补上这条**（见下方 P4 小节）：全链路端到端 + 真实浏览器都做了（契约 64/64、浏览器 15/15）。
  P3 当时那句话已由 P4 兑现：现在绿的是「在野契约对真二进制绿」。

**P4 交付物（全链路 + 浏览器）**：
- `scripts/verify-p4.sh` —— 编排：在 `teleport_test` 建私有 schema `p4`（**不新建库**：`teleport`
  角色 `rolcreatedb=false`，且实例与 clip/registry/serenique 共用，不值得为冒烟测试授 `CREATEDB`），
  构建、起服务、跑两档验证、拆 schema。含**端口预检**（已被残留服务反证过会红）。
- `scripts/verify-p4-contract.sh` —— 真 socket 上的 64 项冻结合同（信封/命名/404-410/所有权 404-not-403/
  HEAD 405 + `Allow: GET`/一次性领取/`format:"html"` 逐字节/CSP/续期只延长/限流键）。
- `scripts/verify-teleport-browser.cjs` —— 原始 CDP 驱动 headless Chromium，15 项：
  Vue 真的执行、路由守卫、**真实登录**、作用域为前缀的 `teleport_session`、数据渲染、
  分享页 SSR、**Mermaid 在真实 CSP 下渲染成 `<svg>`**、无意外 console/网络错误。
- 新 pitfall `2026-10-16-verification-that-cannot-fail.md`。

**P4 的关键证据（差分）**：用 `git archive 43cea51`（P3 之前的 net/http 树）构建旧二进制，
与新版在**同一请求集**上逐条对比 → **108 请求，1 处差异**，且那处只是
`GET {前缀}/api%2Fhealth` 的 **body** 不同（两版都是 404；新：JSON 信封，旧：net/http 纯文本）。
这条证据同时**排除**了两个子代理候选发现：`/ai.md` 的 405 与 2 MB body 的 400 **两版相同**
（既有行为，不是 P3 回归；超限的 *content 字段* 才是 413，有冻结测试盯着）。

**P4 已实测 / 未实测**：
- ✅ 三档闸门绿；环境干净（`teleport_test` 只剩 `public`；生产意图库 `teleport` 仍 **0 张表**；
  clip 的 8790 未被碰；无残留进程）。
- ⛔ **未做：大陆观测点**（P4 全程 loopback；ADR 验收第 4 条只能等 P6 之后）。
- ⛔ **未做：截图未经人眼确认**（当前模型不支持看图；结论来自 DOM 断言，比截图强，但截图本身未目视）。
- ⛔ **未做：`ENVIRONMENT=production` 的 Gin release 模式**；`format:"html"` 的字节比较用了 `$(...)`
  （会吃掉尾部换行，需 `cmp` 才算字节级证据）；合同 14 只在 API 侧间接覆盖（DB 侧由
  `TestNoPlaintextCollected` 盯着）。

**P5 交付物（SQLite → PG 数据搬运 + 行数/行为核对）**：
- `backend/internal/sqlitemigrate/`：`source.go`（**通过 `sqlite3` 命令行 shell 的 `-json` 模式**读旧库，
  另有 `RenderedText`：让 **SQLite 自己**用 `CAST(col AS TEXT)` 渲染值，供校验器做与 Go 无关的比对），
  `mode=ro&immutable=1` URI，**零新增 Go 依赖** —— 发布产物一个字节未变）、`target.go`（目标自省 +
  `OpenTargetPool`）、`import.go`（单事务搬运 + 提交前行数核对）、`verify.go`（15 项只读检查）。
- `backend/cmd/sqliteimport/`：`import` / `verify` 两个子命令；`-replace` 必须**同时**给 `-yes`
  才会 TRUNCATE，否则只打印「将要删掉什么」并以码 3 退出。
- `backend/internal/sqlitemigrate/testdata/legacy_sqlite_schema.sql`：**冻结的旧 SQLite DDL**，
  只用来给测试造夹具（旧 `schema.sql` 已随 P2 删除，这份是它的最后形态）。
- `scripts/verify-p5.sh`：第四档闸门。拒绝非 `*test*` 库名；拒绝带 `-wal/-shm/-journal` 的源；
  可 `P5_FETCH=1` 从 hcyj **只读**取回源（先 `sha256sum` 远端、再 `scp`、再比对）；
  在 `teleport_test` 建私有 schema 彩排，跑完即拆。
- 决策 `2026-10-16-p5-schema-ownership.md`；pitfall `2026-10-16-verifier-shares-the-conversion-it-checks.md`
  （🔴 本会话最重要的教训）、`2026-10-16-controls-that-tamper-the-reference.md`、
  `2026-10-16-migration-bookkeepings-do-not-match.md`；并给
  `2026-10-16-merged-baseline-hides-a-stale-test-db.md` 追加了第二次同族现象。

**P5 的核对（用户要的四条，全部有检查）**：
- **报告数 5 / 密钥数 2 / 分享链接 6 / 申请 4 / 续期 0** —— 源侧用 `sqlite3` shell 独立数，
  目标侧用 `psql` 独立数，两边都对着**人给的数字**（不是工具自己算出来的数字）断言。
- **`expires_at=0` 的永久链接仍永久**：不只断言「现在能打开」，而是直接断言目标里
  `expires_at` 仍是 0（把它改成 `now+1h` 的篡改会被点名报错 —— 因为「能解析」**不蕴含**「永久」）。
- **已吊销的仍 404**：`is_active=0` 的 token 在目标里必须仍非活跃，且 `store.ResolveShare` 必须返回
  `missing(404)`。源里那两个被吊销的链接中有一个**同时**是 `expires_at=0`，所以它必须**同时**满足
  「永久」和「404」两个性质。
- 另加：`report_content` 摘要（报告正文逐字节）、`ownership_boundary_unchanged`（ADR 点名的
  `owner_key_id=''` root 报告必须**仍然**归属 root）、`agent_keys_still_authenticate`
  （按 `token_hash` 走 store 的鉴权路径，活着仍活、吊销/过期的仍解析不出来）。

**P5 期间发现并修掉的自己代码里的真 bug（按严重度）**：
- 🔴 **超范围整数被静默回绕，而且校验器报绿**。源里一个 INTEGER 列的值为 2^63
  （SQLite 装不下就用 REAL 存，`sqlite3 -json` 打出 `9.2233720368547758e+18`），
  `int64(float64)` 在超范围时结果未定义、本平台回绕成 `-9223372036854775808`，
  **写进了 PG 而且导入 exit=0**；更糟的是**校验也全绿** —— 因为校验器和搬运工具共用
  `convertValue`，两侧错得一模一样，错误互相抵消。已修两层：① `integral()` 加显式范围检查
  （拒绝）；② 新增 5 项 `db_rendered_text[...]` 检查，让**两个数据库各自**把值渲染成文本再比
  字符串，**全程不经过 `convertValue`**（真源上共 157 个格子）。两道反证都做过：
  把范围检查 `if false &&` 掉 → 单元测试 5 个子用例变红并打出回绕值；端到端那条也变红。
  见 pitfall `2026-10-16-verifier-shares-the-conversion-it-checks.md`。
- 🔴 **`json.Number` 没被处理**：`sqlite3 -json` 把整数和实数都解码成 `json.Number`，而我的
  `anyToInt64/anyToFloat64` 只认 `int64/float64/string` → 每一个值都会报错（幸好是响亮失败）。
  同时把 `fmt.Sscanf("%d")` 换成 `strconv.ParseInt`：前者会把 `720.0` **静默截断成 720**。
- 🟠 **`report_content` 摘要在真数据上「靠运气」通过**：源给的是原始 JSON 文本，store 给的是
  解码后的 map 再编码，键序不同。真数据的每个 `metadata` 恰好都是字母序，所以它绿了。
  已改成两侧都过 `canonicalJSON`。
- 🟠 **负向对照第一版是重言式**：我把篡改施加在**源文件**上，然后又拿这个被篡改的文件当源去
  `verify` —— 「目标 vs 源」变成了「A vs A」。已改为篡改**目标**、参照仍是原源。
- 🟠 **`ownership_boundary_unchanged` 一项**（后加的检查）**没有任何反证用例** —— 后来才加的检查
  是这类漏洞的高发区，因为它天然没有历史对照。已补两个（root 报告被改归某把密钥、
  密钥的报告被改成 root 报告）。现在 **20 项检查每一项都有对应的篡改用例**（Go 侧 18 个）。
- 🟠 **`row_equality[key_renewals]` 是空绿**（真源里这张表 0 行）—— 现在 detail 直接写
  `0 rows — … this check compared nothing`，让空绿在日志里可见。
- 🟠 **`TestVerifyDetectsEachKindOfTamper` 在无库档里是「空绿」**：父测试不 skip、只有子测试
  skip，`go test` 于是打印 `--- PASS` 而实际什么都没断言。已按 storetest 的文档在父测试开头
  调 `SkipWithoutDatabase`。
- 🟠 **`teleport_test.public` 里躺着一整套陈旧 schema**（P4 会话期间 18:08 某次没带
  `search_path` 的迁移留下的），与 P4 交接里「环境干净」的说法不符 —— 那句话说的是
  「只剩 `public` 这个 schema」，不是「它是空的」。已清掉（连同我手工实验留下的 `p5_scratch`）。
- 🟠 **目标自省把视图当成表**：`information_schema.columns` 也描述视图，`v_live_shares` 于是被
  报成「不认识的表」。已改成只读 `BASE TABLE`，并把期望的视图单列出来断言其存在。

**P5 的对抗性实测（自建夹具，不经过任何 Go 代码的字节级比对）**：
- 文本列逐字节相等（emoji / 中文 / 尾随空格 / CRLF / 孤立 CR / tab / 反斜杠 / 引号）：
  源 `hex(title)` 与目标 `convert_to(title,'UTF8')` 的 hex 完全一致。
- BIGINT 极值精确：`created_at = 9223372036854775807`、`updated_at = -9223372036854775808`、
  `view_count = 9223372036854775807`、`request_count = 9223372036854775807` 全部原样。
- DOUBLE PRECISION 位级精确：0.1、最小次正规数 `4.9406564584124654e-324`、
  `0.30000000000000004`、最大有限 double `1.7976931348623157e+308`，
  用 PG 的 `float8send` 与源的精确十进制表示各自算 IEEE754 字节 → **全部 MATCH**。
- **非有限值**：SQLite 能存 `9e999`（= Inf），而 `sqlite3 -json` 会打出**非法 JSON**（裸 `Inf`）
  → 导入**响亮失败**（不是静默写坏值）。这是已接受的行为（错误信息在 pitfall 里记了原因）。
- 唯一的静默失败就是上面 🔴 那条超范围整数 —— **修好之后，本节的每一项都变成红或明确拒绝**。

**P5 第二轮：一次独立审查（`subagent` reviewer）提出的 8 项，已逐条处理。**
结论：无 BLOCKER，但其中两项是「20 项检查仍可能对**被改过的数据**报绿」的洞，必须在不可逆的
生产搬运之前补掉 —— 已补。

- 🟠 **文本比较的两侧出自同一条有损管道**（`sqlite3 -json` + `encoding/json`；Go 的 JSON 解码器会把
  **非法 UTF-8 替换成 U+FFFD**），于是含非法字节的文本会被**两侧同样替换**、比较相等、报绿。
  **已修**：`db_rendered_text` 升级为 **`db_hex_bytes`**，比的是**字节**
  （源 `lower(hex(col))` vs 目标 `lower(encode(convert_to(col::text,'UTF8'),'hex'))`），
  两侧都由各自的数据库产生。**实测真源 0 个文本格子含非法 UTF-8** —— 是潜在洞而非活洞。
- 🟠 **DOUBLE PRECISION 完全没有独立检查**（原先故意把 REAL 排除在渲染比较之外），而且夹具里的
  REAL 全是短位数（720.0/24.0/72.0/0），**闸门抓不到精度丢失**。审查者建议的修法
  （`hex(col)` 取 IEEE754 字节）**实测不成立**：`hex(0.1)` 给的是 `302E31`（"0.1" 的文本字节），
  不是 `3fb999999999999a`。**实际修法**：`source.go` 里 REAL 列改读 SQLite 自己的
  `printf('%!.17g', col)`，并用 `CASE WHEN typeof(col)='real'` 保护（否则 `printf` 会把 TEXT
  强转成数字：`'abc'`→`0.0`，那是自己引入的静默损坏）→ **JSON float 路径不再参与数据搬运**；
  夹具换上一个真正需要 17 位的值 `0.30000000000000004`，新增 `TestImportPreservesRealPrecision`
  （用 PG 的 `float8send` 比 IEEE754 字节）。顺带修正一处**我自己写错的注释**：原文称
  「SQLite 的 CAST AS TEXT 用 15 位有效数字」—— 实测是**最短往返**的，见 pitfall 的更正文。
- 🟠 **`beginTx` 的断言其实拦不住事务**：pgx v5 的 `Tx` 接口**自己就有 `Begin`**（伪嵌套事务 =
  savepoint），所以 `Begin(ctx) (pgx.Tx, error)` 这个方法集 **`*pgxpool.Tx` 也满足**，于是「传事务
  进来」会被接受并**静默开 savepoint**，而不是按注释说的失败。**已修**：显式断言
  `*pgxpool.Pool`；新增 `TestImportRefusesATransactionNotAPool`，并**反证过**
  （换回弱断言 → 该测试变红：「Import accepted a transaction」）。
- 🟡 **`row_equality` 的列分隔符可被值伪造**：注释声称「控制字符不可能出现在这些列里」——
  对 `content`/`note`/`purpose` 这类任意文本而言是**假的**；两行可能拼出同一个字符串而比较相等，
  且失败信息会指错列。**已修**：改成**长度前缀**拼接（`len:cell`），不可伪造，
  `firstDifference` 现在也能指出正确的列；新增 `TestJoinCanonicalRowCannotBeForgedByAValue`。
- 🟡 **`metadata` 为 `'5'` / `'[1,2]'` 时 `report_content` 会对逐字节正确的副本报错**（两库的 CHECK
  都只要求「合法 JSON」；store 的 `decodeMetadata` 一律降级成 `{}`，而 `canonicalJSON` 保留了
  `'5'`）。**已修**：`canonicalJSON` 改为**完全镜像 store 的读法**；旧断言「非法 JSON 必须报错」
  随之**显式改掉**（行为是故意改的，测试注释里写明了理由，row_equality 仍比原始字节）。
- 🟡 **「空目标检查」的注释夸大了它做的事**：`SELECT count(*)` 只拿 `AccessShareLock`，
  **不阻塞**并发写者，所以「并发写不可能插入」是假的；真正兜住的是提交前的重数，
  而**保计数的**并发改动（删一条 + 插一条）根本发现不了。**已修**：注释改成事实，
  并写明 target 必须是空闲的、刚迁移的库。
- 🟡 **打印的 sha256 未必描述被导入的字节**（CLI 先哈希、`Import` 再读一次）。**已修**：
  导入后**再哈希一次**，不一致就非零退出并提示用 `-replace -yes` 重来。
- 🟡 **`scripts/verify-p5.sh` 四处**：① `P5_SKIP_NEGATIVE=1` 时仍打印「every check can fail」
  → 已按实际情况措辞；② `P5_SCHEMA` 无守卫地插进 `DROP SCHEMA ... CASCADE`
  （`P5_SCHEMA=public` 会删掉 `teleport_test.public`）→ 已加标识符与保留名校验；
  ③ DSN 把口令放进 argv（共享主机上任何 `ps` 都看得到；且口令含 `@:/?#` 会静默拼出另一个 DSN）
  → **已改成 DSN 不带口令 + `export PGPASSWORD`**（pgx 从环境读；两个方向都实测过：
  有它则连上、无它则 28P01）；④ 「用了不同实现读源」的说法不成立（同一条 `sqlite3` 二进制）
  → 措辞已改。
- 🟡 `checkColumnsMatch` 只比列名（类型/可空性靠 `convertValue` 的未知类型拒绝间接兜住）——
  检查名 `column_sets` 本身是准确的，已在注释里写明它**不比类型**。
- ✅ 审查**确认无缺陷**的部分：int64 边界的开闭区间、NULL vs 空串从不混淆、事务形状与
  FK/TRUNCATE 顺序、脚本的退出码纪律。**时钟**：`checkShareSemantics` 与 store 的 `ResolveShare`
  用的是**同方向同边界**的比较（`expires_at <= now`），所以**不会假通过**；唯一的错误模式是
  「恰好在这一毫秒过期」导致的**假失败**；`checkAgentKeys` 把同一个 `now` 传进 SQL 谓词，
  **完全没有窗口**。

**P5 已实测 / 未实测**：
- ✅ 四档闸门：`check.sh`（无库，**154 项 SKIP** 并大声提示）、`test-pg.sh`（强制真库）、
  `verify-p4.sh`（未重跑，P4 之后 Go/前端未改）、`verify-p5.sh`（15/15 + 负向对照）。
- ✅ 真源真库彩排：17 行、5 张表、`expires_at=0` 的 2 个、`is_active=0` 的 2 个，全部对上。
- ✅ 负向对照：把目标里 2 个被吊销的链接改活、2 个永久链接加上过期时间 → 验证**按名字**变红
  （`revoked_links_stay_404` + `permanent_links_stay_permanent`），并且篡改行数被断言 `>= 1`
  （防止空对照）。
- ✅ **20 项**检查（`db_rendered_text[...]` 5 项是新增的独立比对）+ **15 个** Go 侧篡改用例
  （`TestVerifyDetectsEachKindOfTamper`），含「多一列」「改列名」「删视图」「root 归属被改」这类
  **静默丢数据**的形状 —— 每一项检查都有对应的反证用例。
- ⛔ **未做：真的往生产意图库 `teleport` 灌数据**（P6，且要先问人类）。生产库现在仍是 **0 张表**。
- ⛔ **未做：旧版与新版的对账**（旧版已停，ADR 早就记过这个代价）。
- ⛔ **未做：搬运后的大陆观测点/浏览器验证**（属 P6）。
- ⚠️ **未验证的假设**：源的 `content`/`metadata` 里没有 NULL、没有 BLOB（有 typeof 层面的检查，
  但真源里本来就没有）；`sqlite3` shell 版本（3.53.4）之外的行为未测。

**Updated:** 2026-10-16
**目标：** 把 teleport 重写到 Gin + pgx/sqlc + PostgreSQL，**在野契约逐字节不变**，
然后部署到 hpcore（本机），让 hcyj 只剩转发。

---

## 线上现状（2026-10-17 **切换后**实测）

| 事实 | 值 |
| --- | --- |
| teleport 的公网入口 | **`https://teleport.hcyj.xyz/`（根路径）** —— 自有域名，与 clip 形态一致 |
| 现在是什么状态 | **在线**。hpcore 上 `teleport.service`（systemd，用户 `relay`）`active`+`enabled`，监听 `127.0.0.1:8788`；`frpc-relay-teleport`（user unit）把 hcyj 的 8788 隧道到这里；hcyj 的 Caddy 终结 TLS 并反代到 `172.17.0.1:8788` |
| 证书 | **Let's Encrypt**（`CN = teleport.hcyj.xyz`，2026-10-17 签发，2027-01-15 到期），与 api/clip 同一个 CA —— 没有引入新依赖 |
| 数据 | 生产库 `teleport`：5 报告 / 6 分享链接 / 2 密钥 / 4 申请 / 0 续期，与源**逐字节一致**（不可变列已比对；只有使用计数因真实访问而变化） |
| 旧前缀 `api.hcyj.xyz/yeciorez/teleport*` | **410 Gone** + `teleport moved to https://teleport.hcyj.xyz/`（已裁决的 break change，见 `2026-10-16-p6-single-root-entry.md`） |
| `https://api.hcyj.xyz/` 的 200 | 那是 Caddy 的 fallback `handle` → `172.17.0.1:3000`（**别的服务**），从来不是 teleport |
| 未受影响 | `clip.hcyj.xyz` 与 `api.hcyj.xyz` 的兜底服务都仍 200（改 Caddy 时逐项验过） |
| 旧版回滚材料 | hcyj 上 `teleport` unit 仍 `disabled`+`inactive`，`teleport.db` 与 `teleport.prev-*` 都在（**别删**） |

大陆观测点：**hpcore（家宽）与 koma 各测一次**都通过（`/api/health` 200、永久链接 200、
已吊销 404、旧前缀 410；h2 + 证书链校验通过）。`bjbuwe` 当前不可 ssh。

**新域名 `teleport.hcyj.xyz` 已裁决**（见 `2026-10-16-teleport-own-hostname.md`）：
新版挂在**根路径**，**P6 与切换一起做**（现在不加 Caddy，否则新域名也是 502）。

---

## 现在这个仓库是什么

2026-10-16 从共用仓库里拆出来：用户裁决「分成两个仓库 不要塞在一个项目里面去了」。
本仓库用 `git archive HEAD` 从共用仓库导入了**线上正在跑的那一版**（提交 `a288b15`），
环境是**忠实副本**，不是重构后的起点 —— 这样 243 个测试与 23 条 pitfall 守卫就是重写的规格说明书。
（clip 在它自己的仓库里，已经上线；两边共享的只有 hcyj 的 Caddy、本机 Postgres 实例、frp 机制。）

**本次导入**：`backend/`（net/http + SQLite）、`web/`（Vue 面板，不重写）、`schema.sql`、
243 个测试函数、`.ai/` 全量记忆库。导入提交 `ef71255`。

## 决策（先读这个）

**`.ai/decisions/2026-10-16-rewrite-on-gin-and-postgres.md`** —— 本仓库的纲领：

1. **第一张表是冻结清单**（14 项在野契约：agentkey 派生标签、cookie/头名、路由前缀、
   封套与命名、404/410 语义、归属边界、epoch 毫秒、续期规则、markdown 与 `format:"html"`、
   一次性凭据的 fragment 规则）。改任意一项都会让**已发出的密钥或在野链接失效**。
2. 数据库：同实例、独立 database `teleport`，时间列仍是 `BIGINT` epoch 毫秒。
3. 包映射表 + `database/sql → pgx` 的四类语义差异（`ErrNoRows`、`OnConflict`、
   SQLite 专有函数、**失去单写者保证后必须显式加锁的三处**）。
4. 分阶段 P0–P6，每阶段结束都要能跑旧测试。
5. 验收：243 个测试对**真库**全绿 + 契约逐条实测 + 真实浏览器 + 大陆观测点 + 数据搬运核对。

## 下一步（严格按顺序）

| # | 任务 | 为什么排在这里 |
| --- | --- | --- |
| **P0** | ✅ 清点完成 + 补上缺口：审计发现 **agentkey 的三个派生标签零测试覆盖**（最危险的一条），已加 4 个守卫（标签字面量 + 黄金字节 + 独立重算 + 续期域分离），并做了两次反证确认会变红。剩下的 11 项契约已由现有测试钉住（见下） | 重写期间的每一次"绿"必须是同一个判据下的绿 |
| **P1** | ✅ 完成（**附更正，见下**）：库/角色 `teleport`（非超级用户）、`migrations_pg/0001_init.sql`（合并式基线）、生成的 `schema.pg.sql`、`MigratePG` 运行器、8 个新测试（在真库 `teleport_test` 上验证 4 处**必须改对**的转换）、两道漂移守卫 | schema 是后面一切的接口 |
| **P1.1** | ✅ 更正：P1 记的「在真库上验证」当时**已经不成立** —— `teleport_test` 是**未提交的中间版本**建的库，合并式基线按文件名记录、永不重放，于是库里留着 `CHECK ((metadata)::json IS NOT NULL)` 而文件里是 `CHECK (metadata IS JSON)`，`TestPGMetadataCheckRejectsInvalidJSON` 实际是红的（期望 23514，得到 22P02）。已 DROP/CREATE 重建 `teleport_test` 并复跑 → 8/8 绿。两道漂移守卫是**文件对文件**，看不到真库。见 pitfall `2026-10-16-merged-baseline-hides-a-stale-test-db.md`。生产意图库 `teleport` 当时仍是空的，P5/P6 未受影响 | 「绿」必须真的对应当前文件；否则后面每一次绿都不可信 |
| P2 | `internal/store/` 换成 pgx + sqlc —— **注意：SQLite 的单写者保证在 PG 里消失，计数—插入式的上限必须显式加锁**。**做法已裁决为 B（一次性大爆炸）** | 存储语义最容易在不知不觉中变 |
| ↳ **P2.1** | ✅ 测试夹具 + 闸门：`internal/store/storetest`（每测一个 schema，`search_path` 锁定，用完即拆）、`scripts/test-pg.sh`（无 DSN 拒绝运行 + 自检守卫）、`check.sh` 在跳过 PG 时大声提醒。7 个新测试（3 个夹具守卫 + 3 个上限锁验证 + 1 个旧真库测试改用夹具） | 没有可信的夹具，后面每一次「绿」都不可信 |
| ↳ **P2.2** | ✅ 锁定设计已验证：`pg_advisory_xact_lock` 的三个键（`pg.go`）。确定性实验证明 READ COMMITTED 下 cap=1 能落 2 行（不是假设）；锁能把第二个事务真的擋住并使 cap 精确，24 竞争者×5 轮 → 每轮恰好 3 个赢 | 先有靶子再改实现，否则锁加错地方也看不出来 |
| ↳ **P2.2b** | ✅ 迁移内容哈希守卫：`schema_migrations` 记 sha256，库与文件不符就拒绝启动（含旧迁移器建的库）。4 个测试 + 端到端反证（改文件→拒绝，撤销→通过，文件与 HEAD 逐字节相同） | 把 2026-10-16 那次事故从「靠守卫事后发现」变成「源头不可能发生」 |
| ↳ **P2.3** | ✅ 完成：`queries.go` + `keys.go` 改 pgx；sqlc 生成 `storedb`（两张动态 `SET` 列表用 `COALESCE(sqlc.narg(...))` + `CASE` 表达为单条静态语句——ADR 的 Revisit 条款担心的那点已被实测证明可行）；三处上限用已验证的 advisory lock；`SELECT ... FOR UPDATE` 未被采用（无法锁不存在的行）。断言未为了变绿而重写：唯一改断言机制的一处是 `TestNoPlaintextCollected` 的 `sqlite_master` → `information_schema`（PG 无 sqlite 元数据表），**被断言的性质（明文永不入库）原样保留** | P2 的主体 |
| **P3** | ✅ 完成：Gin 路由表 + `gin.HandlerFunc` handler（`c.Param`）；`internal/httpx` 的认证中间件用 `httpx.GinMiddleware` 适配成 Gin 中间件；信封/session/principal 仍是框架无关的纯函数。**零测试改动**通过。决策与 gin 版本证据见 `2026-10-16-p3-gin-integration.md` | 封套与状态码逐条对齐 —— 已逐条验证 |
| **P4** | ✅ 完成：全链路起新版（Gin + pgx + 内嵌 SPA + 真 PG），HTTP 契约 **64/64** + 真实浏览器 **15/15**；另做**差分跑**（P3 之前的树 vs 新版，108 请求 / 1 处已知差异）。第三档闸门 `scripts/verify-p4.sh`。**未做：大陆观测点（要等 P6 切换后）** | 切换前的最后一道闸 —— 已过，但外部校验（P5/P6）仍在 |
| **P5** | ✅ 完成：`internal/sqlitemigrate` + `cmd/sqliteimport`（**单事务、默认拒绝非空目标、`-replace` 需 `-yes`**）+ `scripts/verify-p5.sh`（第四档闸门）。**schema 由应用自己的迁移器建，工具从不发 DDL；P5 不建生产库的 schema**（决策记录 `2026-10-16-p5-schema-ownership.md`）。真源真库彩排 15/15 + 负向对照按名字变红。**未做：真往生产库灌**（属 P6） | 切换当天不能靠猜 —— 现在不用猜了，但**生产的第一次仍然在 P6** |
| **P6** | ✅ **完成（2026-10-17 执行）**：线上切到 hpcore 的新版（Gin + pgx + PostgreSQL），入口是 **`https://teleport.hcyj.xyz/`（根路径）**；hcyj 只做 TLS 终结与转发（frpc 隧道 8788）。旧前缀 `api.hcyj.xyz/yeciorez/teleport*` → **410**（已裁决的 break change）。两个大陆观测点验证通过；旧站点 `clip.hcyj.xyz` 与 `api.hcyj.xyz` 兜底都未受影响。**完整执行记录与三处偏差见 `.ai/runbooks/p6-cutover.md` 第七节**。⛔ 唯一未验的是面板登录（需管理员明文口令，我只有哈希） | 用户要的最终形态已达成：hcyj 只转发，teleport 有自己的域名且在根路径 |

**保持冻结的线上环境**：重写期间**不要**改 hcyj 的 Caddy、不要升级本机 Postgres、
**绝对不要轮换 `SESSION_SECRET`**（它会让所有派生密钥失效）。

## 未做 / 需要人类

- **P5 已裁决并已执行**（`.ai/decisions/2026-10-16-p5-schema-ownership.md`）：schema 由应用迁移器
  （`store.OpenPG` → `MigratePG`）建，搬运工具**从不发 DDL**；**P5 不建生产库 schema**，
  生产 `teleport` 保持 0 张表到 P6；读旧库用 `sqlite3` shell（**零新增依赖**）。
  P6 的顺序是 `migrate` → `import` → `verify`（都在同一个窗口里，不存在「提前建好却静默变旧」的间隔）。
- **P6 已执行（2026-10-17，用户批准「改 caddy 挂一会没事的」）**：切换、Caddy 改动、frpc 隧道、
  生产库灌数据全部完成并验证。P5 期间没有碰线上，这次是用户明确批准后才动的。
  回滚材料都还在：hcyj 的 `teleport` unit 仍 `disabled`+`inactive`，`teleport.db` 与
  `teleport.prev-*` 未删；hpcore 上旧二进制是 `/data/services/teleport/teleport.prev-*`，
  旧 env 是 `teleport.env.pre-p6-*`，hcyj 的旧 Caddyfile 是 `Caddyfile.pre-p6-*`。
- **P6 的实际工作量比纲领预想的多**（只读侦察实测，细节见 runbook）：hpcore 上**没有** teleport
  的 systemd unit（要新建，模板是 `clip.service`）；`/data/services/teleport/teleport.env` **仍是
  旧配置**（含 `DB_PATH`、没有 `DATABASE_URL`）；frpc 是 **user unit**（用户 `oicey`），
  要照抄 `frpc-relay-clip` 的形式；hcyj 的 frps **没有 `allowPorts` 限制**（8788 隧道会被接受）；
  DNS 已解析到同一 IP，只差 site block + 证书；`clip.hcyj.xyz` 的 block 是现成模板。
- **🔴 改 Caddyfile 是 P6 最危险的一步**：写错会让 hcyj 的 **443 整体不可用**，连带 clip 与
  `api.hcyj.xyz` 上别的服务。所以 `caddy-routing-on-shared-host.md` 的校验三步（备份 →
  `docker cp` + `md5sum` 对比 → `caddy adapt`）不能跳。
- **P5 的源获取方式（P6 会重跑一次）**：`P5_FETCH=1 bash scripts/verify-p5.sh` 会
  `ssh hcyj sha256sum` → 检查无 `-wal/-shm/-journal` → `scp` → 本地再 `sha256sum` 比对。
  已实测的源：`sha256 0b3855d7b130e30d7ca2128afce7c9dd30dfaf7eba5f7a57293347a2cbbeb24d`
  （P6 建议用 `P5_EXPECT_SHA256=` 把它钉住）。**只读**：hcyj 上没有任何写入。
- **P5 未做 → P6 已补**：真往生产库灌（已做，20/20）、大陆观测点（已做，两个观测点）、
  真实浏览器（已在公网新域名上跑，见 runbook）。**旧新对账仍未做**（旧版已停，ADR 早记过这个代价）。
- ⚠️ **`sqliteimport verify` 在活库上会报使用计数差异**：它比对 `view_count`/`request_count`/
  `last_used_at` 这些**可变列**，而库一旦开始服务这些列就会偏离冻结的源 —— 那不是穿透，是流量。
  P6 切换后就实测到这一条（`view_count` 7→11，因为验证时真的访问了那个永久链接）。
  活库上应只比**不可变列**。
- **⚠️ P5 的独立复审没拿到（要如实知道）。** 四种尝试全部因**环境**失败：
  `bg_delegate` × 2 在 35 次工具调用后被 24 turn 上限截断（`child_turn_limit`，未交答案）；
  `fusion_validate` × 2 的三个候选各自读了文件后以**空响应**告终
  （`session_shutdown before agent_settled` / `Pi assistant response is empty`）。
  因此本次提交背后的证据是 **一个作者的对抗性反证 + 机械可证伪性**（20 项检查各有篡改用例、
  负向对照必须按名字变红、范围检查 revert 后必须变红），**不是独立审查**。
  补做的 `subagent` 审查（另一种机制）在会话末尾启动，结果见其后的提交/交接。
- **P5 已提交并推送**：`06cefbe`（`de22443..06cefbe`，分支 `main`）。**未部署**。
- **P2 测试策略已裁决**（`.ai/decisions/2026-10-16-p2-test-strategy.md`）：B —— 每测一个 schema，
  从内嵌迁移现建现拆；闸门两层（`check.sh` 可跳过 DB，`test-pg.sh` 强制）。
- **P2.3 做法已裁决并已执行**（`.ai/decisions/2026-10-16-p2-store-port-technique.md`）：**B 一次性大爆炸**。
  5 项缓解均已落地：① 断言冻结——唯一改断言**机制**的是 `TestNoPlaintextCollected`
  （`sqlite_master` → `information_schema`，因为 PG 没有 sqlite 元数据表），
  **被断言的性质（明文永不入库）一字未变**；② 冻结契约测试随全套件对真库绿；
  ③ 三处上限用已验证的 advisory lock；④ 未用 `SELECT ... FOR UPDATE`；
  ⑤ **P4 已做**（契约 64/64 + 浏览器 15/15 + 旧树差分 108 请求 / 1 处已知差异）；
  **P5/P6 仍是外部校验，尚未做**。
- **P2.3 已实测 / 未实测**：
  - ✅ `env -u TEST_PG_DSN bash scripts/check.sh` 通过（含 gofmt / vet / schema 快照 / sqlc 漂移）。
  - ✅ `bash scripts/test-pg.sh` 通过（强制真库，且自检夹具仍拒绝非 test 库）。
  - ✅ `go test ./...` 对真库 0 失败（含 `internal/api`、`internal/store`、`internal/config`）。
  - ✅ `sqlc generate` 成功、`sqlc diff` 无漂移；两张动态 `SET` 列表确实可用
    `COALESCE(sqlc.narg(...))` + `CASE` 表达为单条静态语句 —— ADR 的 Revisit 条件未触发。
  - ✅ **P4 已补上**：HTTP 端到端契约实测已在真二进制上做完（`scripts/verify-p4-contract.sh`，64/64），
    含真 agent 密钥 POST / 读分享页 / PATCH / DELETE / 过期 410 / 未知 404 / `HEAD` 领取 405。
  - ✅ 真实浏览器已做（15/15）。⛔ **未做：大陆观测点（P4 全程 loopback）、数据搬运核对、部署**
    —— P5/P6。
- **P2.3 期间修掉的自身缺陷**（已记 pitfall）：`ClaimApplication` 的失败分支里，
  `diagnoseClaim` 在事务未提交时又向**连接池**要第二条连接；测试池只有 4 条、8 个竞争者
  ⇒ 持有一半等另一半，**永久挂死**（pgxpool 无超时）。已改为读事务自己的 query set。
  规则：**事务内只准用 `WithTx` 的 query set**。见
  `.ai/pitfalls/cases/2026-10-16-store-nested-pool-acquire-deadlocks.md`。
- **配置变更需人类知晓**：`DB_PATH` → `DATABASE_URL`（**必填、无默认值**，`Load()` 缺它就报错），
  新增 `DB_MAX_CONNS`（默认 8）。P6 部署时 `teleport.env` 必须提供 `DATABASE_URL`，
  否则新版**启动即失败**，而不是静默连到别的库。该值含口令，**故意不写日志**。
- **域名与入口已裁决**（`2026-10-16-teleport-own-hostname.md`，**约束 2 已被取代**）：
  teleport 拥有 `https://teleport.hcyj.xyz/`（**根路径**，`ROUTE_PREFIX=""`），P6 与切换一起做；
  而**旧前缀 `api.hcyj.xyz/yeciorez/teleport` 已弃用**（用户 2026-10-16 裁决 break change）——
  **`expires_at=0` 的永久链接也随之失效**，这是用户知情选择的结果，理由与后果见
  `2026-10-16-p6-single-root-entry.md`。已核实 agent 密钥与域名无关
  （`DeriveToken` 不吃 URL），所以**凭据不失效，只失效旧 URL**。
  **前端前缀在构建期烧死**：`vite.config.ts` 的 `base = VITE_ROUTE_PREFIX + "/"`（默认仍是旧前缀），
  所以发布构建必须显式 `VITE_ROUTE_PREFIX=/`，并与后端的 `ROUTE_PREFIX="/"` **成对**使用。
  ⚠️ **后端那边要写 `"/"` 而不是空值**：`env()` 把空值当「未设置」→ 回落默认的
  `/yeciorez/teleport`，于是「前端根路径 + 后端旧前缀」→ 白屏。已实测三种写法的启动日志前缀。
  影响面：`ROUTE_PREFIX`、`PUBLIC_BASE_URL`、`FRONTEND_ORIGINS`、`CookiePath`、Vite `base`、
  `/ai.md` 与 `/llms.txt` 里的地址、`format:"html"` 的同源 iframe 判定。
- **更正交接文档里的一处计数**（保留，已按此落地）：需要显式加锁的「计数—插入」实际上有
  **3 处**，不是文档写的「待批准上限 / 密钥上限 / 分享链接计数」：① `CreateApplication` 的
  `KEY_MAX_PENDING`，② `ClaimApplication` 与 `CreateManualKey` 共用的 `KEY_MAX_ACTIVE`，
  ③ `CreateRenewal` 的「每把密钥至多一个待批续期」。**分享链接没有任何数量上限**
  （`RecordView` 只是 `view_count = view_count + 1`，在 PG 里本来就是原子的），照字面找会白找。
  已落地为 `LockPendingApplications` / `LockActiveKeys` / `LockPendingRenewals`（`pg.go`）。
- 仓库在 GitHub 上（`git@github.com:zeroicey/teleport.git`，分支 **`main`**）。
  2026-10-16 核实：当时记忆库里记着「还不存在」，是错的 —— remote 一直有，只是本地领先 7 个
  未推提交；P2.3 已把这些连同本次改动一起推上去（`ddce6b2..e74ebf3`）。
  **本地与远端的分支名是 `main`，不是 `master`**（早期交接文档写成 master，已改正）。
- 线上仍是旧版（hcyj，SQLite + net/http）。**但旧版目前已停掉（disabled+inactive），公网入口 502** —— 用户裁决「等重写版就绪再切」。
- 与 clip 共享的基础设施改动要**同时想两边**（hcyj 的 Caddyfile 同时服务两个站点）。

## 长期约束（改动前必须知道）

- 冻结清单（见决策记录第一张表）**不可改**；改之前必须先改决策记录。
  **路由前缀那一项已于 2026-10-16 改了两次**：先改成「旧前缀必须一直有效」，随后用户裁决
  **放弃旧前缀**（break change）—— 现在 `teleport.hcyj.xyz` **根路径是唯一入口**，
  `expires_at=0` 的永久链接也随之失效（知情接受）。见
  `2026-10-16-p6-single-root-entry.md`（含「为什么一个二进制做不到双入口」的实测证据链）。
- **归属是安全边界**：非归属者的读/改/删一律 404 而不是 403，且判定在任何写入之前。
- **事务内只准用 `WithTx` 的 query set。** 在 `s.pool.Begin` 之后碰 `s.q.` 或 `s.pool.`
  会向池再要一条连接，而自己已占着一条；小池 + N 竞争者 = 无超时永久挂死。
  新增 store 方法时按此 grep 自检。见
  `.ai/pitfalls/cases/2026-10-16-store-nested-pool-acquire-deadlocks.md`。
- **计数—插入必须显式加锁**（PG 没有 SQLite 的单写者）：三处上限用 `lockCap` + 事务级
  advisory lock，计数必须在锁之后、同一个事务里。
- **密钥是派生的，不是暂存的**；服务端只存 `sha256`。
- **一次性凭据只走 URL fragment**，页面无 JS 时惰性，`HEAD` 领取必须 405。
- **封套与命名**：`{ok,data,requestId}`；请求 camelCase、响应 snake_case；未知字段 400。
- **时间一律 epoch 毫秒**；`expires_at=0` = 永不过期，且「永不过期」只能由人类显式给出。
- **限流桶键必须用 `httpx.ClientIP`**，绝不用任何 RealIP 式实现。
- **markdown 报告永不渲染原始 HTML**；`format:"html"` 的原样渲染是**唯一的例外**，有双向绊线测试。
- 大陆可达性是硬约束；验证必须用大陆观测点。
- **Postgres 实例是与 clip / registry / serenique 共享的**（`max_connections = 100` 是所有人
  共用的），不是 teleport 的预算。任何夹具/脚本/压测都不得按「整台实例都是我的」来开连接：
  一个循环里开连接池会瞬间吃掉 100 个槽位，把别人的服务弄成「连不上」而不是「测试失败」。
  见 pitfall `2026-10-16-test-harness-exhausted-shared-postgres-connections.md`。
