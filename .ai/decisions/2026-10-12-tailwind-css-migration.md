# 手写 CSS 迁移到 TailwindCSS v4：用 `@layer components` 承接语义类名

**Status:** ✅ ACCEPTED · **Date:** 2026-10-12 · **Deciders:** 用户（「css可以迁移到 tailwindcss」）· **Supersedes:** none

## Context

用户要求把 `web/src/styles.css`（手写 CSS，约 380 行，自定义类名
`.topbar` `.card` `.btn` `.input` `.data-table` `.badge` `.alert` `.modal` …）
迁移到 TailwindCSS。

当时的实际约束：

1. **Vue 3.5 + Vite 8（rolldown）。** Tailwind v4 官方 Vite 插件可用。
2. **暗色模式已定：`prefers-color-scheme`。** 没有 class 切换，也没有 JS 状态。
3. **分享页/交接页是 Go 服务端渲染的**，它们**不能**用 Tailwind（没有构建步骤）。
   所以 UI 优化必须**两套都改**，不能只改 SPA。
4. 既有类名被 5 个 `.vue` 文件大量使用（`KeyManagementView.vue` 一个文件就有
   上百处），全量改写模板的工作量与回归风险都不小。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A. 全量改写：删除语义类名，模板里直接堆 utility | 最"Tailwind"；样式与结构同处一地 | 5 个 view 的模板全部重写；`KeyManagementView.vue` 上千行，回归面最大；语义类名（`.btn-danger`）承载的**一致性**会散落到上百处 | hard |
| B. 只引入 Tailwind，旧 CSS 原样保留，新代码用 utility | 改动最小 | 两套体系长期并存；`@theme` token 与旧 `:root` 变量重复定义，改一处漏一处 | easy |
| C. **Tailwind 作为引擎 + `@layer components` 承接既有语义类名**（选定） | 单一 token 来源（`@theme`）；模板改动最小；语义类名保留一致性；新代码可直接用 utility | 需要理解 `@layer` 与 `@apply` 的交互；不是"纯 utility"风格 | medium |

## Decision

**C。** 规则：

1. **`web/src/app.css` 是唯一样式入口**（原 `styles.css` 删除）。结构固定为：
   `@import "tailwindcss"` → `@source "./**/*.{vue,ts}"` → `@theme` token →
   不带 layer 的 `@media (prefers-color-scheme: dark)` 覆盖 → `@layer base` →
   `@layer components`。
2. **没有 `tailwind.config.js`，没有 PostCSS。** Tailwind v4 通过
   `@tailwindcss/vite` 插件接入，配置在 CSS 里（`@theme`）。
3. **token 是唯一来源。** 颜色/圆角/时长全部定义为 `--color-*` / `--radius-*` /
   `--duration-*`，`@layer components` 里的 `@apply` 引用这些名字
   （如 `border-line`、`bg-surface`、`text-ink-muted`），模板里也能直接用。
4. **暗色覆盖必须不带 layer。** 见 Consequences 第 2 条。
5. **`@layer components` 里的顺序是有语义的，不是排版。** 两个单类选择器在同一
   layer 里特异性相同，**由源码顺序决定胜负**。当前有两条被依赖的顺序：
   - `.btn-ghost` 写在 `.btn-danger` 之前，这样 `class="btn btn-ghost btn-danger"`
     保留 danger 的 hover 边框；
   - `.login-shell` / `.login-card` 写在 `.card` **之后**，否则 `.card` 的 `p-5`
     会盖掉登录卡的 `padding: 28px`。
   见 Consequences 第 7 条 —— 这一条是踩过才补上的。
6. **`@source` 必须显式写。** 见 Consequences 第 3 条。
7. **`web/src` 里不再有任何 `<style>` 块。** 登录页原本有一个 `<style scoped>`，
   本轮搬进 `app.css`，让「唯一样式入口」这条真正成立。

