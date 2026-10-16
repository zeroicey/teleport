# 部署 Teleport 到 hcyj

**Applies to:** 生产（hcyj / `api.hcyj.xyz`，旧版 SQLite） · **Owner:** lead · **Last verified:** 2026-10-11

> ## ⚠️ 本手册描述的是 **P2.3 之前**的线上版本（SQLite）
>
> 重写版（P2.3 起）的存储是 **PostgreSQL**，启动需要 `DATABASE_URL`，
> 备份对象是 database 而不是文件。下面的 §4.2 与 §4.4 已给出 PostgreSQL 变体，
> 但**那双路径尚未在切换窗口里验证过** —— 首次用它们就是在 P6。
>
> 另：重写期间 **ROUTE_PREFIX / PUBLIC_BASE_URL 未改**（P6 才加
> `teleport.hcyj.xyz` 根路径入口），所以本文的路径与域名对 P2.3 之后的构建仍适用。
>
> 当前线上是 **`disabled` + `inactive`，公网 502**（用户裁决：等新版）。
> 所以重写版上线前，这份手册的「先绿后发」部分要改为 `pnpm run backend:test-pg`
> （旧版没有真库闸门）。

## Preconditions

> **P6 切换前必读**：数据从旧版 SQLite 搬到新版 PostgreSQL 的完整顺序与护栏见
> `.ai/decisions/2026-10-16-p5-schema-ownership.md` 与
> `.ai/sessions/2026-10-16-p5-import-handoff.md` 的「下一步」。要点：

```bash
# 1. 取源（在 hpcore 上，**只读** hcyj）——先查 -wal，再比对两端 sha256
P5_FETCH=1 bash scripts/verify-p5.sh          # 彩排：在 teleport_test 的临时 schema 上跑
P5_EXPECT_SHA256=<pinned> bash scripts/verify-p5.sh
# 2. 生产库：建 schema（应用自己的迁移器）→ 灌数据 → 核对（都在同一窗口）
./bin/teleport-linux-amd64 migrate            # 需要 teleport.env 里的 DATABASE_URL
./bin/import/sqliteimport import -source bin/import/teleport.db -dsn "$DATABASE_URL"
./bin/import/sqliteimport verify -source bin/import/teleport.db -dsn "$DATABASE_URL"
```

> 三条护栏：① 搬运工具**从不发 DDL**（schema 只可能来自嵌入迁移器）；
> ② 非空目标**默认拒绘**，`-replace` 必须再给 `-yes` 才会 TRUNCATE；
> ③ 源带 `-wal/-shm/-journal` 时**拒绘**（`immutable=1` 读法会静默丢掉 WAL 里的已提交事务）。

## When to use this

发布新版本，或后端二进制需要更新时。前端**没有独立部署步骤** —— 它已在二进制里。

## Preconditions

- 本地：`pnpm`、Go 1.26+（`/usr/local/go/bin`）、`GOPROXY=https://goproxy.cn,direct`
- SSH：`ssh hcyj`（root，key `~/.ssh/hcyj_ed25519`）
- **服务器上没有 Go 工具链** → 必须本地交叉编译
- 服务器**没有 docker compose**，只有 `docker run`

## Procedure

### 1. 发布构建（本地）

```bash
env -u TEST_PG_DSN pnpm run check   # 闸门（无库档：PG 测试会 SKIP 并大声提醒）
pnpm run backend:test-pg            # 必需：真库闸门，无 TEST_PG_DSN 直接退出
pnpm run build:release              # → bin/teleport-linux-amd64
```

> **不要只跑 `pnpm run check`。** 不设 `TEST_PG_DSN` 时它会跳过一百多个测试
> （store、api 与大部分冻结契约都在里面），结尾会打印
> `gate passed — WITHOUT DATABASE-BACKED TESTS`。真要发版必须再跑 `backend:test-pg`，
> 它需要 `TEST_PG_DSN` 指向名字含 `test` 的库（指向别的库会被夹具**拒绝**）。

顺序不能反：Vite 产物写进 `backend/internal/webui/dist/`（`//go:embed` 的输入），
`embed_frontend` 构建标签把 webui 从"无前端"桩切到真实文件系统。
构建会打印 `version`（`git describe --always --dirty`）、`size`、`sha256`。

### 2. 首次准备（只做一次）

```bash
ssh hcyj
useradd --system --no-create-home --shell /usr/sbin/nologin teleport
mkdir -p /data/services/teleport && chown teleport:teleport /data/services/teleport
```

> `/data` 在 hcyj 上**默认不存在**，必须显式创建。

### 3. 上传与原子替换

```bash
scp bin/teleport-linux-amd64 hcyj:/data/services/teleport/teleport.new
ssh hcyj 'cd /data/services/teleport \
  && chmod 700 teleport.new && chown teleport:teleport teleport.new \
  && mv teleport.new teleport \
  && systemctl restart teleport'
```

> **必须 `mv` 覆盖，不能 `scp` 直接写目标文件。** `scp` 以截断方式打开目标，
> 若服务正在运行会短暂读到残缺的可执行文件；`mv` 是同目录内原子替换。

### 4. 环境文件（含密钥）

`/data/services/teleport/teleport.env`，权限 **600 root:root**，模板见
`backend/deploy/teleport.env.example`。

**旧版**必填三项：`AGENT_SECRET_KEY`、`SESSION_SECRET`、`ADMIN_PASSWORD_HASH`。

**P2.3 之后另需**（缺失则**启动即失败**，而不是静默连到别的库）：

