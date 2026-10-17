# P6 切换 runbook（teleport 从 hcyj 旧版 → hpcore 新版）

**Status:** ✅ 已执行（2026-10-17 完成切换，线上验证通过） · **Created:** 2026-10-16
· **执行记录见文末**（含三处与原计划的偏差）
· **双入口问题已裁决**（单一根路径，旧前缀弃用）：见 `2026-10-16-p6-single-root-entry.md`
· **Related:** `2026-10-16-rewrite-on-gin-and-postgres.md`（纲领，P6 段）、
`2026-10-16-teleport-own-hostname.md`（新域名裁决）、`deploy-teleport.md`（常规部署）、
`caddy-routing-on-shared-host.md`（改 Caddy 的纪律）、
`2026-10-16-p5-schema-ownership.md`（数据搬运的裁决）

## When to use this

把线上从「hcyj 上跑旧版（net/http + SQLite，当前已停 → 502）」切成
「hpcore 上跑新版（Gin + pgx + PostgreSQL），hcyj 只做 TLS 终结与转发」。

**本文档尚未执行过。** 所有命令都写成可直接粘贴的形式，但每一步都标了「已验证 / 未验证」。

---

## 一、现场事实（2026-10-16 实测，全部只读，未改动任何东西）

### 拓扑

```text
用户 ──443──> hcyj caddy(docker) ──┬─ clip.hcyj.xyz           ─> 172.17.0.1:8790 ┐
                                   ├─ api.hcyj.xyz/yeciorez/teleport* ─> :8788    │
                                   └─ api.hcyj.xyz/（兜底）    ─> 172.17.0.1:3000  │
                                                                                  │
                        hcyj frps（systemd，*:7000/58173，proxyBindAddr=0.0.0.0）│
                                    ▲                                             │
                                    │ frpc 由 hpcore 主动拨出（hcyj→hpcore 入向已死）│
                                    │                                             ▼
                            hpcore: frpc(user unit) ─────────> clip: 127.0.0.1:8790
```

### 实测值

| 项 | 值 | 怎么核实的 |
| --- | --- | --- |
| DNS | `teleport.hcyj.xyz` / `api.hcyj.xyz` / `clip.hcyj.xyz` **都解析到 8.148.233.134** | `getent hosts` |
| TLS | `teleport.hcyj.xyz` → **HTTP 000**（无 site block、无证书）；`api`/`clip` → 200 | `curl -o /dev/null -w %{http_code}` |
| hcyj 的 443 | `caddy` **docker 容器**持有；Caddyfile = 宿主 `/root/hcyj/caddy/Caddyfile`（单文件 bind mount） | 容器列表 + 本 runbook 的姊妹文档 |
| hcyj Caddyfile 结构 | `https://clip.hcyj.xyz {…}`（第 29 行，**根路径站点的现成模板**）；`https://api.hcyj.xyz {…}`（第 65 行）内含 `handle /yeciorez/teleport*`（87）→ `reverse_proxy 172.17.0.1:8788`（92）、兜底 `handle` → `172.17.0.1:3000`（101）、`@notTeleport` 站点级响应头排除（116-117） | `sudo grep -n` |
| hcyj frps | systemd `frps.service`，`/usr/local/bin/frps -c /etc/frps.toml`；`bindAddr=0.0.0.0` `bindPort=7000` `proxyBindAddr=0.0.0.0`，**没有 `allowPorts` 限制** → 新增 remotePort 8788 会被接受 | 读 `/etc/frps.toml` |
| hcyj 旧 teleport | unit `disabled` + `inactive`；**8788 无人监听**（只有 8790 被 clip 的隧道占着） | `systemctl is-enabled/is-active`、`ss -ltn` |
| hpcore 的 8788 | **无人监听**；`clip` 监听 `127.0.0.1:8790` | `ss -ltnp` |
| hpcore 的 unit | **没有 teleport unit**；`clip.service` 存在（**system unit**，用户 `relay`）→ 可当模板 | `systemctl list-unit-files`、`systemctl cat clip` |
| hpcore 的 frpc | **user unit**（用户 `oicey`）：`~/.config/systemd/user/frpc-relay-clip.service` → `/data/services/frp/bin/frpc -c /data/services/frp/conf/frpc-relay-clip.toml`；同机还有 `frpc-dragon.service` | `systemctl --user list-units`、`systemctl --user cat` |
| clip 隧道配置 | `serverAddr = "100.64.0.4"`、`serverPort = 7000`、TLS、token；proxy：`localIP=127.0.0.1 localPort=8790 remotePort=8790` | 读 `frpc-relay-clip.toml`（密钥已屏蔽） |
| `/data/services/teleport/` | `teleport`（旧版二进制，26 MB，relay:relay 750）、`teleport.env`（root:root 600）、`.pgpassword.tmp`（root 600） | `sudo ls -la` |
| `teleport.env` 现状 | 仍是**旧配置**：含 **`DB_PATH`**、无 `DATABASE_URL`。变量名：ADMIN_PASSWORD_HASH, AGENT_SECRET_KEY, COOKIE_SECURE, DB_PATH, DEFAULT_SHARE_HOURS, ENVIRONMENT, FRONTEND_ORIGINS, KEY_*, LISTEN_ADDR, MAX_CONTENT_BYTES, PUBLIC_BASE_URL, ROUTE_PREFIX, SESSION_SECRET, SESSION_TTL | 只列了变量名，**没有打印任何值** |
| 生产意图库 | `teleport` 库 **0 张表**（从未迁移过） | `information_schema` |
| 旧数据 | `hcyj:/data/services/teleport/teleport.db`，sha256 `0b3855d7…beb24d`；5 报告 / 6 分享链接 / 2 密钥 / 4 申请 / 0 续期 | P5 的 `scripts/verify-p5.sh` |

