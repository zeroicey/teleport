# 部署 Teleport 到 hcyj

**Applies to:** 生产（hcyj / `api.hcyj.xyz`） · **Owner:** lead · **Last verified:** 2026-10-10

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
pnpm run check                 # 闸门必须先绿
pnpm run build:release         # → bin/teleport-linux-amd64
```

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
`backend/deploy/teleport.env.example`。必填三项：
`AGENT_SECRET_KEY`、`SESSION_SECRET`、`ADMIN_PASSWORD_HASH`。

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