```bash
# PostgreSQL DSN；口令在这一行里，所以整个文件 600 root:root
DATABASE_URL=postgres://teleport:CHANGE_ME@127.0.0.1:5432/teleport?sslmode=disable
# 连接池上限；实例与 clip / registry / serenique 共用（max_connections=100 是总预算）
DB_MAX_CONNS=8
```

> `DB_PATH` 已被 `DATABASE_URL` **取代**（不是兼容并存）：新版读到 `DB_PATH` 会忽略它。
> 启动日志**不会**打印 DSN（它含口令），所以不要指望从 journalctl 里看到库地址。

哈希用二进制自身生成（不要手搓）：

```bash
ssh hcyj '/data/services/teleport/teleport hash-password'
```

> **值里的 `$` 是雷区**：PBKDF2 哈希形如 `pbkdf2$600000$<salt>$<key>`，
> 用 `set -a; . ./env` 加载会被 shell 展开成 `pbkdf200000`。
> systemd 的 `EnvironmentFile=` 按字面读取，不受影响。
> 见 `.ai/pitfalls/cases/env-file-dollar-expansion.md`。

## Verification

```bash
# 1. 线上跑的是哪次提交
ssh hcyj '/data/services/teleport/teleport version'             # git revision；-dirty 表示构建时工作区不干净
ssh hcyj '/data/services/teleport/teleport version --frontend'  # embedded / none

# 2. 二进制可复现：本地与线上 sha256 必须一致
sha256sum bin/teleport-linux-amd64
ssh hcyj 'sha256sum /data/services/teleport/teleport'

# 3. 服务状态
ssh hcyj 'systemctl is-active teleport && systemctl is-enabled teleport'

# 4. 真实请求（大陆观测点）
curl -sI https://api.hcyj.xyz/yeciorez/teleport/api/health   # 200
curl -s  https://api.hcyj.xyz/yeciorez/teleport/api/health
```

### 4.1 缓存头（`spa.go` 改过就必须查这三条）

```bash
P=https://api.hcyj.xyz/yeciorez/teleport
curl -sI $P/assets/share.js | grep -i cache-control   # no-cache（不是 immutable）
curl -sI $P/ | grep -i cache-control                  # no-cache
H=$(curl -s $P/ | grep -o 'assets/index-[^"]*\.js' | head -1)
curl -sI $P/$H | grep -i cache-control                # public, max-age=31536000, immutable
```

`share.js` 是 `/assets/` 下**唯一**的非哈希名（`vite.config.ts` 的 `entryFileNames`
把它固定住，因为 Go 硬编码引用它）。判定必须看**名字形态**而不是目录 ——
给错方向会静默地让回访浏览器用一年旧副本。见
`.ai/pitfalls/cases/2026-10-10-share-js-cached-immutable-for-a-year.md`。

### 4.2 数据库备份（**部署前必做**）

**旧版（SQLite）**：

```bash
# 该服务器没有 sqlite3 CLI，用 python3 的 sqlite3 模块
ssh hcyj 'systemctl stop teleport && cd /data/services/teleport \
  && cp -a teleport.db teleport.db.bak-$(date +%Y%m%d-%H%M%S)'
# 必须**停服后**再拷：WAL 模式下热拷 .db 可能拿到不一致的快照
ssh hcyj 'python3 -c "
import sqlite3
c=sqlite3.connect(\"/data/services/teleport/teleport.db\")
print(c.execute(\"PRAGMA integrity_check\").fetchone()[0])
print(c.execute(\"SELECT COUNT(*) FROM reports\").fetchone()[0])
"'
```

**P2.3 之后（PostgreSQL）** —— 备份不再是「拷一个文件」，且 `teleport` 库与别的服务同实例：

```bash
# 逻辑备份；-Fc 便于 pg_restore 选择性恢复
ssh hpcore 'PGPASSWORD=$(cat /data/services/teleport/.pgpassword.tmp) \
  pg_dump -Fc -U teleport -d teleport \
  > /data/services/teleport/teleport-pg-$(date +%Y%m%d-%H%M%S).dump'
```

> 重写版的迁移器会在 `schema_migrations` 记**内容 sha256**：若 `migrations_pg/` 的
> 基线文件改过而库里的记录不匹配，服务**拒绝启动**并让你重建库或改用新迁移文件。
> 所以「回滚二进制」不等于「回滚 schema」：换回旧二进制前先确认它能读当前 schema。
> 见 `.ai/pitfalls/cases/2026-10-16-merged-baseline-hides-a-stale-test-db.md`。

备份完再 `mv` 换二进制并 `systemctl start`，停机窗口约 2 秒。
**回滚材料不止二进制，还有数据库。** 旧版的全部状态是那一个文件；重写版的是 `teleport` 库。

**不要只看 `systemctl is-active`。** 用真实 HTTP 请求验收，且必须从**大陆观测点**发 ——
境外观测点无法检测大陆封锁（`.ai/pitfalls/cases/cloudflare-free-ip-blocked-in-mainland.md`）。

## Rollback

```bash
# 保留上一版二进制，回滚即换回并重启
ssh hcyj 'cd /data/services/teleport && cp teleport teleport.bad \
  && cp teleport.prev teleport && systemctl restart teleport'
```

升级前先 `cp teleport teleport.prev`。数据库迁移是前向的（`schema_migrations`），
回滚二进制**不会**回滚 schema —— 若本次改动含迁移，回滚前先确认旧代码能读新 schema。

## 密钥轮换

见 `README.md`「安全设计要点 / 密钥轮换」。要点：改 `teleport.env` 后
`systemctl restart teleport`；轮换 `SESSION_SECRET` 会让所有已登录会话立即失效；
轮换 `AGENT_SECRET_KEY` 需要把新值交给所有调用方（包括 agent skill）。
