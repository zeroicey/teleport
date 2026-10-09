# 前端嵌进 Go 二进制，部署只剩一个文件

**Status:** ✅ ACCEPTED · **Date:** 2026-10-09 · **Deciders:** user（提议）+ lead · **Supersedes:** none

## Context

用户原话：「其实你可以在本地把 前端编译好的代码 和 go 代码一起编译到一个可执行，
这样你部署也方便，通过 go embed」。

服务器上不装 Node、不放静态目录，就不存在「二进制与静态资源版本不一致」这一类故障。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. `//go:embed` 把 Vite 产物打进二进制 | 部署 = 一个文件；资源与代码永不漂移 | 改前端也要重新编译后端；二进制变大（约 15 MB） | easy |
| B. 二进制旁边放 `dist/` 目录 | 前后端可独立更新 | 两个部署物必须同步，迟早漂移 | easy |
| C. Go 里手写 HTML | 无构建步骤 | 放弃 Vue 组件化 | hard |

## Decision

Vite 产物输出到 `backend/internal/webui/dist/`，由 `//go:embed` 打进二进制；
发布构建 `scripts/build.sh` 顺序固定为**先前端后后端**。

关键设计：`embed_frontend` 构建标签切换 `webui` 的两套实现 —— 没有它时走「无前端」桩，
所以**干净仓库里 `go build` / `go test` / `go vet` 依然可用**；只有发布构建显式打开它。

## Consequences

- 部署流程：`pnpm run build:release` → `scp` → `mv` 原子替换 → `systemctl restart`。
  升级时**先传 `.new` 再 `mv`**，不要 `scp` 直接写目标（运行中的进程会看到残缺可执行文件）。
- `git describe --tags --always --dirty` 编进 `main.version`；`teleport version` 可查线上
  跑的是哪次提交，`-dirty` 表示构建时工作区不干净。同 revision 构建**逐字节可复现**，
  可用 `sha256sum` 对比本地与线上。
- `backend/internal/webui/dist/` 是生成物，**已 gitignore，不要手改、不要提交**。
- 改前端却忘了 `pnpm run build`，`//go:embed` 打进去的就是旧产物 —— 这是本设计的主要
  操作陷阱。

## Revisit when

需要独立、频繁地更新前端而不重新发布后端（例如前端改用 CDN 分发，但大陆可达性
要求见 `.ai/decisions/2026-10-09-mainland-reachability-single-entry.md`）。
