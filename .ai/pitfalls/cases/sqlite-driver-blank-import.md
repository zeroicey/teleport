# SQLite 驱动靠空导入注册：删掉它仍能编译，运行时才炸

**Severity:** 🔴 high（静默编译通过） · **First hit:** 2026-10-09 · **Hits since:** 1

## Symptom

服务启动或首次查询时：

```text
sql: unknown driver "sqlite"
```

而 `go build ./...`、`go vet ./...`、`go test` 在删掉那行 import 之前**全部通过**。
错误发生在运行时，不在构建期。

## Root cause

`modernc.org/sqlite` 是纯 Go 驱动，靠包的 `init()` 向 `database/sql` 注册自己。
Go 的导入规则会**丢弃未被引用的导入**，所以必须用空白标识符显式导入：

```go
import _ "modernc.org/sqlite"
```

`database/sql` 只看到 `sql.Open("sqlite", dsn)` 里的字符串，编译期无法校验驱动是否存在。

## Fix

`backend/internal/store/store.go` 里的 `_ "modernc.org/sqlite"` **不能删**。
本项目特意用纯 Go 驱动（无 cgo）来支持 `CGO_ENABLED=0` 静态交叉编译 ——
见 `.ai/decisions/2026-10-09-go-sqlite-backend.md`。

## Guard

- `store.go` 里保留注释说明这行的用途（Go 的空导入在 review 时最容易被当噪音删掉）。
- 真实 DB 断言放在测试里：任何 `store` 包的测试都会在驱动缺失时立刻失败，
  所以 `pnpm run backend:test` 是这条的回归网。
- 发布构建 `CGO_ENABLED=0` 也能顺带暴露"是否意外引入了 cgo 依赖"。