### 配置项全名（`internal/config`，4 项必填）

必填：`DATABASE_URL`、`SESSION_SECRET`、`AGENT_SECRET_KEY`、`ADMIN_PASSWORD_HASH`
（缺任一 → **启动即失败**，这是刻意的）。
其余：`LISTEN_ADDR`（默认 `127.0.0.1:8788`）、`ROUTE_PREFIX`（默认 `/yeciorez/teleport`）、
`PUBLIC_BASE_URL`、`ASSET_BASE_URL`、`FRONTEND_ORIGINS`、`COOKIE_PATH`、`COOKIE_SECURE`、
`ENVIRONMENT`、`DB_MAX_CONNS`、`STATIC_DIR`、`TRUSTED_PROXIES`、`DEFAULT_SHARE_HOURS`、
`MAX_CONTENT_BYTES`、`SESSION_TTL`、`KEY_*`、`ENV_FILE`。

---

## 二、已裁决：单一根路径入口（旧前缀弃用）

**裁决**（用户 2026-10-16）：「break change 没事的 使用 teleport.hcyj.xyz 然后直接 /链接 就好了」
→ 已记录为 `.ai/decisions/2026-10-16-p6-single-root-entry.md`，并**取代**了
`2026-10-16-teleport-own-hostname.md` 里「旧前缀永久保留」那条约束。

### 为什么这条裁决是必要的（侦察发现，每条都可核验）

一个二进制**做不到**同时服务两个前缀：

1. `ROUTE_PREFIX` 是单值，决定路由挂载点（`api/router.go` 的 `p := cfg.RoutePrefix`）、
   `config.Route()`、`AppBaseURL() = PublicBaseURL + RoutePrefix`、`CookiePath`。
2. **分享页的资产与 iframe 是绝对 URL**：`api/helpers.go` 的 `renderSharePage` 用
   `cfg.AssetBaseURL` 与 `cfg.ShareURL(token)+"/html"`，两者都指向 `PUBLIC_BASE_URL` 那个域名，
   **与请求从哪个入口进来无关**。
3. 分享页的 CSP 是 `script-src 'self'` + `frame-src 'self'`（`views/sharepage.go` 里写明了
   「与页面同源」），跨源取资源会被**直接挡掉**。
4. **前端前缀是构建期烧进产物的**：`vite.config.ts` 的
   `base = (VITE_ROUTE_PREFIX ?? '/yeciorez/teleport') + "/"`；`web/src/api.ts` 用
   `import.meta.env.BASE_URL`。一个二进制 = 一份烧死的基址。

所以「旧前缀只靠 Caddy 去前缀转发」这条捷径不成立：旧前缀的分享页仍会去新域名取 JS 与 iframe
→ 被 CSP 挡 → **Mermaid 不渲染、`format:"html"` 的报告变成空框**，而正文文本还在
（一种半可见的坏法，比彻底 404 更难发现）。

### 接受的代价

旧长链接全部失效 —— **包括 `expires_at=0` 的永久链接**，不可挽回。用户已知情并明确接受
（原话：「break change 没事的」）。

### 因此 P6 变简单

不需要 `handle_path`/`strip_prefix`，不需要第二个实例，后端不需要支持双前缀；
`AGENTS.md` 里「Caddy 必须用 `handle`」那条纪律照旧成立（后端不再需要前缀，所以没有冲突）。

### 旧前缀怎么处理（建议：一条明确的 410，而不是静默 404）

