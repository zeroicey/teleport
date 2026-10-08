/**
 * Markdown -> HTML rendering for the public share page.
 *
 * Design: this runs in the Worker, at request time.
 *
 *   - markdown-it with `html: false`. Raw HTML in the source is escaped, never
 *     passed through, so a report author cannot inject markup. This is the
 *     single most important XSS control in the whole render path.
 *   - Code blocks are highlighted here too, using a curated highlight.js
 *     language subset — importing all ~190 languages would bloat the Worker
 *     bundle for no real benefit on personal reports.
 *   - Mermaid blocks are NOT rendered here. Mermaid is ~5 MB, so the fence is
 *     emitted as `<pre class="mermaid">` and a small client script lazy-loads
 *     Mermaid only when such an element exists. See web/src/share/main.ts.
 *
 * markdown-it's own `validateLink` rejects `javascript:`, `vbscript:` and
 * dangerous `data:` URLs; we rely on it rather than hand-rolling a filter.
 */
import MarkdownIt from 'markdown-it';
import hljs from 'highlight.js/lib/core';

// Curated language set: the categories this platform actually publishes
// (security/pentest, architecture, development progress).
import bash from 'highlight.js/lib/languages/bash';
import c from 'highlight.js/lib/languages/c';
import cpp from 'highlight.js/lib/languages/cpp';
import csharp from 'highlight.js/lib/languages/csharp';
import css from 'highlight.js/lib/languages/css';
import diff from 'highlight.js/lib/languages/diff';
import dockerfile from 'highlight.js/lib/languages/dockerfile';
import go from 'highlight.js/lib/languages/go';
import http from 'highlight.js/lib/languages/http';
import ini from 'highlight.js/lib/languages/ini';
import java from 'highlight.js/lib/languages/java';
import javascript from 'highlight.js/lib/languages/javascript';
import json from 'highlight.js/lib/languages/json';
import markdown from 'highlight.js/lib/languages/markdown';
import nginx from 'highlight.js/lib/languages/nginx';
import php from 'highlight.js/lib/languages/php';
import plaintext from 'highlight.js/lib/languages/plaintext';
import powershell from 'highlight.js/lib/languages/powershell';
import python from 'highlight.js/lib/languages/python';
import ruby from 'highlight.js/lib/languages/ruby';
import rust from 'highlight.js/lib/languages/rust';
import shell from 'highlight.js/lib/languages/shell';
import sql from 'highlight.js/lib/languages/sql';
import typescript from 'highlight.js/lib/languages/typescript';
import xml from 'highlight.js/lib/languages/xml';
import yaml from 'highlight.js/lib/languages/yaml';

const LANGUAGES: Record<string, Parameters<typeof hljs.registerLanguage>[1]> = {
  bash,
  c,
  cpp,
  csharp,
  css,
  diff,
  dockerfile,
  go,
  http,
  ini,
  java,
  javascript,
  json,
  markdown,
  nginx,
  php,
  plaintext,
  powershell,
  python,
  ruby,
  rust,
  shell,
  sql,
  typescript,
  xml,
  yaml,
};

for (const [name, definition] of Object.entries(LANGUAGES)) {
  hljs.registerLanguage(name, definition);
}

// Friendly aliases so ```` ```ts ````, ```` ```sh ````, ```` ```yml ```` work.
const ALIASES: Record<string, string> = {
  sh: 'bash',
  zsh: 'bash',
  console: 'bash',
  ts: 'typescript',
  js: 'javascript',
  jsx: 'javascript',
  tsx: 'typescript',
  py: 'python',
  yml: 'yaml',
  ps1: 'powershell',
  html: 'xml',
  svg: 'xml',
  md: 'markdown',
  text: 'plaintext',
  txt: 'plaintext',
  conf: 'ini',
  toml: 'ini',
  cxx: 'cpp',
  cs: 'csharp',
  golang: 'go',
  rs: 'rust',
};

function resolveLanguage(raw: string): string | null {
  const name = raw.trim().toLowerCase();
  if (!name) return null;
  if (name in LANGUAGES) return name;
  return ALIASES[name] ?? null;
}

/** Highlight code, always returning a complete, escaped <pre><code> block. */
function highlightCode(code: string, language: string | null): string {
  if (language && hljs.getLanguage(language)) {
    try {
      const { value } = hljs.highlight(code, { language, ignoreIllegals: true });
      return `<pre class="code-block" data-lang="${escapeHtml(language)}"><code class="hljs language-${escapeHtml(language)}">${value}</code></pre>`;
    } catch {
      // Fall through to the escaped plain-text path below.
    }
  }

  // No language (or highlighting failed): escape rather than emit raw source.
  const label = language ? ` data-lang="${escapeHtml(language)}"` : '';
  return `<pre class="code-block"${label}><code class="hljs">${escapeHtml(code)}</code></pre>`;
}

const md = new MarkdownIt({
  html: false, // never trust report content as markup
  linkify: true,
  breaks: false,
  typographer: false,
});

// Replace the default fence renderer so we can special-case Mermaid and take
// full control of the emitted markup. (Because this rule is fully overridden,
// markdown-it's own `highlight` option is intentionally unused.)
md.renderer.rules.fence = (tokens, idx) => {
  const token = tokens[idx];
  if (!token) return '';

  const info = token.info.trim();
  const firstWord = info.split(/\s+/)[0] ?? '';

  // Check Mermaid before language resolution: `mermaid` is not a highlight.js
  // language, so resolveLanguage() would return null for it.
  if (firstWord.toLowerCase() === 'mermaid') {
    // Mermaid reads `textContent`, so the source must be HTML-escaped here.
    // It is rendered client-side into an SVG.
    return `<div class="mermaid-wrap"><pre class="mermaid">${escapeHtml(token.content)}</pre></div>`;
  }

  return highlightCode(token.content, resolveLanguage(firstWord));
};

/**
 * Harden link/image rendering.
 *
 * markdown-it already blocks dangerous protocols, but external links in a
 * report should not be able to reach back into the page via `window.opener`,
 * and images must be same-origin or data URIs (the CSP also enforces this).
 */
const defaultLinkOpen =
  md.renderer.rules.link_open ??
  ((tokens, idx, options, _env, self) => self.renderToken(tokens, idx, options));

md.renderer.rules.link_open = (tokens, idx, options, env, self) => {
  const token = tokens[idx];
  // attrGet is typed `string | number | null`, so coerce before testing.
  const href = String(token?.attrGet('href') ?? '');
  if (/^https?:\/\//i.test(href)) {
    token?.attrSet('target', '_blank');
    token?.attrSet('rel', 'noopener noreferrer nofollow');
  }
  return defaultLinkOpen(tokens, idx, options, env, self);
};

export interface RenderResult {
  html: string;
  /** True when the document contains at least one Mermaid diagram. */
  hasMermaid: boolean;
}

/** Render report Markdown to safe HTML for embedding in the share page. */
export function renderMarkdown(source: string): RenderResult {
  const html = md.render(source);
  return { html, hasMermaid: /class="mermaid"/.test(html) };
}

export function escapeHtml(value: unknown): string {
  return String(value ?? '').replace(/[&<>"']/g, (ch) =>
    ch === '&' ? '&amp;'
    : ch === '<' ? '&lt;'
    : ch === '>' ? '&gt;'
    : ch === '"' ? '&quot;'
    : '&#39;',
  );
}

/** Resolved language list, exported for documentation/tests. */
export const SUPPORTED_LANGUAGES: string[] = Object.keys(LANGUAGES);
