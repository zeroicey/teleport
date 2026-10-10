# 一个 `_headers` 文件写对了规则，代码里却把整个目录当成"内容哈希"

**Severity:** 🟠 medium（线上已存在，静默失效） · **First hit:** 2026-10-10 · **Hits since:** 1

## 症状

线上（`1c5ab91`）实测：

```
GET /yeciorez/teleport/assets/share.js
  → HTTP/2 200
  → cache-control: public, max-age=31536000, immutable
```

而项目自己的契约（README「安全响应头与缓存策略」）明确要求它是 `no-cache`：

| 路径 | 要求 | 实测 |
| --- | --- | --- |
| `/assets/index-<hash>.js` | `immutable` | `immutable` ✅ |
| `/assets/share.js` | **`no-cache`** | **`immutable`** ❌ |
| `/`（SPA 外壳） | `no-cache` | `no-cache` ✅ |
| `/robots.txt`（其它稳定名） | `no-cache` | `no-cache` ✅ |

**没有报错、没有告警、页面看起来完全正常** —— 只有回访过分享页的浏览器在静默使用旧副本。

## 根因

`backend/internal/spa/spa.go` 用**目录**而不是**文件名形态**判断「是否内容哈希命名」：

```go
// 1. Hashed assets: exact matches only.
if rel == assetsDir || strings.HasPrefix(rel, assetsDir+"/") {
    if h.serveFile(w, r, rel, true) {   // ← immutable 无条件为 true
```

但 `/assets/` 里**有一个例外**：`vite.config.ts` 的 `entryFileNames` 把分享页入口固定成
稳定路径，因为 Go 在 `views/sharepage.go` 里硬编码引用 `/assets/share.js`：

```ts
entryFileNames: (chunk) =>
  chunk.name === SHARE_ENTRY_NAME ? SHARE_ENTRY_FILE : 'assets/[name]-[hash].js',
```

于是「`/assets/` 下的名字都含内容哈希」这个前提对 `share.js` 不成立。**目录级假设被一个文件级例外推翻。**

影响面恰好是最坏的一类：`share.js` 承载分享页客户端逻辑（Mermaid 懒加载、过期倒计时），
它的字节**会随部署变化**而 URL **永不变**。任何回访过的浏览器最长一年内继续执行旧副本 ——
针对分享页客户端的修复，对已看过分享页的人**不会生效**。

## 最刺眼的地方：正确答案曾经**就在仓库里**

被删除的 `public/_headers`（Cloudflare Workers Assets 时代的产物，gitignore 的遗留目录）
写着一段**完全正确、且明确点名了同一个陷阱**的注释：

```
# /assets/share.js is referenced by the Go backend at a stable URL (see
# vite.config.ts), so it must be revalidated on every load. Without this it
# would be served with long-lived caching and a browser could run stale share
# page JS for days after a deploy.
# ...
# Do not "optimise" this into `immutable` for the SPA bundles without also
# leaving the shell and share.js uncached, or a deploy can strand clients on an
# old build.

/assets/share.js
  Cache-Control: no-cache
```

也就是说：**规则在 CF 时代被显式写下过一次，迁移到 Go 托管时被重新实现成了目录级判断，
于是同一个陷阱又踩了一遍 —— 而且这次没有 `_headers` 兜着。**

> **元教训**：删掉一个组件时，它**表达过的规则**必须一起搬到新实现里，并且搬的应该是
> 「为什么」而不是「看起来像什么」。`_headers` 的路径规则被翻译成了「`/assets/` 都不可变」，
> 丢掉的正是那条注释解释的例外。

## 为什么测试没拦住

`backend/internal/spa/spa_test.go` 只覆盖了两个端点，**恰好跳过了中间那档**：

| 测试 | 夹具路径 | 断言 |
| --- | --- | --- |
| `TestServesHashedAssetsAsImmutable` | `assets/index-abc123.js` | `immutable` |
| `TestServesRealNonAssetFile` | `favicon.ico`（**不在** `/assets/` 下） | `no-cache` |