**不要保留原来的 `handle /yeciorez/teleport*` → `172.17.0.1:8788`**：那会把旧路径转发到
已经挂到根的新后端 → 命中 SPA 回退 → 返回一个引用**根路径资产**的 HTML，而 `api.hcyj.xyz`
的根是**别的服务**（`172.17.0.1:3000`）的兜底，用户会得到莫名其妙的结果。

改成一条显式声明：

```caddyfile
# 旧前缀已弃用（2026-10-16 用户裁决，break change）：明确告知而非静默 404。
handle /yeciorez/teleport* {
    respond "teleport moved to https://teleport.hcyj.xyz/" 410
}
```

（这是运维礼仪，不是本决策的一部分；410 是**路径级**响应，不涉及任何具体 token 的状态，
所以不违反「未知/已吊销 404、仅过期 410」那条冻结语义。）

## 三、执行步骤（每一步都给了验证与回滚）

> 标记约定：✅ = 已验证过（在临时 schema / 本机 / 只读侦察中） · ❓ = 未验证

### 步骤 0：前置确认（只读）

```bash
# 0.1 旧库无写入者（旧版已停 → 预期无人打开它；确认没有别的进程在写）
ssh hcyj 'systemctl is-active teleport; ls -la /data/services/teleport/teleport.db*'
#     期望：inactive；且**没有** teleport.db-wal / -shm / -journal
# 0.2 生产库仍是空的（import 会拒绝非空目标，但先确认一次）
# 0.3 记录旧版回滚材料仍完整：hcyj 的 teleport.prev-*、teleport.db（回滚到旧版依赖它）
```

### 步骤 1：构建（❓ 注意前缀是构建期烧进前端的）

```bash
cd /data/dev/teleport
# ROUTE_PREFIX="" → 前端 base="/" 。VITE_ROUTE_PREFIX 必须在 build 的环境里可见。
VITE_ROUTE_PREFIX=/ pnpm run build:release        # 产出 bin/teleport-linux-amd64
# ⚠️ VITE_ROUTE_PREFIX=/ 必须与 teleport.env 里的 ROUTE_PREFIX=/ **成对**出现。
#    只改一边 = 前端基址与后端前缀不一致，根路径下白屏/资源 404。
#    注意 env 那边写 "/"，不能写空值（空值 = 未设置 = 回落旧前缀，实测见步骤 4）。
./bin/teleport-linux-amd64 version                # 期望：版本号
./bin/teleport-linux-amd64 version --frontend     # 期望：embedded
```

**验证**：P4 那套全链路闸门可以对着这个二进制跑一遍（`P4_SKIP_BUILD=1` 之前要先起服务），
或者至少确认 `version --frontend` = `embedded`。
**反证**：不带 `VITE_ROUTE_PREFIX=/` 构建出来的产物 base 是 `/yeciorez/teleport/`，
在根路径下会 404 —— 这正是「构建期耦合」的现场证据。

### 步骤 2：取旧数据（只读获取，钉住哈希）✅

```bash
P5_FETCH=1 P5_EXPECT_SHA256=0b3855d7b130e30d7ca2128afce7c9dd30dfaf7eba5f7a57293347a2cbbeb24d \
  bash scripts/verify-p5.sh
```

它拒绝带 `-wal/-shm/-journal` 的源、并在**临时 schema** 上把 import+20 项核对+负向对照再跑一遍。
✅ 已实测通过。

### 步骤 3：建生产 schema 并灌数据（同一窗口内，不可逆的第一步）

```bash
export DATABASE_URL='postgres://teleport:<口令>@127.0.0.1:5432/teleport?sslmode=disable'
# 3.1 建 schema（应用自己的迁移器；会写 schema_migrations 的内容哈希）
./bin/teleport-linux-amd64 migrate
# 3.2 灌数据（**不带 -replace**；非空目标会被拒绝）
./bin/import/sqliteimport import -source bin/import/teleport.db
# 3.3 核对（20 项；每一项都有反证用例）
./bin/import/sqliteimport verify -source bin/import/teleport.db
```

**期望**：`import` 17 行；`verify` → `20 passed, 0 failed`，且报告里
`reports=5 share_tokens=6 agent_keys=2 key_applications=4 key_renewals=0`、
`permanent(expires_at=0)=1`、`revoked(is_active=0)=2`。
**回滚**：此时还没有任何用户可见变化。回滚 = `DROP DATABASE`/重建（或保留库不动，反正旧版不读它）。
**注意**：**不要**对生产用 `-replace`（它会 TRUNCATE 五张表）。
❓ 未验证：这一步从未在生产库上跑过（只在 `teleport_test` 的临时 schema 上跑过）。

### 步骤 4：写 env 文件（❓）

