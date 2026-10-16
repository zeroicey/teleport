# teleport — P3 完成（HTTP 层换成 Gin）+ 差分复审挖出的三个真回归

**Date:** 2026-10-16 · **Branch/Commit:** `main`（基于 `43cea51`）· **Duration:** 一个会话的下半段
· **State:** P0 ✅ / P1 ✅ / **P2 ✅ / P3 ✅**；**P4 ⛔ 未开工**；线上旧版仍 `disabled`，公网 502
· **Gate:** `scripts/check.sh`（无库档）与 `scripts/test-pg.sh`（强制真库）**两档都绿**；`-race` 干净

---

## 一、先读什么

1. `.ai/decisions/2026-10-16-p3-gin-integration.md` —— P3 的做法、gin 版本证据、
   **「Divergences found by differential review, and fixed」那一节**（本会话最重要的内容）。
2. `.ai/pitfalls/cases/2026-10-16-router-swap-changed-the-wire.md` —— 换路由为什么必须做差分跑。
3. `.ai/CURRENT_TASK.md` 的 P3 小节。

## 二、本会话交付了什么

| 交付 | 位置 |
| --- | --- |
| Gin 路由表 + 39 个 `gin.HandlerFunc`（`c.Param`） | `backend/internal/api/router.go`、`keys.go` |
| net/http 中间件 → Gin 中间件适配器（含 context 回写、拒绝时 Abort） | `backend/internal/httpx/gin.go` |
| ServeMux 语义的**显式补回**：307 路径清洗/尾斜杠重定向、`/k/` 子树、拒绝 `%2F` | `router.go`（`serveMuxRedirect`/`cleanPath`/`rejectEncodedSlashes`） |
| 6 个新守卫（全部反证过） | `internal/api/router_test.go` |
| gin 版本钉在 **v1.10.1**（避开 mongo-driver + quic-go） | `backend/go.mod` |

**测试数**：`internal/api` 47 → 53 个测试函数；**既有断言零改动**（P3 的完成判据）。

## 三、已验证 / 未验证

- [x] `internal/api` + `internal/httpx` **零测试改动**通过；两档闸门绿；`-race` 绿。
- [x] **路由表逐条机械比对**：旧 40 条 method+path **全部保留**，新增的只有 20 条 GET 的 HEAD 孪生。
- [x] **差分复审**（`git archive HEAD` 旧树 vs 新树，57 请求 × 2 夹具 = 114 次，比对状态+15 个头+body）：
      发现 12/57 形状不同，其中 3 个是真回归，已修（见 ADR 那一节）。
- [x] 新守卫全部**反证过会红**：删 HEAD 注册 → 2 个红；禁用三处修复 → 12 个子测试中 11 个红。
- [x] **第二轮复审**（reviewer 子代理，只读）在已推的移植上又找出 1 个真 bug + 若干注释/文档漂移：
      `redirect307` 手写 `Location` 头时**丢掉了查询串**（旧实现用 `http.Redirect` + `urlFromEscaped`
      保留它，并带 `text/html` 与 67B body）。已改为照 `http.Redirect` 做，实测与旧实现逐字节一致；
      查询串用例已进永久守卫。注释/ADR 漂移（HEAD 守卫的机制说法、39 处样板注释、ADR 的
      `gin.WrapH` 与「外层链未变」）已一并修正。复审同时确认：路由表逐条无误、`c.Writer` 与旧 `w`
      等价、没有 `go func` 持有 `*gin.Context`（无数据竞争）、四个 `applyReportPatch/Delete` 调用点
      都不会传空 id、无死代码。
- [x] Gin 不加任何响应头（逐个 header 检查）；`gin.New()` 确认没装它的 logger/recovery。
- [x] 压缩仍生效（3289 → 1516 字节，`Content-Length` 正确丢弃，gzip 精确还原）；
      `c.Abort()` 真的挡住 handler（坏凭据 → 401 且 0 行写入）。
- [ ] **假定、未测：P4 全链路端到端**（新二进制起 Gin+pgx、真实浏览器、大陆观测点）。
      现在绿的是「冻结断言对新 HTTP 层绿」，**不是**「在野契约端到端绿」。
