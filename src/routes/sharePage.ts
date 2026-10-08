/**
 * Server-rendered public share page.
 *
 *   GET /s/:token   — no authentication
 *
 * Rendering server-side (rather than shipping a SPA shell and resolving the
 * token client-side) is what lets us return a real 404/410 status for unknown,
 * revoked or expired links — which is the whole point of an expiring share.
 *
 * Rendering pipeline
 *   Markdown -> HTML happens HERE, in the Worker, via src/render/markdown.ts
 *   (markdown-it with `html: false`, so raw HTML is escaped, never executed).
 *   Code fences are syntax-highlighted server-side.
 *
 *   Mermaid diagrams are the one exception: the fence is emitted as
 *   `<pre class="mermaid">` and a small external script (`/assets/share.js`)
 *   lazy-loads Mermaid on the client only when such an element exists. Mermaid
 *   is ~5 MB, so shipping it to every text-only report view would be wasteful.
 *
 * Content Security Policy
 *   `script-src 'self'` with no inline scripts at all — the page contains zero
 *   executable inline JS, so there is no nonce to manage on the content path.
 *   `style-src` must allow 'unsafe-inline' because Mermaid injects <style> into
 *   the SVG it renders at runtime; that is the only relaxation, and it cannot
 *   execute script.
 */
import { Hono } from 'hono';
import { ApiError } from '../lib/errors';
import { parseShareToken, requiredParam } from '../lib/validate';
import { renderMarkdown, escapeHtml } from '../render/markdown';
import { ReportsService } from '../services/reports';
import type { AppEnv } from '../types';

const sharePage = new Hono<AppEnv>();

/**
 * Feature flag: raw HTML rendering is off until a sanitizer is wired up.
 * Flipping this on without DOMPurify + a nonce-based CSP is an XSS hole.
 */
const HTML_MOUNT_ENABLED = false;

/**
 * Content Security Policy for the share page.
 *
 * `default-src 'none'` is the baseline: everything not explicitly allowed is
 * blocked. Note there is deliberately no `frame-src`, which means no iframe can
 * be created.
 *
 * That is safe because Mermaid runs with `securityLevel: 'strict'` (see
 * web/src/share/main.ts), and in Mermaid 12 STRICT does NOT use an iframe —
 * only `securityLevel: 'sandbox'` does. Strict instead sanitizes diagram source
 * through the DOMPurify copy Mermaid bundles. Switching to 'sandbox' would
 * require adding `frame-src data:` to this policy, and would weaken the
 * isolation boundary, so 'strict' is the right choice.
 *
 * The one relaxation is `style-src 'unsafe-inline'`: Mermaid injects <style>
 * into the SVG it renders at runtime. Allowlisting a stylesheet hash is not
 * possible because those styles are generated per diagram. Style injection
 * cannot execute script, so this does not open an XSS path.
 */
const CONTENT_SECURITY_POLICY = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data:",
  "font-src 'self'",
  "connect-src 'self'",
  "base-uri 'none'",
  "form-action 'none'",
  "frame-ancestors 'none'",
  'upgrade-insecure-requests',
].join('; ');

sharePage.get('/:token', async (c) => {
  const token = parseShareToken(requiredParam(c.req.param('token'), 'token'));
  const service = new ReportsService(c.env.DB);

  const result = await service.resolveShare(token);
  if (result.status === 'missing') throw ApiError.notFound('Share link not found');
  if (result.status === 'gone') throw ApiError.gone('This share link has expired');

  c.executionCtx.waitUntil(service.recordView(token).catch(() => undefined));

  const { report, share } = result.payload;

  // Never cache a page whose lifetime is deliberately bounded.
  c.header('Cache-Control', 'no-store, must-revalidate');
  c.header('Content-Security-Policy', CONTENT_SECURITY_POLICY);
  c.header('Content-Type', 'text/html; charset=utf-8');

  return c.body(renderSharePage({ report, share, htmlMountEnabled: HTML_MOUNT_ENABLED }));
});