```bash
sudo install -m 0600 -o root -g root /dev/null /data/services/teleport/teleport.env
sudo tee /data/services/teleport/teleport.env >/dev/null <<'EOF'
ENVIRONMENT=production
LISTEN_ADDR=127.0.0.1:8788
# Gin 没有任何从 ENVIRONMENT 推导 mode 的代码；它读 GIN_MODE。
# 不设这个，服务在 production 下仍跑 debug 模式，每次启动把整张路由表打进 journal
# （P6 切换时实测到 61 行 GIN-debug）。
GIN_MODE=release
# ── 新域名挂根路径 ──
# ⚠️ 写 "/" ，**不要写空值**：空的 ROUTE_PREFIX 会被当成「未设置」而回落到默认值
#    （仍是 /yeciorez/teleport）。实测（启动日志里的 prefix=）：
#      ROUTE_PREFIX=            → prefix=/yeciorez/teleport   ← 陷阱
#      ROUTE_PREFIX=/           → prefix=                    ← 要的是这个
#      ROUTE_PREFIX=/yeciorez/teleport → prefix=/yeciorez/teleport
ROUTE_PREFIX="/"
PUBLIC_BASE_URL=https://teleport.hcyj.xyz
FRONTEND_ORIGINS=https://teleport.hcyj.xyz
COOKIE_SECURE=true
# ── 存储（旧的 DB_PATH 必须删掉；新版读到它不会用，缺 DATABASE_URL 会直接启动失败）──
DATABASE_URL=postgres://teleport:CHANGE_ME@127.0.0.1:5432/teleport?sslmode=disable
DB_MAX_CONNS=8
# ── 密钥：从旧 env **原样搬运**，绝不轮换 ──
SESSION_SECRET=<从旧文件复制>
AGENT_SECRET_KEY=<从旧文件复制>
ADMIN_PASSWORD_HASH=<从旧文件复制>
# ── 其余沿用旧值 ──
DEFAULT_SHARE_HOURS=…  MAX_CONTENT_BYTES=…  SESSION_TTL=…  KEY_* …
EOF
```

**必须确认**：`SESSION_SECRET` **一字不改**（它派生所有 agent 密钥与续期域，轮换会让
所有已发出的密钥失效）；`DB_PATH` **删掉**；`COOKIE_SECURE=true`（否则启动日志会警告）。
`DB_MAX_CONNS=8`：PostgreSQL 实例与 clip / registry / serenique **共用**，`max_connections=100`
是大家的总预算。

### 步骤 5：装二进制 + systemd unit（❓，unit 以 `clip.service` 为模板）

```bash
sudo install -m 0750 -o relay -g relay ./bin/teleport-linux-amd64 /data/services/teleport/teleport.new
sudo mv /data/services/teleport/teleport /data/services/teleport/teleport.prev-$(date +%Y%m%d-%H%M%S)
sudo mv /data/services/teleport/teleport.new /data/services/teleport/teleport

sudo tee /etc/systemd/system/teleport.service >/dev/null <<'EOF'
[Unit]
Description=Teleport (AI report publishing + expiring share links)
After=network-online.target docker.service
Wants=network-online.target

# StartLimit* are [Unit] keys. In [Service] systemd ignores them SILENTLY (it logs
# "Unknown key … ignoring" and keeps the defaults), which is exactly what happened on
# the first attempt here: the limit stayed at the default 5-in-10s. With it, a
# PostgreSQL outage longer than ~10s trips the limiter and systemd stops retrying —
# the unit sits in `failed` until a human runs `systemctl reset-failed`. Verify with
# `systemctl show teleport -p StartLimitIntervalUSec` (0 = disabled).
StartLimitIntervalSec=0

[Service]
Type=simple
User=relay
Group=relay
WorkingDirectory=/data/services/teleport
# 口令在里面，所以这个文件必须 600 root:root；systemd 以 root 读入后再降权。
# EnvironmentFile 的值是**字面**的（不做 shell 展开），所以口令里的 $ 不会被吃掉。
EnvironmentFile=/data/services/teleport/teleport.env
ExecStart=/data/services/teleport/teleport
Restart=always
RestartSec=2

NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
RestrictRealtime=true
LockPersonality=true
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now teleport
curl -sI http://127.0.0.1:8788/api/health     # 期望 200（本机 loopback）
journalctl -u teleport -n 30 --no-pager       # 期望 "database ready"；不应有 DB_PATH/缺 DATABASE_URL 的错
```

**注**：`ProtectSystem=strict` 下服务不需要写任何文件（它只写 PostgreSQL）——
与 clip 相同。若启动失败，先看 journal：**缺 `DATABASE_URL` 是会明确报错的第一名**。

### 步骤 6：frpc 隧道（❓，user unit，以 clip 为模板）

