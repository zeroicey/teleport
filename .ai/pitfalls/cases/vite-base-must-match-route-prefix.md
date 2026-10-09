# Vite 的 `base` 必须跟着部署前缀走，否则根绝对路径会打到别的服务

**Severity:** 🟠 medium · **First hit:** 2026-10-09 · **Hits since:** 0（设计期避开）

## Symptom

应用挂在 `/yeciorez/teleport/` 而不是域名根路径下时，页面白屏或资源 404，
`index.html` 里引用的是 `/assets/index-xxxx.js`。

## Root cause

Vite 默认 `base: '/'`，构建产物用**根绝对路径**引用资源。而
`api.hcyj.xyz` 是**共享主机**（兜底反代到 :3000 的另一个服务），
`/assets/...` 会打到别的服务、或在当前服务上 404。

同一个根因还会命中 `createWebHistory('/')` 与相对接口路径。

## Fix

前缀**只写一处**，由 `ROUTE_PREFIX` 统一推导：

| 位置 | 取法 |
| --- | --- |
| `vite.config.ts` | `base` 由 `ROUTE_PREFIX` 推导 |
| 前端路由 | `createWebHistory(import.meta.env.BASE_URL)` |
| API 客户端 | `web/src/api.ts` 的前缀取自 `import.meta.env.BASE_URL` |

三者同源，不可能漂移。

## Guard

**构建后直接看产物**：`backend/internal/webui/dist/index.html` 里**不应**出现任何
`src="/assets/...` 这种根绝对路径。

```bash
grep -n 'src="/assets/\|href="/assets/' backend/internal/webui/dist/index.html
# 期望：无输出
```

## 关联

忘了 `pnpm run build` 就 `go build`，`//go:embed` 打进去的是旧产物 ——
见 `.ai/decisions/2026-10-09-single-self-contained-binary.md`。