`assets/share.js` 只出现在测试夹具的表里，**从未被任何断言引用**。
即：唯一那个「在 `/assets/` 下但名字稳定」的文件，正好是唯一没被测的形态。

## Fix

**已修（2026-10-10）**：

1. `spa.go`：新增 `isContentHashed(rel)`，**把方向反过来** —— 默认「不缓存」，
   只有名字符合 Vite 的 `[name]-[hash].ext` 形态才给 `immutable`：

   ```go
   func isContentHashed(rel string) bool {
       if !strings.HasPrefix(rel, assetsDir+"/") { return false }
       name := strings.TrimPrefix(rel, assetsDir+"/")
       dot := strings.LastIndex(name, ".")
       if dot <= 0 { return false }
       stem := name[:dot]
       if strings.ContainsAny(stem, "/") { return false }
       return strings.Contains(stem, "-")
   }
   ```

   方向很关键：旧实现问「在不在 `assets/` 下」，答"是"就永久缓存；
   新实现问「名字里有没有哈希」，答"不是"就只做校验。
   **忘记维护任何东西的后果，从"一年陈旧字节"降级为"一次便宜的 304"。**
   `stableAssetFiles` 退化为断言目标（记录那个已知例外），不再参与缓存判定。

   > 已验证：真实构建的 105 个 `dist/assets` 文件里，**恰好只有 `share.js` 没有连字符** ——
   > 也就是说 104 个哈希资源照旧 `immutable`（无性能回归），只有它降为 `no-cache`。

2. `spa_test.go`：三条测试 ——
   - `TestServesStableAssetUncached`：断言 `share.js` 是 `no-cache`；
   - `TestStableAssetListMatchesBuild`：**读 `vite.config.ts` 的 `SHARE_ENTRY_FILE`
     与 `views/sharepage.go` 的硬编码引用**，断言三者一致（含 `assets/SHARE.js`、
     `assets/nested/deep/x.js` 等必须保持 `no-cache`）；
   - `TestShapeHeuristicCoversTheRealBuild`：拿真实的构建目录逐个文件核对分类。

## Guard

**按「名字形态」而不是「所在目录」分类缓存策略**，并且**默认选安全的那一侧**：

- 目录级假设一旦出现例外，例外必须有自己的测试 —— 否则它会落在两个已有测试的缝隙里。
- 白名单「什么该永久缓存」，而不是黑名单「什么是例外」：漏维护的代价是 304，不是一年陈旧。
- 缓存策略这类「错也不报错」的配置，验收时应当**直接读响应头**，而不是相信实现注释。
- 三条测试都已**反证**验过：
  - 把判定改回 `immutable` 常量 → `TestServesStableAssetUncached` 红，
    报 `Cache-Control = "public, max-age=31536000, immutable", want no-cache`；
  - 把 `vite.config.ts` 的 `SHARE_ENTRY_FILE` 改成 `assets/share2.js` →
    `TestStableAssetListMatchesBuild` 红，报「分享页没有引用它，会请求一个不存在的脚本」。
  **一个从未红过的测试不算守卫。**

## 同一轮踩到的镜像坑（值得一并记住）

给 `/llms.txt` 补绊线时，我把禁用词放宽成 `"ask the user"` 和 `"the user for a key"`。
测试立刻红了 —— 但**不是**因为旧文案，而是因为我**新写的**那句
"…so the key cannot be handed to you **by the user**" 自己命中了禁用词。
这正是本仓库另一条踩坑记录（`2026-10-10-leak-assertion-matches-own-fixture.md`）
描述的形状：**断言被自己的夹具打红**。

处理方式不是放宽断言（那会削弱守卫），而是**改文案**
（"a human can only approve it, never read it back out, so none can be handed to you"），
让断言保持宽，同时重新确认它对**真正的**旧文案仍然报红。


## 一句话

> 用目录表达「这批文件都是内容哈希命名」时，**那个不哈希的例外就是你未来的静默失效点**；
> 迁移实现时，旧组件注释里写下的**为什么**，比它的路径规则更值得搬。
