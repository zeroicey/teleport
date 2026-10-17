# teleport — P6 切换完成（线上已迁到 hpcore + PostgreSQL + 自有域名根路径）

**Date:** 2026-10-17 · **Branch/Commit:** `main`（切换时部署的产物是 `c9be0ec`）
· **State:** P0–P5 ✅ / **P6 ✅（线上已切换并验证）**；旧版已停、回滚材料完整
· **Gate:** 四档闸门 + P6 端到端；契约在**根路径**上 64/64、真实浏览器 15/15

---

## 一、现在线上是什么

| 项 | 值 |
| --- | --- |
| 入口 | **`https://teleport.hcyj.xyz/`**（根路径，自有域名，与 clip 形态一致） |
| 服务 | hpcore 的 `teleport.service`（systemd，用户 `relay`）监听 `127.0.0.1:8788`；`frpc-relay-teleport`（user unit，用户 `oicey`）把 hcyj 的 8788 隧道过来；hcyj 的 Caddy 终结 TLS 后反代到 `172.17.0.1:8788` |
| 证书 | Let's Encrypt，`CN=teleport.hcyj.xyz`，2026-10-17 → 2027-01-15 |
| 数据 | 生产库 `teleport`：5 报告 / 6 分享链接 / 2 密钥 / 4 申请 / 0 续期；不可变列与源**逐字节一致** |
| 旧前缀 | `api.hcyj.xyz/yeciorez/teleport*` → **410** + 「moved to …」（已裁决的 break change） |
| 未受影响 | `clip.hcyj.xyz` 与 `api.hcyj.xyz` 的兜底服务都仍 200 |

## 二、先读什么

1. `.ai/runbooks/p6-cutover.md` —— **第七节「执行记录」**：实际怎么跑的、验证结果、
   以及**三处与原计划的偏差**。第二节是已经用掉的双入口分析（保留，因为它解释了为什么
   最后选了「单一根路径」）。
2. `.ai/pitfalls/cases/2026-10-17-acme-intermittently-unreachable-from-hcyj.md` —— 本次最实用的
   运维发现：hcyj 到 Let's Encrypt **间歇可达**，新域名证书会卡住；判断顺序与重试手段都在里面。
3. `.ai/decisions/2026-10-16-p6-single-root-entry.md` —— 为什么旧前缀被弃用（break change），
   以及「一个二进制做不到双入口」的证据链。
4. `.ai/CURRENT_TASK.md` 的「线上现状（2026-10-17 切换后实测）」表。

## 三、这次实际改了什么（系统侧，不在 git 里）

| 位置 | 改动 |
| --- | --- |
| hpcore `/data/services/teleport/` | 新二进制（32 MB，`version=c9be0ec`）；旧版改名 `teleport.prev-20261017-125500`；`teleport.env` 重写（旧版备份 `teleport.env.pre-p6-20261017-125419`） |
| hpcore `/etc/systemd/system/teleport.service` | **新建**（模板 `clip.service`；`EnvironmentFile` 600 root:root） |
| hpcore `~/.config/systemd/user/frpc-relay-teleport.service` | **新建** + `enable --now` |
| hpcore `/data/services/frp/conf/frpc-relay-teleport.toml` | **新建**（token 从 clip 那份逐字复制） |
| hcyj `/root/hcyj/caddy/Caddyfile` | 新增 `teleport.hcyj.xyz` site block；旧 `handle /yeciorez/teleport*` 改成 `respond … 410`；备份 `Caddyfile.pre-p6-20261017-131101` |
| 生产库 `teleport` | 建 schema + 灌 17 行（`sqliteimport import`），随后 `verify` 20/20 |

**`ROUTE_PREFIX="/"`**（不是空值 —— 空值会被 `env()` 当成未设置而回落旧前缀，**实测过**）；
**`GIN_MODE=release`**（Gin 不读 `ENVIRONMENT`，不设就打整张路由表）。

## 四、已验证 / 未验证

