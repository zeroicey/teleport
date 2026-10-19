# Report-Only 的 CSP 里放 `upgrade-insecure-requests`，会被静默忽略并在 console 报错

**Severity:** 🟠 medium（不泄漏、不宕机，但会让「保险丝」失效） · **First hit:** 2026-10-18 · **Hits since:** 1

## Symptom

真实浏览器（headless Chromium over CDP）里出现一条 console error，浏览器检查因此变红：

```
security: The Content Security Policy directive 'upgrade-insecure-requests' is ignored
when delivered in a report-only policy.
```

而**响应头看起来完全正常**：`Content-Security-Policy-Report-Only` 带着完整策略，
`curl -D-` 检查每一项都符合预期，页面功能也完全正常（Vue 挂载、登录、渲染都对）。

## Root cause

两层叠加，各自都不显眼：

1. **`upgrade-insecure-requests` 只在强制执行（enforcing）的策略里合法。** 在 report-only
   策略里它被忽略——而**忽略这件事本身会被报成 console error**，不是「违反」、不是 warning。
2. **它不产生任何「CSP violation」**，所以任何「收集 violation」的思路都看不到它。它抱怨的是
   **策略本身**，而不是被策略拦下的资源。

于是它恰好躲开了所有常规检查：

| 检查 | 为什么抓不到 |
| --- | --- |
| Go 单元测试（断言 header 字符串） | 字符串是你自己写进去的，断言它存在反而会**加固**这个错误 |
| `curl -D-` 看响应头 | 头是对的：字段名、策略内容都正确 |
| CSP violation 报告 | 它不是 violation，是「指令被忽略」 |
| 页面功能测试 | 指令被忽略的**后果**是零（本来也没对象可升级），页面完全正常 |

唯一能抓到它的是**真实浏览器的 console**——因为它是一条 console error。

## Fix

把该指令从面板策略里移除：`backend/internal/views/sharepage.go` 的
`DashboardContentSecurityPolicy`。移除是安全的，因为面板策略允许的所有来源都是
`'self'` 或 `data:`，**没有任何明文子资源可升级**——这个指令在面板上本来就是空转的。

保留它的地方只有分享页的 `ContentSecurityPolicy`（那份是**强制执行**的，指令合法且有意义）。

## Guard

- `internal/views/sharepage_test.go` 的 `TestDashboardCSPAvoidsReportOnlyIncompatibleDirectives`：
  禁止面板策略出现 `upgrade-insecure-requests` / `block-all-mixed-content` / `sandbox`
  这三个「report-only 下无意义」的指令。**做过反证**：把指令加回去 → 该测试变红。
- `scripts/verify-p4.sh` 的真实浏览器阶段（`no unexpected console errors`）是第二道网，
  也是**唯一**能在本仓库里真正发现这类问题的那道——所以它不能为了「跑得快」被跳过。

**推广的规则：任何「先 Report-Only 观察」的 CSP，都必须用真实浏览器验证 console 干净。
只验证响应头内容等于没验证——本仓库已经有过一次「验证不可能失败」的教训。**
