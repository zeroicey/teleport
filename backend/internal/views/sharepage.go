// Package views renders server-side HTML.
package views

import (
	"strconv"
	"strings"
	"time"

	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/markdown"
)

// ContentSecurityPolicy is the CSP for the share page.
//
// `default-src 'none'` is the baseline: everything not explicitly allowed is
// blocked, and there is deliberately no `frame-src`, so no iframe can be
// created.
//
// The one relaxation is `style-src 'unsafe-inline'`: Mermaid injects <style>
// into the SVG it renders at runtime, and those styles are generated per
// diagram so they cannot be hashed ahead of time. Style injection cannot execute
// script, so this does not open an XSS path.
const ContentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'; " +
	"upgrade-insecure-requests"

// HTMLMountEnabled gates raw HTML rendering. It stays off: enabling it without
// a sanitizer and a nonce-based CSP would be an XSS hole.
const HTMLMountEnabled = false

// SharePageOptions carries everything the share template needs.
type SharePageOptions struct {
	Report domain.Report
	Share  domain.ShareToken
	// AssetBaseURL is the absolute base the page's static assets are served
	// from, e.g. https://teleport.zeroicey.me.
	//
	// The original implementation used a root-relative "/assets/share.js",
	// which only works when the page and its assets share an origin. In this
	// deployment the share page is proxied to the backend while the assets are
	// the frontend's static build, so an absolute URL is required for direct
	// backend-origin access and remains same-origin (satisfying the
	// `script-src 'self'` policy) on the normal path.
	AssetBaseURL string
}