```bash
loginctl show-user oicey | grep -i linger        # 期望 Linger=yes（clip 的 frpc 已在跑，大概率已开）
sudo tee /data/services/frp/conf/frpc-relay-teleport.toml >/dev/null <<'EOF'
# Teleport: hpcore:8788 -> hcyj :8788, proxied by Caddy for teleport.hcyj.xyz.
# hcyj only forwards: this host dials OUT to frps on hcyj, because inbound
# hcyj -> hpcore is dead (Tailscale inbound to this box times out).
serverAddr = "100.64.0.4"
serverPort = 7000

auth.method = "token"
auth.token = <与 frpc-relay-clip.toml 同一个 token>

transport.tls.enable = true

[[proxies]]
name = "relay-teleport"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8788
remotePort = 8788
EOF
sudo chown oicey:oicey /data/services/frp/conf/frpc-relay-teleport.toml

install -D -m 0644 /dev/stdin ~/.config/systemd/user/frpc-relay-teleport.service <<'EOF'
[Unit]
Description=frp client for Teleport (hpcore:8788 -> hcyj :8788)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/data/services/frp/bin/frpc -c /data/services/frp/conf/frpc-relay-teleport.toml
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now frpc-relay-teleport
# 验证：hcyj 上 8788 现在应该有人监听（frps 代理绑上去）
ssh hcyj 'ss -ltn | grep :8788'
ssh hcyj 'curl -sI -o /dev/null -w "%{http_code}\n" http://172.17.0.1:8788/api/health'   # 期望 200
```

**回滚**：`systemctl --user disable --now frpc-relay-teleport`（hcyj 的 8788 随即消失，不影响 clip）。

### 步骤 7：hcyj 的 Caddy（🔴 最危险的一步：配置写错会让 443 上**所有**站点不可用）

按 `caddy-routing-on-shared-host.md` 的纪律走，**不要跳校验**：

```bash
ssh hcyj
cd /root/hcyj/caddy
cp Caddyfile Caddyfile.pre-p6-$(date +%Y%m%d-%H%M%S)      # 1. 备份
# 2. 编辑 Caddyfile：
#    (a) 新增下面的 site block（结构照抄 clip 的 block：第 29 行）。
#        注意：**不要**加站点级 log —— 它会把一次性凭据头 X-Teleport-Claim 明文写进日志
#        （见 caddy-routing-on-shared-host.md 的「已知坑」）。
#    (b) 把 api.hcyj.xyz 里的旧 handle 换成 410（见第二节）。
#    (c) 站点级响应头继续排除 /yeciorez/teleport*（@notTeleport），保持原样。
#
#    ── 新增的 site block ──────────────────────────────────────────────
#    https://teleport.hcyj.xyz {
#        tls {
#            protocols tls1.2 tls1.3
#            ciphers <照抄 clip block 里那一行，逐字>
#            curves x25519
#            alpn h2 http/1.1
#            key_type rsa2048
#        }
#        # 后端自己发更严格的 CSP/nosniff/noindex/no-store；这里只加 TLS 层的 HSTS。
#        header {
#            Strict-Transport-Security "max-age=63072000; includeSubDomains; preload"
#        }
#        respond /.git* 404
#        respond /.env* 404
#        respond /.*.bak 404
#        # 172.17.0.1:8788 是本机 frps，隧道到 hpcore 的 teleport。根路径，不去前缀。
#        reverse_proxy 172.17.0.1:8788 {
#            header_up Host {host}
#            header_up X-Real-IP {remote}
#            header_up X-Forwarded-For {remote_host}
#            header_up X-Forwarded-Proto {scheme}
#        }
#    }
# 3. 校验（这一步最会骗人 —— 必须校验**容器内**看到的那个文件）
docker cp /root/hcyj/caddy/Caddyfile caddy:/tmp/Caddyfile.check
docker exec caddy md5sum /tmp/Caddyfile.check; md5sum /root/hcyj/caddy/Caddyfile   # 两者必须一致
docker exec caddy caddy adapt --config /etc/caddy/Caddyfile >/dev/null && echo "adapt OK"
docker exec caddy caddy adapt --config /etc/caddy/Caddyfile | grep -c 'teleport'   # >0
# 4. 重启并立刻验证**三个方向**
docker restart caddy
curl -sI https://teleport.hcyj.xyz/api/health          # 新域名 200
curl -sI https://api.hcyj.xyz/                          # 兜底上游仍 200（别的服务）
curl -sI https://clip.hcyj.xyz/                         # clip 仍 200
```

**回滚**：`cp Caddyfile.pre-p6-<ts> Caddyfile && docker restart caddy`，再确认 `api.hcyj.xyz/` 200。

### 步骤 8：双入口 + 永久链接 + 大陆观测点（❓）

