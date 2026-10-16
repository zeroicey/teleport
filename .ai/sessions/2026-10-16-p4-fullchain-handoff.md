# teleport — P4 完成（全链路：Gin + pgx + 内嵌前端 + 真库 + 真实浏览器）

**Date:** 2026-10-16 · **Branch/Commit:** `main`（基于 `8b189d8`，**P4 尚未提交**）· **Duration:** 一个会话
· **State:** P0 ✅ / P1 ✅ / P2 ✅ / P3 ✅ / **P4 ✅**；**P5 ⛔ / P6 ⛔**；线上旧版仍 `disabled`，公网 502
· **Gate:** `scripts/check.sh`（无库档）与 `scripts/test-pg.sh`（强制真库）**两档都绿**；
  新增第三档 `scripts/verify-p4.sh`（全链路 + 浏览器）

---

## 一、先读什么

1. `.ai/CURRENT_TASK.md` 的 P4 小节。
2. `.ai/pitfalls/cases/2026-10-16-verification-that-cannot-fail.md` —— **本会话最重要的教训**：
   验证脚本自身必须先被反证，否则「全绿」是表演。
3. 三个验证脚本：`scripts/verify-p4.sh`（编排）、`scripts/verify-p4-contract.sh`（HTTP 契约）、
   `scripts/verify-teleport-browser.cjs`（CDP 浏览器）。

## 二、怎么跑 P4

```bash
cd /data/dev/teleport
export P4_PGPASSWORD="$(sudo -n cat /data/services/teleport/.pgpassword.tmp)"
bash scripts/verify-p4.sh          # 构建 → 起服务 → 契约 64 项 → 浏览器 15 项 → 拆 schema
P4_SKIP_BUILD=1   ...              # 复用已有二进制
P4_SKIP_BROWSER=1 ...              # 只跑 HTTP 契约
P4_PORT=8799      ...              # 换端口（预检会拒绝已被占用的端口）
```

日志与截图留在 `bin/e2e/`（`p4-contract.log`、`p4-browser.log`、`p4-server.log`、`teleport-0N-*.png`）。

**它不碰线上、也不碰生产意图库**：它在 `teleport_test` 里建一个私有 schema `p4`（`search_path` 注入 DSN），
跑完就删。**不新建数据库** —— `teleport` 角色的 `rolcreatedb=false`（实测），
而在与 clip/registry/serenique 共用的实例上为了一次冒烟测试去授 `CREATEDB` 是不划算的。

## 三、P4 证明了什么（结果）

| 档 | 结果 |
| --- | --- |
| HTTP 契约（真二进制、真 socket、真 PG） | **64 / 64** |
| 浏览器（headless Chromium，CDP） | **15 / 15** |
| `scripts/check.sh` | 绿（147 项 SKIP，因为无 DSN） |
| `scripts/test-pg.sh` | 绿 |

浏览器档证明的是只有浏览器能证明的东西：Vue bundle 真的执行并挂载、路由守卫把匿名访客送到登录表单、
**真实登录**后拿到作用域为前缀的 `teleport_session` 并加载出数据（`rows=4`）、分享页是服务端渲染
（不是 SPA 壳）、**Mermaid 在真实 CSP 下渲染成 `<svg>`**（且页面自己的 `mermaidErrors` / `mermaidLoadError`
通道为空）、没有意外的 console/网络错误。

## 四、独立差分证据（本会话最有价值的一块）

用 `git archive 43cea51`（**P3 之前的树**，net/http HTTP 层 + 同一套 pgx store）构建出旧二进制，
与新版在**同一请求集**上逐条对比（两个独立 schema、同一套环境变量、同一套密钥）：

> **108 个请求，只有 1 处不同。**

那一处是 `GET {前缀}/api%2Fhealth`：**两者都是 404**，只是 body 不同（新：JSON 信封；
旧：net/http 的 `text/plain` "404 page not found"）。这正是 `rejectEncodedSlashes` 的既定行为
（ADR 里记过），状态码一致，属于更一致的改法。

被这条证据**排除**的两个候选发现（子代理提出，实测证明是**两版相同**，即既有行为、不是 P3 回归）：
- `PUT/TRACE/PATCH/CONNECT` 打到 `/ai.md` → **两版都是 405**（其他路径两版都是 404）；
- **2 MB body → 两版都是 400**（超限的 *content 字段* 才是 413，那条有冻结测试
  `TestContentSizeLimit` 盯着；两者是不同的路径）。