// RenderSharePage renders the public share page.
func RenderSharePage(opts SharePageOptions) string {
	report := opts.Report
	share := opts.Share
	neverExpires := share.ExpiresAt == 0
	assetBase := strings.TrimRight(opts.AssetBaseURL, "/")

	bodyHTML := `<p class="notice">此报告为 HTML 格式，但 HTML 渲染尚未启用。</p>`
	if report.Format == domain.FormatMarkdown {
		bodyHTML = markdown.New().Render(report.Content).HTML
	} else if HTMLMountEnabled {
		bodyHTML = `<p class="notice">HTML 渲染尚未启用。</p>`
	}

	var b strings.Builder
	b.Grow(len(report.Content) + 8192)

	b.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="robots" content="noindex, nofollow, noarchive" />
<meta name="color-scheme" content="light dark" />
<title>`)
	b.WriteString(markdown.Escape(report.Title))
	b.WriteString(`</title>
<style>
  :root {
    color-scheme: light dark;
    --fg:#18181b; --fg-muted:#71717a; --bg:#ffffff; --surface:#fafafa;
    --border:#e4e4e7; --accent:#2563eb; --code-bg:#f6f8fa;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --fg:#e4e4e7; --fg-muted:#a1a1aa; --bg:#0f1115; --surface:#17171c;
      --border:#2a2a32; --accent:#60a5fa; --code-bg:#161b22;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin:0; background:var(--bg); color:var(--fg);
    font:16px/1.75 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans SC",Roboto,Helvetica,Arial,sans-serif;
    -webkit-text-size-adjust:100%;
  }
  .wrap { max-width: 820px; margin: 0 auto; padding: 48px 20px 96px; }

  .eyebrow { display:flex; gap:8px; align-items:center; flex-wrap:wrap; font-size:13px; color:var(--fg-muted); margin-bottom:14px; }
  .badge { border:1px solid var(--border); border-radius:999px; padding:2px 10px; font-size:12px; white-space:nowrap; }
  .expiry { color: var(--accent); font-variant-numeric: tabular-nums; }

  h1 { font-size:1.9rem; line-height:1.3; margin:0 0 18px; letter-spacing:-0.01em; }

  .meta {
    border-top:1px solid var(--border); border-bottom:1px solid var(--border);
    padding:12px 0; margin:0 0 32px; font-size:13px; color:var(--fg-muted);
    display:grid; gap:5px;
  }
  .meta .mono { font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; font-size:12px; }

  #report-root { overflow-wrap: break-word; }
  #report-root h2 { font-size:1.35rem; margin:2.2em 0 .7em; padding-bottom:.3em; border-bottom:1px solid var(--border); }
  #report-root h3 { font-size:1.15rem; margin:1.8em 0 .6em; }
  #report-root h4 { font-size:1rem; margin:1.5em 0 .5em; }
  #report-root p { margin:0 0 1.1em; }
  #report-root ul, #report-root ol { padding-left:1.5em; margin:0 0 1.1em; }
  #report-root li { margin:.3em 0; }
  #report-root a { color:var(--accent); }
  #report-root blockquote {
    margin:0 0 1.1em; padding:.4em 1em; color:var(--fg-muted);
    border-left:3px solid var(--border);
  }
  #report-root hr { border:0; border-top:1px solid var(--border); margin:2em 0; }
  #report-root img { max-width:100%; height:auto; border-radius:6px; }

  #report-root table { border-collapse:collapse; width:100%; margin:0 0 1.2em; font-size:14px; display:block; overflow-x:auto; }
  #report-root th, #report-root td { border:1px solid var(--border); padding:8px 12px; text-align:left; }
  #report-root th { background:var(--surface); font-weight:600; }

  /* inline code */
  #report-root :not(pre) > code {
    background:var(--code-bg); border:1px solid var(--border);
    border-radius:4px; padding:.15em .4em;
    font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; font-size:.875em;
  }

  /* code blocks */
  .code-block {
    position:relative; background:var(--code-bg); border:1px solid var(--border);
    border-radius:8px; padding:14px 16px; overflow-x:auto;
    font-size:13.5px; line-height:1.6; margin:0 0 1.2em;
  }
  .code-block code {
    font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
    background:none; border:0; padding:0; font-size:inherit;
  }
  .code-block[data-lang]::before {
    content:attr(data-lang); position:absolute; top:6px; right:10px;
    font-size:11px; color:var(--fg-muted); text-transform:lowercase;
    font-family:ui-monospace,monospace; pointer-events:none;
  }

  /* chroma token colours (hand-rolled theme, no CSS import) */
  .chroma .c, .chroma .ch, .chroma .cm, .chroma .c1, .chroma .cs, .chroma .cp, .chroma .cpf { color:var(--fg-muted); font-style:italic; }
  .chroma .k, .chroma .kc, .chroma .kd, .chroma .kn, .chroma .kp, .chroma .kr, .chroma .kt { color:#cf222e; }
  .chroma .s, .chroma .s1, .chroma .s2, .chroma .sb, .chroma .sc, .chroma .sd, .chroma .se, .chroma .sh, .chroma .si, .chroma .sx, .chroma .sr, .chroma .ss, .chroma .dl { color:#0a3069; }
  .chroma .m, .chroma .mb, .chroma .mf, .chroma .mh, .chroma .mi, .chroma .mo, .chroma .il { color:#0550ae; }
  .chroma .nf, .chroma .fm, .chroma .nc, .chroma .nn, .chroma .ne, .chroma .ni, .chroma .nl, .chroma .py { color:#8250df; }
  .chroma .na, .chroma .nb, .chroma .bp, .chroma .nt, .chroma .nv, .chroma .vc, .chroma .vg, .chroma .vi, .chroma .vm { color:#953800; }
  .chroma .o, .chroma .ow, .chroma .p { color:inherit; }
  .chroma .err { color:#dc2626; }
  @media (prefers-color-scheme: dark) {
    .chroma .k, .chroma .kc, .chroma .kd, .chroma .kn, .chroma .kp, .chroma .kr, .chroma .kt { color:#ff7b72; }
    .chroma .s, .chroma .s1, .chroma .s2, .chroma .sb, .chroma .sc, .chroma .sd, .chroma .se, .chroma .sh, .chroma .si, .chroma .sx, .chroma .sr, .chroma .ss, .chroma .dl { color:#a5d6ff; }
    .chroma .m, .chroma .mb, .chroma .mf, .chroma .mh, .chroma .mi, .chroma .mo, .chroma .il { color:#79c0ff; }
    .chroma .nf, .chroma .fm, .chroma .nc, .chroma .nn, .chroma .ne, .chroma .ni, .chroma .nl, .chroma .py { color:#d2a8ff; }
    .chroma .na, .chroma .nb, .chroma .bp, .chroma .nt, .chroma .nv, .chroma .vc, .chroma .vg, .chroma .vi, .chroma .vm { color:#ffa657; }
  }

  /* mermaid */
  .mermaid-wrap { margin:0 0 1.4em; }
  .mermaid { background:transparent; text-align:center; margin:0; overflow-x:auto; }
  .mermaid:not(.is-rendered):not(.mermaid-error) { opacity:.55; }
  .mermaid-error {
    border:1px solid #dc2626; border-radius:8px; padding:12px 14px;
    text-align:left; font-size:12.5px; color:#dc2626; white-space:pre-wrap;
  }
  .mermaid[data-processed] svg { max-width:100%; height:auto; }

  .notice {
    border:1px solid var(--border); border-left:3px solid var(--accent);
    border-radius:6px; padding:12px 16px; color:var(--fg-muted);
  }

  footer { margin-top:64px; font-size:12px; color:var(--fg-muted); border-top:1px solid var(--border); padding-top:16px; }
  footer p { margin:.3em 0; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div class="eyebrow">
      <span class="badge">`)

	b.WriteString(markdown.Escape(report.Category))
	b.WriteString(`</span>
      <span class="badge">`)
	b.WriteString(markdown.Escape(string(report.Format)))
	b.WriteString(`</span>
      <span class="expiry" data-expires-at="`)
	b.WriteString(strconv.FormatInt(share.ExpiresAt, 10))
	b.WriteString(`">`)
	if neverExpires {
		b.WriteString("永不过期")
	} else {
		b.WriteString("计算中…")
	}
	b.WriteString(`</span>
    </div>
    <h1>`)
	b.WriteString(markdown.Escape(report.Title))
	b.WriteString(`</h1>
  </header>

  <div class="meta">
    <span>报告 ID / Report: <span class="mono">`)
	b.WriteString(markdown.Escape(report.ID))
	b.WriteString(`</span></span>
    <span>创建 / Created: `)
	b.WriteString(markdown.Escape(formatTS(report.CreatedAt)))
	b.WriteString(`</span>
    <span>更新 / Updated: `)
	b.WriteString(markdown.Escape(formatTS(report.UpdatedAt)))
	b.WriteString(`</span>
    <span>浏览 / Views: `)
	b.WriteString(strconv.FormatInt(share.ViewCount, 10))
	b.WriteString(`</span>
  </div>

  <main id="report-root">`)
	b.WriteString(bodyHTML)
	b.WriteString(`</main>

  <footer>
    <p>由 Teleport 发布 · 此链接具有时效性，可能随时失效。</p>
    <p>Published via Teleport. This link expires and may be revoked at any time.</p>
  </footer>
</div>
<script type="module" src="`)
	b.WriteString(markdown.Escape(assetBase))
	b.WriteString(`/assets/share.js"></script>
</body>
</html>
`)
	return b.String()
}

// formatTS renders an epoch-millisecond timestamp as UTC, matching the original
// `new Date(ms).toISOString().replace('T',' ').slice(0,16) + ' UTC'`.
func formatTS(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04") + " UTC"
}