export default sharePage;

interface RenderOptions {
  report: {
    id: string;
    title: string;
    category: string;
    format: string;
    content: string;
    metadata: Record<string, unknown>;
    created_at: number;
    updated_at: number;
  };
  share: { token: string; expires_at: number; view_count: number };
  htmlMountEnabled: boolean;
}

function renderSharePage({ report, share, htmlMountEnabled }: RenderOptions): string {
  const neverExpires = share.expires_at === 0;

  // Mermaid only loads when the rendered document actually contains a diagram.
  const rendered = report.format === 'markdown' ? renderMarkdown(report.content) : null;

  const bodyHtml = rendered
    ? rendered.html
    : htmlMountEnabled
      ? '<p class="notice">HTML 渲染尚未启用。</p>'
      : '<p class="notice">此报告为 HTML 格式，但 HTML 渲染尚未启用。</p>';

  return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="robots" content="noindex, nofollow, noarchive" />
<meta name="color-scheme" content="light dark" />
<title>${escapeHtml(report.title)}</title>
<style>
  :root {
    color-scheme: light dark;
    --fg:#18181b; --fg-muted:#71717a; --bg:#ffffff; --surface:#fafafa;
    --border:#e4e4e7; --accent:#2563eb; --code-bg:#f6f8fa;
    --kw:#cf222e; --str:#0a3069; --num:#0550ae; --com:#6e7781; --fn:#8250df; --attr:#953800;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --fg:#e4e4e7; --fg-muted:#a1a1aa; --bg:#0f1115; --surface:#17171c;
      --border:#2a2a32; --accent:#60a5fa; --code-bg:#161b22;
      --kw:#ff7b72; --str:#a5d6ff; --num:#79c0ff; --com:#8b949e; --fn:#d2a8ff; --attr:#ffa657;
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
  .expiry.is-expired { color:#dc2626; }

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

  /* highlight.js token colours (hand-rolled theme, no CSS import) */
  .hljs-comment, .hljs-quote { color:var(--com); font-style:italic; }
  .hljs-keyword, .hljs-selector-tag, .hljs-literal, .hljs-doctag,
  .hljs-name, .hljs-strong { color:var(--kw); }
  .hljs-string, .hljs-regexp, .hljs-addition, .hljs-meta .hljs-string { color:var(--str); }
  .hljs-number, .hljs-symbol, .hljs-bullet, .hljs-variable,
  .hljs-template-variable, .hljs-link, .hljs-selector-attr { color:var(--num); }
  .hljs-title, .hljs-section, .hljs-title.function_, .hljs-built_in { color:var(--fn); }
  .hljs-attr, .hljs-attribute, .hljs-property, .hljs-selector-class,
  .hljs-selector-id, .hljs-type { color:var(--attr); }
  .hljs-emphasis { font-style:italic; }

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
      <span class="badge">${escapeHtml(report.category)}</span>
      <span class="badge">${escapeHtml(report.format)}</span>
      <span class="expiry" data-expires-at="${share.expires_at}">${
        neverExpires ? '永不过期' : '计算中…'
      }</span>
    </div>
    <h1>${escapeHtml(report.title)}</h1>
  </header>

  <div class="meta">
    <span>报告 ID / Report: <span class="mono">${escapeHtml(report.id)}</span></span>
    <span>创建 / Created: ${escapeHtml(formatTs(report.created_at))}</span>
    <span>更新 / Updated: ${escapeHtml(formatTs(report.updated_at))}</span>
    <span>浏览 / Views: ${share.view_count}</span>
  </div>

  <main id="report-root">${bodyHtml}</main>

  <footer>
    <p>由 Teleport 发布 · 此链接具有时效性，可能随时失效。</p>
    <p>Published via Teleport. This link expires and may be revoked at any time.</p>
  </footer>
</div>
<script type="module" src="/assets/share.js"></script>
</body>
</html>`;
}

function formatTs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  return new Date(ms).toISOString().replace('T', ' ').slice(0, 16) + ' UTC';
}