- [x] 根路径全链路：`P4_ROUTE_PREFIX=/ bash scripts/verify-p4.sh` → 契约 **64/64** + 浏览器 **15/15**
- [x] 生产库：`import` 17 行单事务、`verify` **20/20**
- [x] 公网路径：`/api/health`、`/ai.md`、`/llms.txt`、`/`、`/dashboard/reports` 全 200
- [x] 永久链接（`expires_at=0`）**200**；已吊销 **404**；未知 **404**；`.git`/`.env` **404**
- [x] 旧前缀三个路径 **410**；`clip` 与 `api` 兜底仍 **200**
- [x] 真 agent 密钥 `POST /api/reports` → **201**（新域名下鉴权成功）
- [x] 真实浏览器（公网）：**10 passed / 5 failed**，5 项**全部**是「需要管理员明文口令」的那些；
      分享页 SSR、Vue 挂载、**Mermaid 在真实 CSP 下渲成 SVG**、无失败请求都通过
- [x] 大陆观测点 **两个**（hpcore 家宽、koma）：健康 200、永久 200、已吊销 404、旧前缀 410
- [ ] **假定、未测：面板登录。** 需要管理员**明文**口令，而库里只有哈希 —— 我无法验证真人口令
      能否登录。能确认的是登录端点可达且正常拒绝错口令（401）。**请人工登录一次确认。**
- [ ] **假定、未测：`sqliteimport verify` 在活库上的语义。** 它比对**可变列**
      （`view_count`/`request_count`/`last_used_at`），库一服务就会偏离源。切换后实测到
      永久链接的 `view_count` 因为我验证时真的访问了它而增长 → 活库上应只比不可变列。
- [ ] **假定、未测：长期稳定性。** 只看了一次快照；frpc 断线重连、证书续期（ARI）、
      重启存活都只有一个数据点。

## 五、Loose ends

- [ ] **面板登录需人工确认一次**（唯一未验的功能路径）。
- [ ] `bin/` 下有本次的日志（`bin/p6/import.log`、`verify.log`、`bin/e2e/*`），都是 gitignore 的。
- [ ] **验证脚本本次修了 3 个真缺陷**，都已提交（见下「不要重复」）。
- [ ] 未做（本来就未做）：旧新对账（旧版已停）、`bjbuwe` 观测点（当前 ssh 不通）。

## 六、不要重复

1. **`printf '%s' "$BIG" | grep -q PATTERN` 在 `set -o pipefail` 下是陷阱**：`grep -q` 命中即退出
   → 写端 SIGPIPE → 管道整体非零，**尽管匹配成功**。它不仅会**假失败**（约 1/10，本次就是这么
   耗掉一段时间的），更危险的是让「必须**不**含 X」的检查（XSS、占位符残留）**假通过**。
   已加 `grep_q/grep_qF/grep_qE` 三个 helper（写临时文件再 grep 文件）并替换 16+ 处。
2. **空值 ≠ 未设置**：`ROUTE_PREFIX=` 会静默回落到默认值；`verify-p4.sh` 的
   `process.argv[3] || '/yeciorez/teleport'` 同理会把「显式空前缀」变成旧前缀。
   凡是「空前缀」这件事，两端都要区分「缺参数」与「参数为空」。
3. **`pkill -f "bin/teleport-linux-amd64"` 会杀掉自己**（模式匹配到本 shell 的命令行 → 143）。
   用 PID 或 `lsof -ti :PORT`。
4. **长 python 补丁不要中途 assert 就以为整批落地**：我有一次补丁在第 2 个 assert 处中止，
   导致 `GUIDE_FILE` 从未定义、而末尾清理引用了它 → `set -u` 下每轮 rc=1「64 passed」却不成功。
   写完后要 grep 验证字节（仓库里早有这条 pitfall，我又踩了一次）。
5. **hcyj 到 Let's Encrypt 间歇可达**：证书签不出来时**先重启 Caddy 重试**，不要改路由、
   更不要全局换 CA（那会影响 api/clip 的续期）。`docker logs caddy` 是空的 —— 看容器里的
   `/var/log/caddy/access.log`。

## 七、下一步

**没有必须做的下一步。** 可选：

1. **人工登录一次面板**（`https://teleport.hcyj.xyz/`）确认密码可用 —— 这是唯一未验的路径。
2. 观察一两天：frpc 是否稳、证书续期是否正常（LE 间歇可达会让 ARI 续期偶尔失败，Caddy 会重试）。
3. 若确认稳定，可以清理回滚材料（hcyj 的 `teleport.db` 与 `teleport.prev-*`、
   hpcore 的 `teleport.prev-*`、旧的 `.bak-*`）—— **但先别急着删**，旧版回滚是它们撑着的。
4. 若发现还有外部依赖指向旧前缀 → 重开 `2026-10-16-p6-single-root-entry.md` 的 Revisit 条件。
