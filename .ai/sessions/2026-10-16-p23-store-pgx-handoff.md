# teleport — P2.3 完成（store 已换成 pgx + sqlc）+ 四路复审

**Date:** 2026-10-16 · **Branch/Commit:** `master`（工作区未提交，基于 `1315b08`）· **Duration:** 一个会话
· **State:** P0 ✅ / P1 ✅ / **P2 ✅（P2.1–P2.3）**；**P3 ⛔ 未开工**；线上旧版仍 `disabled`，公网 502
· **Gate:** `scripts/check.sh`（无库档）与 `scripts/test-pg.sh`（强制真库）**两档都绿**，`-race` 干净

---

## 一、先读什么

1. `.ai/CURRENT_TASK.md` —— 状态 + 「P2.3 复审」小节（这一节是本会话的结论压缩）。
2. `.ai/decisions/2026-10-16-p2-store-port-technique.md` —— P2.3 的做法（B 大爆炸）与 5 项缓解。
3. 本会话新增的 2 个 pitfall（都踩真了）：
   - `2026-10-16-decide-renewal-granted-twice.md`（🔴 真 bug，复审发现）
   - `2026-10-16-store-nested-pool-acquire-deadlocks.md`（🔴 自伤，写代码时踩到）

## 二、本会话交付了什么

| 交付 | 位置 |
| --- | --- |
| store 换成 pgx + sqlc（40 个方法） | `backend/internal/store/{store,queries,keys}.go` |
| sqlc 查询源（SQL 唯一真源） | `backend/internal/store/queries/{reports,keys}.sql` |
| sqlc 生成物（勿手改） | `backend/internal/store/storedb/`、配置 `backend/sqlc.yaml` |
| `sqlc diff` 漂移闸门 | `scripts/check-sqlc-sync.sh`，接进 `scripts/check.sh` |
| 三处上限的 advisory lock + 4 个**决定性**守卫 | `pg.go`（`lockCap`、`Lock*`）、`lock_guard_pg_test.go` |
| 续期行锁 + 2 个守卫（含前置行数检查） | `queries/keys.sql`（`LockRenewalForDecision`）、`keys.go`、`lock_guard_pg_test.go` |
| 有界锁等待 | `pg.go`（`lock_timeout=10s`、`idle_in_transaction_session_timeout=60s`）+ `TestOpenPGBoundsLockWaits` |
| `view_count` 64 位守卫 | `migrations_pg/0001_init.sql`、`pg_test.go`（`TestPGCountersAre64Bit`） |
| 配置改名 + 负向测试 | `config.go`（`DATABASE_URL` 必填、`DB_MAX_CONNS` 默认 8）、`config_test.go` |
| 删掉的 SQLite 残留 | `internal/store/{store.go 的 Open/Migrate/migrationFS, migrations/}`、`schema.sql`、`scripts/check-schema-sync.mjs`、`migration_upgrade_test.go`、go.mod 里的 `modernc.org/sqlite` 全家 |

测试数：`go test ./...` 对真库 0 失败；无库档 143 个 SKIP（闸门现在会**明确报出**这个数字）。

### 怎么跑

```bash
cd /data/dev/teleport
export TEST_PG_DSN="postgres://teleport:$(sudo -n cat /data/services/teleport/.pgpassword.tmp)@127.0.0.1:5432/teleport_test?sslmode=disable"
pnpm run backend:test-pg            # 必需的真库闸门（无 DSN 直接退出）
env -u TEST_PG_DSN pnpm run check   # 无库档；结尾会打印 skipped 数量
cd backend && go test ./internal/store/ -count=1 -race
```

## 三、已验证 / 未验证

- [x] 两档闸门绿；`go test ./...` 对真库绿；`store` + `api` 的 `-race` 绿。
- [x] `sqlc generate` 成功、`sqlc diff` 无漂移；两张动态 `SET` 列表确实能表达为单条静态语句
      （ADR 的 Revisit 条件未触发）。
- [x] **反证过**：删 `lockCap` → 4 个顾问锁守卫必红；删 `FOR UPDATE` → 行锁守卫必红；
      把守卫 UPDATE 的行数检查删掉（改造前的行为）→ 续期守卫必红；`view_count` 改回 INTEGER → 必红。
      反证都在 `/tmp` 的副本里做，仓库未被污染（副本已删）。
- [x] `pg_advisory_xact_lock` 的键与 clip 不冲突；上限在 24 竞争者 × 5 轮下精确。
- [ ] **假定、未测：HTTP 端到端契约**（真 agent 密钥 POST / 读分享页 / PATCH / DELETE / 410 / 404 /
      `HEAD` 405）—— P4。**本会话的绿 =「冻结断言对 PG 绿」，不是「在野契约对 PG 绿」。**