```bash
# 新域名：面板、分享页、agent 文档、API
curl -sI https://teleport.hcyj.xyz/ai.md
# 永久链接（新域名下**必须**能打开）—— token 取真源里 expires_at=0 且 is_active=1 的那个
curl -sI https://teleport.hcyj.xyz/s/Du01mf3JsO9utq69xtKcBw
# 旧前缀现在是**故意失效**的：期望 410（若选择保留 410 那条配置）
curl -s -o /dev/null -w '%{http_code}\n' https://api.hcyj.xyz/yeciorez/teleport/s/Du01mf3JsO9utq69xtKcBw
# 已吊销的必须仍是 404（源里两个 is_active=0 的）
curl -so /dev/null -w '%{http_code}\n' https://teleport.hcyj.xyz/s/jOrlqf4gcF7lDUDrkUjxmg
# 浏览器：面板真登录、Mermaid 渲染、format:html 的 iframe 有内容
# 另加：用真 agent 密钥 POST /api/reports（新域名下）
```

大陆观测点（ADR 验收第 4 条）：hpcore 本机、以及 `bjbuwe` / `koma` 各测一次公网 URL。

---

## 四、风险清单

| # | 等级 | 风险 | 如何发现 | 处置/回滚 |
| --- | --- | --- | --- | --- |
| 1 | 🔴 | **旧长链接（含 `expires_at=0` 的永久链接）失效，不可挽回** | 从旧前缀打开 → 410 | **已由用户裁决接受**（`2026-10-16-p6-single-root-entry.md`）。若日后发现真实的外部依赖（某个 agent 配置写死了旧前缀）→ 回到双入口方案的 A 或 C |
| 2 | 🔴 | 改 Caddyfile 出错 → **443 整体不可用**，连带 clip 与 `api.hcyj.xyz` 的别的服务 | `curl https://clip.hcyj.xyz/` 变成 000 | 备份文件回滚 + `docker restart caddy`；所以校验三步不能跳 |
| 3 | 🔴 | 轮换 `SESSION_SECRET` → **所有已发出的 agent 密钥失效**（派生） | agent 的请求开始 401 | 从旧 env **原样复制**；已经轮换就只能重发密钥 |
| 4 | 🔴 | 生产库灌错或灌重复 | `import` 会拒绝非空目标；`verify` 20 项 | `-replace` **会 TRUNCATE**，生产上不要用；宁可用新库重来 |
| 5 | 🟠 | `teleport.env` 残留 `DB_PATH`、缺 `DATABASE_URL` | 启动即失败并明确报 `DATABASE_URL is required` | 修 env 重启（这是好的失败方式） |
| 6 | 🟠 | **证书签发卡住（实测发生了）**：hcyj 到 Let's Encrypt 的出网**间歇可达**（目录 4/6 成功），`POST /acme/new-order` 反复超时；Caddy 回落到 ZeroSSL 后 http-01 也没下文 | `curl https://teleport.hcyj.xyz/` → **000（curl exit 35）**；证书目录里没有该名字 | **重启 Caddy 重新签发**（实测第一次重启 10 秒内签下 LE 证书）。旧站点不受影响。细节与判断顺序见 `.ai/pitfalls/cases/2026-10-17-acme-intermittently-unreachable-from-hcyj.md` |
| 7 | 🟠 | 新实例的连接池挤占共享 PostgreSQL | 别的服务报 `too many clients` | `DB_MAX_CONNS=8`；实例 `max_connections=100` 是四家共享 |
| 8 | 🟠 | 回滚到旧版依赖 hcyj 的 SQLite 文件 | —— | **别删** `/data/services/teleport/teleport.db` 与 `teleport.prev-*` |
| 9 | 🟠 | 前端构建错前缀（base 烧死） | 根路径下页面白屏、资源 404 | 必须 `VITE_ROUTE_PREFIX=/` 重新构建 |
| 10 | 🟡 | `live`/`expired` 的时钟边界 | 恰好在这一毫秒过期的链接被报「不稳定」 | 只会**假失败**，不会假通过；重跑即可 |
| 11 | 🟡 | （单一入口后**此条不再适用**）曾担心两个入口的 cookie 不共享 | —— | 旧前缀弃用后只有一个 `CookiePath`；保留此行以便说明它为何消失 |
| 12 | 🟡 | 改 `@notTeleport` 时漏排除，导致分享页收到两份 CSP | 分享页 Mermaid/iframe 不工作但状态码 200 | 保留 `handle` 的最长匹配 + 站点级头排除 |
| 13 | 🔴 | **`ROUTE_PREFIX=` 写空值 → 静默回落旧前缀**（`env()` 把空当未设置），而前端基址是 `/` → **根路径下白屏 / 资源 404** | 启动日志 `prefix=/yeciorez/teleport`；或根路径页面白屏 | 写 `ROUTE_PREFIX="/"`。**已实测**（三种写法）并写进 `teleport.env.example` |
| 14 | 🟠 | 旧前缀变 410 后，若**忘记**它曾服务过 `/api/*`，某些仍写着旧地址的 agent 会拿到 410 而非 404 | agent 端报 410 | 这是**已裁决的 break change**，不是 bug；但切换公告里要写清楚新地址 |

