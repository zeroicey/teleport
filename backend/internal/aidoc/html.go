package aidoc

import (
	"strings"

	"github.com/zeroicey/teleport/backend/internal/markdown"
)

// ContentTypeMarkdown is the response type for the raw guide. Agents that fetch
// this get the source text verbatim, which is what they want: no HTML parsing,
// no extraction step.
const ContentTypeMarkdown = "text/markdown; charset=utf-8"

// CSP for the human-readable HTML rendering.
//
// Stricter than the share page's policy: this page loads no scripts at all, so
// `default-src 'none'` needs only the inline-style relaxation that the embedded
// stylesheet requires. There is no `script-src`, so even a future mistake that
// injected a <script> tag would be blocked.
const CSP = "default-src 'none'; " +
	"style-src 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// HTML renders the guide as a standalone, script-free page.
//
// Deliberately not built on the SPA: an agent (or a user with JS off) must be
// able to read this page directly, and the SPA renders client-side.
func HTML(f Facts) string {
	body := markdown.New().Render(Markdown(f)).HTML

	title := "Teleport — 给 AI 的使用说明"
	if f.AppBase != "" {
		title = "Teleport — 给 AI 的使用说明（" + f.AppBase + "）"
	}

	var b strings.Builder
	b.Grow(len(body) + 4096)
	b.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="robots" content="noindex, nofollow, noarchive" />
<meta name="color-scheme" content="light dark" />
<title>`)
	b.WriteString(markdown.Escape(title))
	b.WriteString(`</title>
<style>
:root{--bg:#fff;--fg:#1f2328;--fg-muted:#59636e;--border:#d1d9e0;--code-bg:#f6f8fa;--link:#0969da}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--fg-muted:#9198a1;--border:#3d444d;--code-bg:#151b23;--link:#4493f8}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);
 font:16px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans SC",sans-serif}
main{max-width:820px;margin:0 auto;padding:40px 20px 96px}
h1{font-size:1.7rem;line-height:1.3;margin:0 0 1.2rem}
h2{font-size:1.25rem;margin:2.4rem 0 .8rem;padding-bottom:.35rem;border-bottom:1px solid var(--border)}
h3{font-size:1.05rem;margin:1.8rem 0 .6rem}
a{color:var(--link)}
code{background:var(--code-bg);padding:.15em .35em;border-radius:4px;
 font:.88em/1.5 ui-monospace,SFMono-Regular,Menlo,monospace}
pre{background:var(--code-bg);border:1px solid var(--border);border-radius:6px;
 padding:12px 14px;overflow-x:auto}
pre code{background:none;padding:0;font-size:.86rem}
table{border-collapse:collapse;width:100%;margin:1rem 0;font-size:.92rem;display:block;overflow-x:auto}
th,td{border:1px solid var(--border);padding:7px 11px;text-align:left;vertical-align:top}
th{background:var(--code-bg);white-space:nowrap}
blockquote{margin:1rem 0;padding:.1rem 1rem;border-left:3px solid var(--border);color:var(--fg-muted)}
hr{border:0;border-top:1px solid var(--border);margin:2rem 0}
ul,ol{padding-left:1.4rem}
li{margin:.25rem 0}
/* chroma token colours (inline theme, no external stylesheet) */
.chroma .c,.chroma .ch,.chroma .cm,.chroma .c1,.chroma .cs,.chroma .cp,.chroma .cpf{color:var(--fg-muted);font-style:italic}
.chroma .k,.chroma .kc,.chroma .kd,.chroma .kn,.chroma .kp,.chroma .kr,.chroma .kt{color:#cf222e}
.chroma .s,.chroma .s1,.chroma .s2,.chroma .sb,.chroma .sc,.chroma .sd,.chroma .se,.chroma .sh,.chroma .si,.chroma .sx,.chroma .sr,.chroma .ss,.chroma .dl{color:#0a3069}
.chroma .m,.chroma .mb,.chroma .mf,.chroma .mh,.chroma .mi,.chroma .mo,.chroma .il{color:#0550ae}
.chroma .nf,.chroma .fm,.chroma .nc,.chroma .nn,.chroma .ne,.chroma .ni,.chroma .nl,.chroma .py{color:#8250df}
@media (prefers-color-scheme:dark){
 .chroma .k,.chroma .kc,.chroma .kd,.chroma .kn,.chroma .kp,.chroma .kr,.chroma .kt{color:#ff7b72}
 .chroma .s,.chroma .s1,.chroma .s2,.chroma .sb,.chroma .sc,.chroma .sd,.chroma .se,.chroma .sh,.chroma .si,.chroma .sx,.chroma .sr,.chroma .ss,.chroma .dl{color:#a5d6ff}
 .chroma .m,.chroma .mb,.chroma .mf,.chroma .mh,.chroma .mi,.chroma .mo,.chroma .il{color:#79c0ff}
 .chroma .nf,.chroma .fm,.chroma .nc,.chroma .nn,.chroma .ne,.chroma .ni,.chroma .nl,.chroma .py{color:#d2a8ff}
}
</style>
</head>
<body>
<main>
`)
	b.WriteString(body)
	b.WriteString(`
</main>
</body>
</html>
`)
	return b.String()
}

// LLMSTxt is the discovery index for agents that look for the convention file.
//
// It is intentionally a pointer, not a copy: one canonical guide is easier to
// keep true than two documents that drift.
func LLMSTxt(f Facts) string {
	var b strings.Builder
	b.WriteString("# Teleport\n\n")
	b.WriteString("> Report publishing service with expiring share links. ")
	b.WriteString("Publish a Markdown report via one HTTP call and hand the returned ")
	b.WriteString("share URL to the user.\n\n")
	b.WriteString("## 使用说明（给 AI）\n\n")
	b.WriteString("- [" + f.AppBase + "/ai.md](" + f.AppBase + "/ai.md)")
	b.WriteString(": full usage guide as plain Markdown — read this first.\n")
	b.WriteString("- [" + f.AppBase + "/ai](" + f.AppBase + "/ai)")
	b.WriteString(": the same guide as a web page.\n\n")
	b.WriteString("## 关键事实\n\n")
	b.WriteString("- Base URL: `" + f.AppBase + "`\n")
	b.WriteString("- Auth: `Authorization: Bearer <AGENT_KEY>` for `POST /api/reports`.\n")
	b.WriteString("- Get a key yourself: `POST " + f.AppBase + "/api/agent-keys/applications` with ")
	b.WriteString("`{label, purpose?, requestedHours?}` (201), then poll ")
	b.WriteString("`GET " + f.AppBase + "/api/agent-keys/applications/{id}` with the returned ")
	b.WriteString("`claim_secret` in an `X-Teleport-Claim` header. A human approves in the ")
	b.WriteString("dashboard, and the plaintext key is returned exactly once, on the first ")
	b.WriteString("poll after approval. The key is self-service by design: a human can only ")
	b.WriteString("approve it, never read it back out, so none can be handed to you. ")
	b.WriteString("Details: " + f.AppBase + "/ai.md\n")
	b.WriteString("- Health (public): `GET " + f.AppBase + "/api/health`\n")
	b.WriteString("- List your own reports: `GET " + f.AppBase + "/api/reports` (Bearer). ")
	b.WriteString("Nothing else recovers a report id you forgot — asking about someone else's ")
	b.WriteString("answers 404.\n")
	b.WriteString("- Fix a report instead of republishing: `PATCH ")
	b.WriteString(f.AppBase + "/api/reports/{id}` updates it IN PLACE, so links already ")
	b.WriteString("handed out serve the new content rather than breaking. Send only the ")
	b.WriteString("fields you changed; the only updatable ones are title, content, category, ")
	b.WriteString("format, metadata. An unknown field (e.g. `Content`) is a 400, not a ")
	b.WriteString("silent no-op, and an empty patch is a 400 too.\n")
	b.WriteString("- `DELETE " + f.AppBase + "/api/reports/{id}` removes the report AND every ")
	b.WriteString("share link to it: holders get 404 immediately. No undo, no recycle bin. ")
	b.WriteString("To take a link back but keep the report, revoke the link instead.\n")
	b.WriteString("- Report format: use `markdown`. HTML is stored but not rendered.\n")
	b.WriteString("- Share links expire; `404` = unknown or revoked, `410` = expired.\n")
	b.WriteString("- Renew before you lapse: every authenticated response carries ")
	b.WriteString("`X-Teleport-Key-Expires-At` (Unix seconds; absent = never expires) and ")
	b.WriteString("`X-Teleport-Key-Expired: true` once you are already past it. An expired key ")
	b.WriteString("keeps exactly one ability for a 90-day grace period — `POST ")
	b.WriteString(f.AppBase + "/api/agent-keys/renewals`. Renewing keeps the same key id ")
	b.WriteString("and therefore your earlier reports; re-applying does not.\n")
	return b.String()
}
