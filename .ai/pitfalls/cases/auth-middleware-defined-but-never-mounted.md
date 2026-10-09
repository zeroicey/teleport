# 鉴权中间件定义了却没挂上，写接口静默放行

**Severity:** 🔴 high（未授权写） · **First hit:** 2026-10-08 · **Hits since:** 1

## Symptom

`POST /api/reports` **不带** `Authorization` 头也返回 **201**，报告被真的写进库。
`requireAgentAuth` 函数存在、逻辑正确、单测也过 —— 只是从来没有被挂到路由上。

## Root cause

中间件是**定义**与**挂载**两件事。在 Hono（及同类框架）里逐条路由挂载时，
漏挂一条不会报错：那条路由就按无中间件处理，行为与"故意公开"完全一样。
这是最危险的一类缺陷 —— 功能测试全绿，安全属性为零。

## Fix

**在 router 级挂载**，而不是逐条路由挂：

```ts
reports.use('*', requireAgentAuth)   // 之后新增的路由默认就是受保护的
```

原则：**默认安全**。新增路由不该要求作者记得再挂一次中间件。

验证矩阵（三种都必须拒）：

| 请求 | 期望 |
| --- | --- |
| 无 `Authorization` | 401 |
| 错误的 Bearer 值 | 401 |
| `Basic ...`（错误的 scheme） | 401 |
| 正确 Bearer | 201 |

## Guard

- 每个受保护路由都要有**负例**测试（无凭据 / 错凭据 / 错 scheme），不能只测成功路径。
  `internal/api` 的鉴权矩阵就是这条的回归网。
- 迁移到 Go 时同类保证改成显式包装：`mux.Handle("POST "+p+"/api/reports",
  requireAgent(http.HandlerFunc(s.handleCreateReport)))` —— 没包装就没法编译通过评审。