---

## 五、未验证 / 假设

- ❓ **本文档的每一步都没有执行过**（第 0-8 步全是「在纸上跑」）。已实测的只有：
  P5 的彩排（`scripts/verify-p5.sh`，20/20 + 负向对照）、只读侦察、以及在 `teleport_test`
  临时 schema 上的 import/verify。
- ❓ **Caddy 的 site block 内容**还没有写出来（只确认了要照抄 clip 的 block）；改动前应当先把
  完整 diff 写进本文档给人看。
- ❓ **`ROUTE_PREFIX="/"`（根路径）这个分支从未跑过**：P4 全链路验证用的是 `/yeciorez/teleport`。
  ADR 早就写着「空值是新的分支，必须实测而不是假定」。**这是现在 P6 之前的头号未知数**，
  应当先用 `scripts/verify-p4.sh` 那一套（它已经能建临时 schema、起真二进制、跑 64 项契约
  与真实浏览器）在 `P4_ROUTE_PREFIX=` 空值 + `VITE_ROUTE_PREFIX=/` 构建的二进制上跑绿一次。
  用 `verify-p4.sh` 复跑的成本很低，而它覆盖的正是「空前缀」与「根路径资产」这两件事。
  **已经实测到的部分**：`ROUTE_PREFIX` 的三种写法在启动日志里分别是
  `""→/yeciorez/teleport`（陷阱）、`"/"→""`、`"/yeciorez/teleport"→原值` —— 见第四节风险 13。
- ❓ hpcore 的 `opathy`/防火墙是否放行 8788 出向：不影响（出向是 frpc 主动拨的 7000）。
- ❓ `loginctl` 的 linger 状态没查（步骤 6 里第一步就是查它）。

## 六、Revisit when

- 若发现真实的外部依赖仍指向旧前缀 → 重开 `2026-10-16-p6-single-root-entry.md`，
  回到双入口方案（A：旧前缀 API 转发 + 页面 301 ／ C：两个实例同一个库）。
- 如果 `verify-p4.sh` 能在 `ROUTE_PREFIX=""` 下跑绿 → 可以据此缩小第 9 条风险的未知度。
- 如果 `teleport.db-wal` 出现在 hcyj → 取源方式改为 `sqlite3 .backup` 并重评。

---

## 七、执行记录（2026-10-17，实际怎么跑的）

全部步骤都执行了。结果：**线上从「旧版已停、502」切到「hpcore 新版 + PostgreSQL + 自有域名根路径」，
两个大陆观测点验证通过。**

| 步骤 | 结果 |
| --- | --- |
| 0 前置 | 旧版 `inactive`+`disabled`（无写者）、无 `-wal/-shm/-journal`、源 sha256 与钉住值一致、生产库 0 张表 |
| 1 构建 | `VITE_ROUTE_PREFIX=/ bash scripts/build.sh` → `version=c9be0ec`、`frontend: embedded` |
| — 额外 | **先在根路径上跑通全链路验证**：`P4_ROUTE_PREFIX=/ bash scripts/verify-p4.sh` → 契约 **64/64** + 浏览器 **15/15**（这是 P6 之前最大的未知数） |
| 3 数据 | `sqliteimport import` → 17 行、单事务；`verify` → **20/20** |
| 4 env | 三个密钥从旧文件**逐字搬运**（先比对过 hcyj 与 hpcore 两份的哈希，完全一致）；`DB_PATH` 删除；`ROUTE_PREFIX="/"` |
| 5 服务 | 装了 systemd unit（模板 `clip.service`）+ 二进制；`active`+`enabled`；日志 `prefix="" appBaseUrl=https://teleport.hcyj.xyz environment=production` |
| 6 隧道 | user unit + 新配置（token 从 clip 那份逐字复制）；hcyj 的 8788 随即被 frps 绑上，经隧道 `/api/health` → 200 |
| 7 Caddy | 备份 → 上传 → **宿主/容器 md5 一致** → `caddy adapt` exit 0 → 重启；`api`/`clip` 仍 200，旧前缀 **410** |
| 8 验证 | 见下 |

### 三处与原计划的偏差（都值得知道）

