# `no-cache` 配 `embed.FS`：没有校验器，重新验证就是全量重下

**Severity:** 🟠 medium（静默性能回退，功能完全正常） · **First hit:** 2026-10-12 · **Hits since:** 2

## 症状

自托管字体上线后，用真实二进制核对响应头，发现**每一个** woff2 都是：

```
GET /yeciorez/teleport/fonts/outfit-001-6c18d579.woff2
  → HTTP/1.1 200 OK
  → Cache-Control: no-cache
```

真浏览器实测：冷缓存下**每次整页加载重新下载 260–830 KB** 字体切片
（登录页 5 片 / 260 KB，报告详情页 15 片 / 830 KB）。

功能上一切正常 —— 页面能开、字体正确、没有报错。**只有流量和首屏时间在无声地变差。**

## 根因

这是**两条各自都正确的规则叠在一起**才产生的：

**第一条**：`spa.go` 的缓存策略刻意做成"默认不安全"——
只有名字能自证内容不变（内容哈希）的 URL 才给 `immutable`，
其余一律 `no-cache`。这个方向是对的（见
`.ai/pitfalls/cases/2026-10-10-share-js-cached-immutable-for-a-year.md`）。

**第二条**：字体二进制放在 `web/public/fonts/`，文件名由取材脚本决定，
**不带**内容哈希 —— 因为当时只想着"文件名稳定好维护"。

两条叠加的结果是：字体拿到 `no-cache`。

**而 `no-cache` 并不等于"便宜"，它等于"用之前先验证"。**
问题在于这里**根本没有东西可以验证**：

```go
http.ServeContent(w, r, name, info.ModTime(), rs)
```

`info` 来自 `fs.Stat(h.fsys)`，而 `embed.FS` 的 `ModTime()` 返回**零值**。
`http.ServeContent` 的文档明确写着：modtime 为零值时**不设置 `Last-Modified`**。
既没有 `Last-Modified` 也没有 `ETag` → 浏览器**没有任何校验器可发** →
只能当作未命中，**重新下载整个 body**。

也就是说：`no-cache` 在这套实现下退化成了"每次都重新下载"，
而这句话在代码注释里读起来完全像"每次都会便宜地校验一下"。

> 一个更早的同类信号：`share.js` 那条记录里写着
> "忘记维护任何东西的后果，从'一年陈旧字节'降级为'一次便宜的 304'"。
> **本轮证明那个"便宜的 304"当时并不存在** —— 从来没有 ETag，
> 零 modtime 也没有 `Last-Modified`，所以降级到的其实是"一次完整的重新下载"。
> 当时的推理在一个环节上是错的，而它藏在注释里，没人复核。

## 为什么没有测试拦住

原有的缓存测试只断言 `Cache-Control` **这个字符串**：

```go
if got := rec.Header().Get("Cache-Control"); got != "no-cache" { ... }
```

它对"这个 `no-cache` 实际上要花多少字节"完全没有断言。
`no-cache` 是**手段**，测试却把它当成了**目的**。

## Fix

**已修（2026-10-12）**，三处，缺一不可：

1. **字体二进制改成内容寻址**：`<slug>-<NNN>-<sha256[:8]>.woff2`。
   名字本身证明了字节不变，于是可以安全地拿 `immutable`，**根本不进入重新验证**。
   `spa.go` 新增 `fontsDir` 与 `fontNameHasContentHash`，
   `isContentHashed` 现在识别两个目录、两种生产者（Vite / 取材脚本）。
   `fonts/fonts.css` 保持稳定名（Go 模板要能直接引用），因此仍然 `no-cache`。

2. **给稳定名加 ETag**：`Handler.etagFor` 惰性计算 sha256 并按进程缓存
   （构建内嵌在二进制里，进程生命周期内不变，所以一次哈希就够）。
   于是 `no-cache` 终于能退化成**真正的 304**。
   同时 `serveIndex` 从裸 `Write` 改成 `ServeContent` ——
   裸 `Write` 的处理器**不会**被 `net/http` 处理条件请求，
   那样 ETag 会被发出然后被无视，比不加更糟。

3. **`serveFile` 里那处硬编码的 `false`**：规则 2（"构建里的真实文件"）
   原本写死 `h.serveFile(w, r, rel, false)`。字体不在 `assets/` 下，
   所以走的是这条分支 —— **`isContentHashed` 即使完全正确也永远不会被问到**。

   > 这条最值得记：单元测试测的是 `isContentHashed("fonts/…") == true`，
   > 它**通过了**，而线上行为是错的，因为**调用点**写死了 `false`。
   > **纯函数测试无法覆盖"有没有人问它"。**

## Guard

1. **缓存策略的验收必须读"实际传输的字节"，不能只读 `Cache-Control` 字符串。**
   新增的两条测试断言的是可观察结果：
   - `TestFontCacheControlThroughTheHandler`：走 `ServeHTTP` 读响应头，
     断言哈希字体是 `immutable`、`fonts.css` 是 `no-cache`；
   - `TestRevalidationActuallySavesTheBody`：发条件请求，断言
     **304 且 body 长度为 0**。
   **两条都已反证验过**（把调用点改回 `false` / 去掉 ETag，两条各自变红）。
2. **`no-cache` 必须配一个校验器。** 没有 `ETag` 也没有 `Last-Modified` 的
   `no-cache` 是"每次都全量重下"的同义词。内嵌文件系统尤其容易踩，
   因为 modtime 恒为零。
3. **纯函数正确 ≠ 行为正确。** 判定函数有测试之后，
   还要有一条**穿过真实调用路径**的测试，否则漏掉的正是"没人调用它"。

## 同一轮踩到的镜像坑：中间件判得太晚，把响应弄坏了

为了让 `fonts.css`（原始 107 KB）别再裸传，新增了 gzip 中间件。
第一版在**第一次 `Write`** 时决定是否压缩，看起来合理 —— 因为那时
`Content-Type` 已经设好了。

实际是坏的。`http.ServeContent` 的顺序是：
**先设 `Content-Length`、先 `WriteHeader`，再写 body**。
等第一次 `Write` 被调用时，header 早就刷出去了，
此时再 `Del("Content-Length")` 和 `Set("Content-Encoding")` 都是对
**已经发出去的 header** 做修改 —— 无效。

结果客户端被告诉 `Content-Length: 107422` 且**没有** `Content-Encoding`，
却收到了 gzip 字节。curl 直接报：

```
curl: (18) end of response with 74451 bytes missing
```

**这不是"少压了一层"，这是响应被截断。**

修法是把判定**提前到 header 刷出之前**：`WriteHeader` 只记录状态、
**不刷 header**，压缩与否完全由元数据（状态码、`Content-Type`、
已知的 `Content-Length`）决定；长度未知时缓冲到 `minCompressSize` 再决定，
`Flush` 则让流式响应直接退出压缩。

守卫是 `TestCompressSurvivesServeContent`：用真的 `http.ServeContent`
跑一遍，断言编码头、`Content-Length` 已删除、且 body 能解压回原字节。
**同样反证验过**（把 `WriteHeader` 改回立即 `commit`，测试变红）。

## 一句话

> `no-cache` 不是"便宜"，是"先验证" —— 没有校验器的 `no-cache` 就是全量重下；
> 而判定缓存与编码的时机，必须在 header 刷出**之前**，
> 否则你改的是一个已经发出去的 header。