- [ ] **假定、未测：真实浏览器、大陆观测点、数据搬运核对、部署** —— P4/P5/P6。
- [ ] **假定、未测：`lock_timeout` 在生产负载下的合适性**（10s 是本地推理，不是压测结论）。
- [ ] **假定、未测：`DB_MAX_CONNS=8` 在真实流量下的足够性**（同实例共用 100）。
- [ ] 复审用的是子代理（oracle/reviewer），它们的结论我逐条复核过；但复审本身没有再被第三方审计。

## 四、Loose ends

- [ ] **未提交**。工作区有 30+ 文件的改动（含删除）；提交前请人类批准（仓库规则）。
      提交信息需写明 ADR 缓解 #1 要求的「改了哪条断言、属于哪条冻结契约」。
- [ ] `migrations_pg/0001_init.sql` 的**内容哈希变了**（`view_count` 改 BIGINT）。任何已用它建过库的
      地方会在启动时被哈希守卫拒绝 —— 已知的只有 `teleport_test`（残留 schema 已清理）与空的
      `teleport` 库（0 张表，会正常 bootstrap）。**P5 建库前确认 `teleport` 仍是空的。**
- [ ] `cap_lock_pg_test.go` 的守卫是**模式级**（手写 SQL），`lock_guard_pg_test.go` 才是 store 级。
      两者职责已写进各自注释，别把前者当成后者。
- [ ] `Store.Close()` 现在被 `main.go` 使用（原先无人调用）。
- [ ] 复审提出的报告型（未改）项：`testStoreHandle` 每次 helper 调用开一个 1 连接池（实测峰值 14，
      在预算内）；`sqlc.yaml` 的 `emit_pointers_for_null_types` 与 clip 不同（已注明理由）。
- [ ] P3 要把 `context` 串起来 —— 届时 `store.callCtx` 应换成请求 context，有界等待才算完整。

## 五、不要重复

1. **删掉锁之后统计型并发测试常常抓不到。** 实测：删 `LockActiveKeys` 后 `TestConcurrentClaimsRespectActiveCap`
   只 8/10 抓到；`LockPendingApplications` 删掉后时而抓到时而漏（一次分组运行直接 `ok`）。
   要证明「方法真的拿了锁」，就**持有那把锁、断言方法阻塞**（`lock_guard_pg_test.go` 的做法）。
2. **第一版「行锁守卫」是假的**：只断言「DecideRenewal 会阻塞」，而删掉 `FOR UPDATE` 后它**照样阻塞**
   （阻塞在后一条守卫 UPDATE 上）。守卫必须**反证过**；「阻塞了」不是性质。
3. **事务内绝不能用 `s.q` / `s.pool`。** 会向池再要一条连接，而自己占着一条；小池 + N 竞争者
   = 无超时永久挂死（pgxpool 会无限等）。只用 `s.q.WithTx(tx)`。
4. **SQLite 的单写者免费保护了所有「先读后写」，不只是计数。** P2.3 只列了 3 处「计数—插入」，
   `DecideRenewal`（先读 status 再写）就被漏掉并真的发作了。按**形状**枚举：先读某物、再据它写。
5. **合并式基线的文件改了就一定要重建库。** 哈希守卫会拒绝启动（这是设计），不要绕过。
   顺带：`teleport_test` 里遗留的 `public` 与 `t_*` schema 是 P1 的 stale 残骸，已清；
   夹具现在每测一个 schema，正常结束时不留残留（被 kill 的运行会留，需手动 DROP）。
6. **`time.Sleep` 在守卫里是可以的**，如果它观测的是「锁状态随时间变化」；但**不要**用它代替
   反证。`lock_guard_pg_test.go` 里的 sleep 有注释说明它在观测什么。
7. **`gofmt` 吃撇号**：注释里别写 `store's`，会烧掉 shellcheck/gofmt 的解析（本仓库既有 pitfall）。

## 六、下一步（第一个动作）

按纲领继续 **P3**：`internal/httpx` → Gin 中间件、`internal/api` 路由与 handler 移植，
**每个状态码逐条对齐**。开工前先读 `.ai/decisions/2026-10-16-rewrite-on-gin-and-postgres.md`
的 P3 行与冻结清单，并确认 `pnpm run backend:test-pg` 仍绿（P3 会动 `internal/api` 的测试）。

同时建议：**在提交 P2.3 之前**，让人类看一眼 `migrations_pg/0001_init.sql` 的哈希变化
（§四第 2 条），因为它影响 P5/P6 的建库步骤。
