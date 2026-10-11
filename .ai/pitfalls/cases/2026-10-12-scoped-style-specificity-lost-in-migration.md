# Vue 的 `<style scoped>` 搬进全局层：特异性丢失，规则被静默覆盖

**Severity:** 🟠 medium（不报错、不破版，只有一处内边距变了） · **First hit:** 2026-10-12 · **Hits since:** 1

## 症状

把登录页的 `<style scoped>` 搬进 `web/src/app.css` 的 `@layer components` 之后：

```css
.login-card { padding: 28px; }   /* 期望 */
```

真浏览器读 computed style 得到：

```
card padding 28px  →  实测 20px
```

**页面看起来完全正常**，登录卡仍然是居中的白色卡片，只是内边距比设计少了 8px。
`pnpm run typecheck`、`pnpm run build`、`go test ./...` **全部通过**；
只有一条「真浏览器读 computed style」的断言抓到了它。

## 根因

原来的 scoped 样式会被 Vue 编译成**带属性选择器**的形式：

```css
.login-card[data-v-9f3a1c2e] { padding: 28px }
.card[data-v-9f3a1c2e]       { padding: 20px }   /* 假设 */
```

属性选择器 `[data-v-…]` 给 `.login-card` 增加了特异性
（`0,2,0` 而不是 `0,1,0`），所以它能**稳定压过** `.card`，与两者在文件里的先后无关。

搬进全局层后，scoped 属性消失，两边都退化成**单类选择器**（`0,1,0`）。
特异性相同 → **由源码顺序决定胜负**。而当时我把登录块放在了
`.card` 的**前面**（为了跟"布局"小节排在一起），于是后出现的 `.card`
赢下了 `padding`。

`@apply p-5` 展开成 `padding: calc(var(--spacing) * 5)` ——
一个 **shorthand**，会覆盖 `.login-card` 的 `padding: 28px`，不是只覆盖一部分。

> 同一文件里还有第二条靠顺序成立的规则：`.btn-ghost` 必须写在 `.btn-danger` 之前，
> 否则 `class="btn btn-ghost btn-danger"` 会丢掉 danger 的 hover 边框。
> 那条是上一轮就有的，本轮**差点**因为重排而一起丢掉 ——
> 说明"顺序即语义"这件事在这个文件里已经出现过两次。

## 为什么没有测试拦住

三道既有闸门都测不到：

| 闸门 | 为什么测不到 |
| --- | --- |
| `pnpm run typecheck`（vue-tsc） | 类型层面完全合法，CSS 不是它的关注点 |
| `pnpm run build` | 构建成功，`.login-card` 规则确实被产出（`grep` 得到 1 处命中） |
| `go test ./...` | 后端无关 |
| 手工看页面 | 20px vs 28px 在截图里几乎看不出，且"卡片有内边距"这个判断仍然成立 |

**`grep` 能证明规则被产出，不能证明规则生效。** 这两件事在层叠世界里完全不同。

## Fix

**已修（2026-10-12）**：

1. 把 `/* -- sign-in screen -- */` 整块移到 `.card` **之后**；
2. 在 `.login-card` 上方写明**为什么**要放在这个位置，而不是只写"放在这里"：

```css
/* Declared AFTER `.card` on purpose. Both are single-class selectors in the
   same layer, so source order decides: `.card` sets `p-5`, and this must
   override it. The original lived in a `<style scoped>` block, where Vue's
   attribute selector added specificity — moving it here without moving the
   order silently shrank the card's padding. */
```

3. 顺手把"顺序即语义"的两条规则都写进了
   `.ai/decisions/2026-10-12-tailwind-css-migration.md` 的 Decision 第 5 条。

## Guard

1. **迁移 scoped 样式到全局层时，逐条问「它原来是靠什么赢的」。**
   如果答案是"靠 scoped 属性选择器"，那它在全局层就需要**另一个**保证
   （源码顺序、或显式提高特异性），否则会被同样的对手盖掉。
2. **验收必须读 computed style，不能只 grep 产物。**
   本轮新增的 `/tmp/uiverify/logincheck.mjs` 断言的是
   `getComputedStyle(card).padding === '28px'`、`maxWidth === '400px'`、
   `display === 'flex'`、以及渐变是否真的 accent 着色 —— 都是**计算后**的值。
3. **`@layer components` 里的顺序是有语义的，重排前先看注释。**
   该文件里已有两条被依赖的顺序（`.btn-ghost` 早于 `.btn-danger`；
   `.login-card` 晚于 `.card`），两条都写了理由。
4. **断言要针对性质，不要针对序列化字符串。**
   写这条守卫时我第一版断言 `backgroundImage.includes('37, 99, 235')` 并失败了 ——
   因为浏览器把 `color-mix(in srgb, var(--color-accent) 12%, transparent)`
   序列化成 `color(srgb 0.145098 0.388235 0.921569 / 0.12)`。
   正确做法是解析出 RGB 再断言"蓝色占优"，而不是匹配一个**永远不会出现在
   computed style 里**的 token 名。

## 一句话

> `<style scoped>` 的**属性选择器是隐式特异性** —— 把它搬进全局层时，
> 那份特异性不会跟着走，规则会被一个原本输给它的对手悄悄盖掉，
> 而且构建、类型检查、单元测试**全都不会响**。
