# 后端用 Go 重写，SQLite 用纯 Go 驱动

**Status:** ✅ ACCEPTED · **Date:** 2026-10-09 · **Deciders:** lead · **Supersedes:** none

## Context

原后端是 Cloudflare Workers + Hono + D1（TypeScript）。迁到国内服务器后，服务器只有
2 vCPU / 3.5 GiB RAM、**没有 Go 工具链、没有 docker compose**，且需要长期稳定运行。

用户原话：「用 Go 语言还是用什么写都看你自己，不过 Go 语言可能方便部署一点」；
数据库指定 SQLite。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. Go 重写 | 单静态二进制、交叉编译、systemd 友好、无运行时依赖 | 要重写全部业务逻辑并逐条对齐 TS 语义 | hard（代码量已成型） |
| B. 保留 TS/Hono 跑 Node | 复用现有代码 | 服务器要有 Node 运行时；多一层部署物 | easy |
| C. Python | 上手快 | 部署依赖更多（venv/解释器版本） | easy |

## Decision

**Go 重写，stdlib `net/http`，不引 Web 框架。**

| 件 | 选型 | 理由 |
| --- | --- | --- |
| 路由 | `net/http` ServeMux（`{param}` + `PathValue`） | go 1.22+ 已够用，少一个依赖 |
| SQLite | `modernc.org/sqlite` | **纯 Go，无 cgo** → 才能 `CGO_ENABLED=0` 静态交叉编译 |
| Markdown | `yuin/goldmark` | 不设 `WithUnsafe` 即天然 XSS 安全 |
| 高亮 | `alecthomas/chroma/v2` | 服务端渲染，避免前端加载 highlight.js |
| 密码 | `crypto/pbkdf2`（stdlib） | 免第三方依赖 |

**行为对齐是硬要求**，不是「差不多」：`String.trim()` 的空白集合（ECMAScript 含
U+FEFF、**不含 U+0085 NEL**）、`length` 按 **UTF-16 code unit** 计（emoji 算 2）、
Hono `c.req.param()` **不做** URL 解码而 Go `PathValue` **会**。这些都写成了
`UTF16Len`/`JSTrim` 并配 emoji 测试。

## Consequences

- 发布产物是单个静态二进制，`scp` + `systemctl restart` 即完成部署。
- 索引 `无 cgo` 这条被固化：`store.go` 里 `_ "modernc.org/sqlite"` 空导入不能删
  （见 `.ai/pitfalls/cases/sqlite-driver-blank-import.md`）。
- 国内拉包须 `GOPROXY=https://goproxy.cn,direct`。
- 现有 TS 版全部退役：`src/routes/`、`src/services/`、`src/lib/`、`src/render/`、
  `src/middleware/`、`migrations/`、`scripts/hash-password.mjs` 均已删除。

## Revisit when

需要多实例/水平扩展（SQLite 单文件会成为瓶颈），或需要 SQLite 之外的存储特性。