1. **`ROUTE_PREFIX` 的空值陷阱**（原计划里是风险 13，实测确认）：写 `ROUTE_PREFIX=` 会**静默回落**
   到 `/yeciorez/teleport`（`env()` 把空值当未设置），必须写 `"/"`。已在 env 文件里注明。
2. **`GIN_MODE=release` 原本漏了**：Gin 不读 `ENVIRONMENT`，所以 production 下仍打整张路由表
   （61 行 `GIN-debug`）。已补进本文的 env 片段，并在真实 env 里加上。
3. **证书签发卡住**（风险 6 实测发生）：hcyj 到 LE 间歇可达，`new-order` 超时。**重启 Caddy**
   后 10 秒内签下 LE 证书。详见其 pitfall。

### 步骤 8 的实际验证结果

| 检查 | 结果 |
| --- | --- |
| `/api/health`、`/ai.md`、`/llms.txt`、`/`、`/dashboard/reports` | 全 200 |
| 永久链接 `/s/Du01mf3JsO9utq69xtKcBw` | **200**（`expires_at=0` 的链接在新域名下可读） |
| 已吊销 `/s/jOrlqf4gcF7lDUDrkUjxmg` | **404**（不是 403，不泄漏状态） |
| 未知 token | 404 |
| `/.git/config`、`/.env` | 404（站点级探针） |
| 旧前缀三个路径 | **410** + `teleport moved to https://teleport.hcyj.xyz/` |
| `api.hcyj.xyz/` 兜底、`clip.hcyj.xyz` | 仍 **200**（没被改坏） |
| 证书 | Let's Encrypt，`CN=teleport.hcyj.xyz`，2026-10-17 → 2027-01-15 |
| 真 agent 密钥 `POST /api/reports` | **201**（root 密钥在新域名下鉴权成功） |
| 真实浏览器（CDP，公网新域名） | **10 passed / 5 failed**，5 项失败**全部**是「需要管理员明文口令」的那些（我只持有哈希）；分享页 SSR、Vue 挂载、**Mermaid 在真实 CSP 下渲成 SVG**、无失败请求都通过 |
| 大陆观测点 | hpcore（家宽）与 koma 各一次：`/api/health` 200、永久链接 200、已吊销 404、旧前缀 410；h2、证书链校验通过。`bjbuwe` 当前不可 ssh |
| 生产库完整性 | 删掉验证时造的那条测试报告后，与源比对：**不可变列逐字节一致**、reports=5。`verify` 报 2 项差异，是永久链接的 `view_count` 增长 —— 我验证时**真实访问**留下的使用计数，属预期（见下） |

### 执行之后才发现的部署缺陷（已修）

- **🔴 systemd 重启限流陷阱**：unit 用 `Restart=always` + `RestartSec=2`，而
  `StartLimitBurst` 默认 5 / 间隔默认 10s —— 5 次重启正好卡在窗口上。`main.go` 在
  `store.OpenPG` 失败时 `fatal()` 退出，而 unit 只 `After=docker.service`（docker 的 socket，
  **不是** PostgreSQL 就绪）。所以一次超过约 10 秒的数据库不可用就可能触发限流 →
  unit 变 `failed` 且 **systemd 不再重试**，直到有人 `systemctl reset-failed`。
  已加 `StartLimitIntervalSec=0`（**必须放在 `[Unit]`**：放 `[Service]` 会被静默忽略，
  我第一次就放错了，靠 `journalctl` 的 "Unknown key … ignoring" 才发现）。
  实测生效：`StartLimitIntervalUSec=0`。**共享主机的 `clip.service` 有同样的形状**（不在本仓库，
  未改——但那台机器上的服务值得一起看）。
- **🔴 一个仍在生效的凭据躺在 644 文件里**：`/data/dev/research/.tmp/teleport/claim.json`
  （含明文 token，前缀 `VPQ1AJLS`，对应 `agent_keys` 里 `revoked_at=0` 的那把「agent 测试」密钥）
  权限是 644。已 `chmod 600`；**是否吊销那把密钥需要人类决定**（吊销会与「目标 == 源」不一致）。

### 两个遗留的「未验证」

- ⛔ **面板登录没在 P6 验证**：需要管理员**明文**口令，而库里只有哈希。P4 时用一次性口令在
  临时实例上验过；本次能确认的是登录端点可达且正常拒绝错口令（401）。要用真人口令复验一次。
- ⚠️ **`sqliteimport verify` 在活库上会报使用计数的差异**：它是「搬运后一次性核对」的工具，
  比对 `view_count`/`request_count`/`last_used_at` 这些**可变列**。库一旦开始服务，
  这些列就会与冻结的源不同 —— 那不是穿透，是流量。活库上应当只比**不可变列**
  （或接受这几项差异）。