## Consequences

1. **模板改动被压到最小。** 5 个 view 只需删掉对旧 CSS 的隐式依赖、补
   a11y 属性；类名基本原样保留。这让本轮能把预算花在**真正的 UI 问题**
   （对比度、焦点、键盘、触控目标）上，而不是花在机械改写上。
2. **暗色覆盖不带 layer 是刻意的，而且脆弱。** Tailwind 的 token 在
   `@layer theme` 里；不带 layer 的规则优先级更高，所以同一组 `--color-*`
   在 `@media (prefers-color-scheme: dark)` 里重新赋值就能整体换肤。
   **如果有人"顺手"把它包进 `@layer base`，暗色模式会静默失效** ——
   页面仍是浅色，不报错。验证方式是真浏览器读 computed style（见 Guard）。
3. **`@source` 不写就会静默丢样式。** Tailwind v4 自动探测有边界，本项目的
   Vite `root` 是 `web/`，模板在 `web/src/**`。显式
   `@source "./**/*.{vue,ts}"` 让扫描范围确定，不依赖自动探测的猜测。
4. **动态拼接的类名仍然不会被生成。** 本轮把 `min-w-[260px]` 这类**任意值**
   写在模板里是可以的（Tailwind 扫描字面量），但 `class="badge badge-${x}"`
   这种拼接不行。已有代码里 `.badge-ok` / `.badge-warn` / `.badge-danger`
   是完整的字面类名，安全。
5. **Go SSR 页不受影响，也没被统一。** 分享页/交接页/`/ai` 仍是各自内联的
   手写 CSS。这是接受的：它们没有构建步骤，且必须能在没有 JS 的情况下渲染。
   本轮只把它们的**字体**与 SPA 对齐（见
   `.ai/decisions/2026-10-12-self-hosted-webfonts.md`），CSS 体系仍是两套。
6. **产物体积**：CSS 从手写的 ~9 KB 变成 23.3 KB（未压缩），gzip 后约 4 KB。
   多出来的主要是 Tailwind 的 preflight 与本轮新加的 a11y 规则。
   副作用（正面）：整个应用现在只发出**一个** CSS 文件。原先登录页的 scoped
   样式会额外产出 `LoginView-*.css`，搬进全局层后不再需要。

7. **把 `<style scoped>` 搬进 `@layer components` 会改变层叠结果 —— 这是本轮的真实回归。**
   Vue 的 scoped 样式会被编译成 `.login-card[data-v-xxxx]`，**属性选择器带来额外特异性**，
   所以它能稳定压过 `.card`。搬到全局层后，两者都退化成单类选择器，
   胜负完全由源码顺序决定 —— 而当时 `.login-card` 被放在了 `.card` **之前**，
   于是 `padding: 28px` 被 `.card` 的 `p-5`（20px）静默覆盖。

   **它不会报错、不会破版，只是卡片内边距少了 8px** ——
   只有真浏览器读 computed style 才能发现（`pnpm run build`、`typecheck`、
   `go test` 全部通过）。修法是把整块移到 `.card` 之后，并在源码里写明
   「顺序是刻意的」。

   > 一般化的教训：**scoped 样式的特异性是隐式的，迁移时必须显式补回来。**
   > 只要一个规则原来靠 scoped 属性选择器赢，搬到全局后它就需要别的手段
   > （顺序、或提高特异性），否则它会被同样的对手悄悄盖掉。

## Revisit when

- 需要主题切换（暗/亮手动切换）—— 那会迫使暗色从 media query 改成 class 策略，
  第 4 条的"不带 layer"技巧要重新设计；
- Go SSR 页需要与 SPA 视觉完全统一 —— 届时要么给它们也接 Tailwind CLI 构建，
  要么抽出一份共享的、两边都能内联的 CSS；
- 某天决定彻底转向纯 utility 风格 —— 那是 Option A，需要一次性重写模板。
