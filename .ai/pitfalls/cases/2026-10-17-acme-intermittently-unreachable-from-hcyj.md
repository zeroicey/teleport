# hcyj 到 Let's Encrypt 是间歇可达，新域名证书会卡住

**Severity:** 🟠 medium · **First hit:** 2026-10-17（P6 切换）· **Hits since:** 1

## Symptom

新加的 `teleport.hcyj.xyz` site block 配置**校验通过、Caddy 重启成功、另外两个站点照常 200**，
但新域名是：

```text
$ curl -s -o /dev/null -w "%{http_code}" https://teleport.hcyj.xyz/
000                      # curl 退出码 35 = SSL connect error
$ docker exec caddy sh -c 'ls /data/caddy/certificates/*/'
api.hcyj.xyz  clip.hcyj.xyz          # ← 没有 teleport.hcyj.xyz
$ docker logs caddy                   # 空的！
```

**注意这两处误导**：

1. **缺证书表现为 TLS 握手失败（000 / exit 35），不是 4xx。** 容易误判成「路由写错了」而去
   改 Caddyfile —— 方向完全错。
2. **`docker logs caddy` 是空的**，因为全局 `log` 块把日志重定向进了容器里的文件。
   真正的日志在 **`/var/log/caddy/access.log`**（容器内）。

## Root cause

hcyj（阿里云广州）到 **ACME API 的出网是间歇性的**。实测（同一分钟内连测）：

```text
hcyj  → https://acme-v02.api.letsencrypt.org/directory      成功 4 / 6
hpcore→ 同一个 URL                                          成功 6 / 6，0.7s
```

`directory` 偶尔能过，但签发要做 **POST /acme/new-order**（HTTP/2），实测反复超时：

```json
{"level":"error","logger":"tls.obtain","msg":"could not get certificate from issuer",
 "identifier":"teleport.hcyj.xyz",
 "error":"creating new order: attempt 1: .../acme/new-order: http2: timeout awaiting response headers"}
```

Caddy v2.10 随后**回落到 ZeroSSL**（ZeroSSL 可达，200），但它的 http-01 挑战发起后没有下文
（日志停在 `trying to solve challenge challenge=http-01`），于是签发一直悬着。

**已有站点的证书不受影响**（缓存在 Caddy 存储里，且续期走 ARI，偶尔失败会重试），
所以这是**只有新主机名才会遇到**的失败模式。

## Fix

**重启 Caddy 容器，让它重新发起签发** —— 命中一个 LE 可达的窗口就成功了：

```bash
ssh hcyj 'docker restart caddy'
# 然后轮询证书是否落盘
ssh hcyj 'docker exec caddy sh -c "find /data/caddy/certificates -name \"*.crt\""'
```

实测：**第一次重启后 10 秒内**就签下来了，而且是 **Let's Encrypt** 的证书
（`issuer = C=US, O=Let's Encrypt, CN=YR1`），与 api/clip 同一个 CA —— 没有引入新依赖。

## Guard

- **判断顺序**：先看证书是否存在，再看路由。
  `docker exec caddy sh -c 'find /data/caddy/certificates -name "*.crt"'`。
- **日志位置**：`docker exec caddy sh -c 'grep teleport.hcyj.xyz /var/log/caddy/access.log'`
  （不是 `docker logs`）。
- **重试验证方式**：`docker restart caddy` 是安全的重试手段（配置已验证过，另两个站点已验证不受影响）。
- **不要**为了绕开它把 CA 全局换成 ZeroSSL/Buypass：那是**全局**选项，会影响 api.hcyj.xyz 与
  clip 的续期，代价远大于「多重启几次」。已实测 Buypass / LE staging / Google pki.goog 从 hcyj
  都**不可达**，ZeroSSL 与 SSL.com 可达 —— 所以「换个 CA」并不是随手可用的退路。
- P6 runbook 的步骤 7 因此加了一条：**签不出证书先重启、再判断**，并把这归为「已预期的风险」
  而不是「改坏了」（`.ai/runbooks/p6-cutover.md` 的风险 6 已按实测更新）。

## 什么时候重新看

- 如果 hcyj 到 LE 的可达率继续下降（连续多次重启都签不出来）→ 那时才值得考虑
  给**单个 site block** 配 `tls { issuer ... }` 指向可达的 CA（ZeroSSL 需要 EAB），
  而不是改全局。
- 如果 Caddy 升级后 **dns-01** 可用且拿得到 DNS API 凭据 → 那是最稳的（不依赖 80/443 被境外回连）。