- [ ] **假定、未测：完整响应头集合**（复审只比对了 15 个常见头）。
- [ ] **假定、未测：真实 net/http 服务器下的行为**（测试用 `httptest.NewRecorder`，
      HEAD 的 body 不会被 strip；路由行为不受影响）。
- [ ] 复审是子代理（oracle/reviewer）做的，结论我逐条复核并实测过；复审本身未再被第三方审计。

## 四、Loose ends

- [ ] **未提交**（本会话结束时）。
- [ ] 路由表的 `get` helper 是「GET 必须成对注册 HEAD」的唯一保障；新增 GET 路由时**必须**用它，
      否则 HEAD 会 404（`TestHeadIsRoutedLikeGet` + 冻结的 `TestKeyHandoffClaimRefusesHEAD` 会抓）。
- [ ] `ServeMux` 与 Gin 的差异**只在实测覆盖的形状上**被补回了。若有别的形状（大小写、
      分号参数、超长路径、`CONNECT`、`*` 通配）尚未枚举 —— 复审明确列了它没检查的部分。
- [ ] 传输链仍包在引擎外面（设计如此）。P4 若要 SSE/流式，需重看 `Compress` 的 `Flush` 退出路径。
- [ ] `internal/httpx` 现在依赖 gin（仅 `gin.go` 一个文件）。若想让 httpx 保持框架无关，
      可把适配器移到 `internal/api`，但 ADR 选择把它留在 httpx。

## 五、不要重复

0. **手写重定向头会把查询串和 body 一起丢掉。** 用 `http.Redirect`（ServeMux 自己就是调它），
   并且按 `urlFromEscaped(path, RawQuery)` 拼目标。对照旧实现实测：不只是状态码，
   `Location`、`Content-Type`、body 长度都要比 —— 否则会在“看起来对”的地方留下真 bug。
1. **换路由/framework 只跑测试套件是不够的。** 47 个测试全绿的同时，文档里的主入口 URL
   变成了 JSON 404、一个原本被拒的 `%2F` 请求变成了 200。测试是「前人想到过要断言的行为」的
   证据，不是语义的证明。**做法**：`git archive HEAD` 出旧树 → 同一个探针文件驱动两边 →
   比对 status + headers + body。成本约一次会话的十分钟，收益是这类回归一个都跑不掉。
2. **不要照抄「ServeMux 从不重定向」这种记忆。** 实测：旧实现**用 307** 重定向了
   `//api/health`、`/api/../api/health`、裸前缀、`/api`、`/s`、`/k`。ADR 第一版把这句写反了，
   差分跑才发现。**结论要来自测量。**
3. **Gin 的 `RedirectTrailingSlash` 发 301**（会改变 POST 的方法），所以它正确地被关掉了；
   但「关掉」不等于「不需要重定向」—— 要用 307 自己补回来。
4. **Gin 匹配的是解码后的路径**，`%2F` 会被当成分隔符。拒绝必须在**路由之前**（外层链），
   放在 NoRoute 已经太晚（那时路由已经匹配成功了）。
5. **Gin 不会把 HEAD 路由给 GET handler**，而冻结契约里唯一一处真 405 正是靠 HEAD 到达 handler。
   新增 GET 路由务必走 `get()` helper。
6. **gin v1.11.0 起会链入 `mongo-driver/v2/bson` 与 `quic-go/http3`。** 钉 v1.10.1。
   `go mod why -m <module>` + `go list -deps .` 是确认「到底有没有被编进二进制」的方法
   （只看 go.mod 会误判）。

## 六、下一步（第一个动作）

**P4**：全链路起新版（`cmd` 级：Gin + pgx 一起跑），跑全套测试 + 真实浏览器 +
**大陆观测点**验证线上 URL。切换前的最后一道闸。

具体建议：先 `pnpm run build:release` 出一个包含前端的二进制，在本机对着（空的）
`teleport` 库起一次，用浏览器手点一遍面板与分享页，再按 P5 搬数据、P6 切 Caddy。
**P4 之前不要碰线上环境**（hcyj / frps / Caddy 保持冻结）。