差分集还覆盖并确认一致：错误路径信封（含 `requestId`）、`/ai` `/ai.md` `/llms.txt` 的
Content-Type、路径清洗与尾斜杠（`//`、`../`、裸前缀、裸 `/api`、`/s`、`/k`）、`/k/a/b` 子树、
所有权 404-not-403（读/改/删）、handoff fragment、**HEAD claim 405 + `Allow: GET`**、
一次性领取与二次领取、续期只延长（72h）、以及**两版都没有 `Server` / `X-Powered-By` 头**。

## 五、已验证 / 未验证

- [x] 三档全绿；`-race` 干净（P3 时验过，P4 未改 Go 代码）。
- [x] 独立差分：108 请求 / 1 处已知差异。
- [x] 环境干净：`teleport_test` 只剩 `public`；生产意图库 `teleport` 仍是 **0 张表**；
      clip 的 8790 未被碰过；无残留进程。
- [ ] **假定、未测：大陆观测点。** P4 全程在本机 loopback，没有经过公网。
      ADR 的验收第 4 条要求大陆观测点，那只能在 P6 切换后做。
- [ ] **假定、未测：`bin/e2e/teleport-0N-*.png` 截图我没有逐张目视**（当前模型不支持看图）。
      结论来自 DOM 断言（`#app` 有子节点、`table.data-table` 有行、`.mermaid svg` 存在），
      断言比截图强，但截图本身**未经人眼确认**。
- [ ] **假定、未测：`ENVIRONMENT=production` 下的 Gin release 模式**（本机跑的是 development）。
- [ ] **假定、未测：`format:"html"` 的逐字节比较用的是 `$(...)`**，命令替换会吃掉尾部换行；
      真要用字节级证据得写文件后 `cmp`。`raw` 字符串本身没有换行，所以结论大概率不变，但严格说未证。
- [ ] **假定、未测：合同 14（明文永不入库）在 P4 只从 API 侧间接覆盖**；DB 侧由 store 的
      `TestNoPlaintextCollected` 盯着（P2.3 时绿）。
- [ ] 审查子代理中 **reviewer 那份返回了 0 行**（它的目标树被我并发编辑改动，输出被截断），
      所以「验证脚本本身的质量」只经了一轮（oracle）而非两轮审查。

## 六、Loose ends

- [ ] **P4 尚未提交**（三个脚本 + 1 个 pitfall + 本文档 + CURRENT_TASK）。
- [ ] `bin/e2e/` 是 gitignore 的，但 `scripts/verify-*` 三个脚本**应该提交**（它们是可复用的验证工具）。
- [ ] `scripts/verify-p4.sh` 的 cleanup 会在进程被 `kill -9` 时留下服务（我这次就遇到一个残留进程占着
      8799）。预检守卫能让下一次运行**明确报错**而不是静默跑错对象，这已经够了，但值得知道。
- [ ] 端口 8790 是 **clip** 的。别再选它（我第一版差分就误打了 clip，得到一个假的 404 结论）。

## 七、不要重复

1. **验证脚本必须先被反证。** 本次在验证脚本里改掉了 8 个 bug，其中至少 4 个是「本该失败却通过」
   （空变量、极性写反、断言实现细节而非契约、`Network.getCookies` 查错作用域）。
   做法：拿一个**故意坏掉的输入**（无前端、残留服务、空变量）跑一遍，看它是否真的红。
2. **别把「测试全绿」当成「行为没变」。** P3 的 47 个测试全绿时，文档里的主入口 URL 已经变成 404 了。
   换路由/framework 必须做**旧树 vs 新树的差分跑**（见 §四）。
3. **长 heredoc 的 shell/python 编辑会静默不落地**（本会话 3 次）。写完必须 `grep` 验证字节。
   同理：`\`` 在单引号里不是转义，`export X="$(cmd)"` 会吞掉退出码，`$(...)` 会吃掉尾部换行。
4. **同一个作者写代码和写检查 = 共享的误解会静默通过。** 断言要对着**契约**写，不是对着实现细节
   （`data-processed`、`raw HTML omitted` 都栽在这上面）。

## 八、下一步

**P5：SQLite → PG 的数据搬运 + 行数核对**（5 报告 / 2 密钥 / 全部分享链接）。
旧数据仍在 hcyj 的 `/data/services/teleport/teleport.db`（只读获取，别改）。
搬之前先决定：**是否把生产意图库 `teleport` 建起来**（现在 0 张表，`migrate` 会自己建 schema）。

**P6 才碰线上**（hcyj 的 Caddy + frpc 隧道 + `teleport.hcyj.xyz`），
并且**必须先问人类**确认切换窗口 —— 我不会自己动手。
