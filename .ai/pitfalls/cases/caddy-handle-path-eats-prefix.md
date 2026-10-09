# `handle_path` 会吃掉路由前缀，让所有路由 404

**Severity:** 🟠 medium · **First hit:** 2026-10-09 · **Hits since:** 0（设计期避开）

## Symptom

Caddy 里若写 `handle_path /yeciorez/teleport*`，前端、API、分享页**全部 404**，
但 Caddy 配置本身合法、`adapt` 无报错。

## Root cause

`handle_path` 的语义是「匹配后把匹配到的前缀**剥掉**再转发」。而本项目的 Go 服务把
**所有**路由都挂在前缀下（`cfg.RoutePrefix + "/api/..."`、`+"/s/{token}"`），
分享链接里也写死了该前缀。前缀被剥掉后，后端收到的是 `/api/reports`，
而它只认 `/yeciorez/teleport/api/reports`。

## Fix

用 `handle`（保留前缀），不用 `handle_path`：

```caddyfile
handle /yeciorez/teleport* {
    reverse_proxy 172.17.0.1:8788 {
        header_up Host {host}
        header_up X-Real-IP {remote}
        header_up X-Forwarded-For {remote}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

前缀是**数据**而不是装饰：Go 路由、`vite.config.ts` 的 `base`、`web/src/api.ts` 的
`BASE_URL`、分享链接、Cookie `Path` 全部由 `ROUTE_PREFIX` 推导。剥掉它等于同时破坏这五处。

## Guard

- `README.md`「部署 / Caddy 分流」第 1 条 + 本文件。
- 验收方式：新增路由后从大陆观测点 `curl -sI` 真实路径，确认 200 而非 404。
- 同时注意：站点级安全头要用 `@notTeleport not path /yeciorez/teleport*` **排除**本前缀，
  否则与后端更严格的策略重复下发、互相冲突。
