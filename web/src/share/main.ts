/**
 * Share-page client bootstrap.
 *
 * Only two jobs, and both are deliberately lazy:
 *
 *   1. Copy the "expires at" countdown (cheap, no dependencies).
 *   2. If — and only if — the document contains a Mermaid diagram, dynamically
 *      import Mermaid and render it.
 *
 * Mermaid is ~5 MB of source. Statically importing it would put that cost on
 * every report view, including the (common) text-only ones. The dynamic import
 * below means the browser fetches Mermaid's chunk graph only when a diagram is
 * actually present, and Vite emits the rest as separate lazy chunks.
 *
 * Markdown itself is rendered server-side by the Worker (src/render/markdown.ts),
 * so this bundle stays tiny.
 */

/**
 * Render every `.mermaid` element in the document.
 *
 * Mermaid is given `securityLevel: 'strict'`, which sandboxes rendered
 * diagrams in an iframe and disables click handlers — important because report
 * content is untrusted input.
 */
async function renderDiagrams(): Promise<void> {
  const nodes = Array.from(document.querySelectorAll<HTMLElement>('.mermaid'));
  if (nodes.length === 0) return;

  try {
    const { default: mermaid } = await import('mermaid');

    mermaid.initialize({
      startOnLoad: false,
      // 'strict' sanitizes labels and blocks interactive/click directives.
      securityLevel: 'strict',
      theme: window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'default',
      fontFamily:
        '-apple-system, BlinkMacSystemFont, "Segoe UI", "Noto Sans SC", Roboto, sans-serif',
    });

    // A single bad diagram must not abort the rest of the page.
    const results = await Promise.allSettled(
      nodes.map((node, index) => mermaid.render(`mermaid-${index}`, node.textContent ?? '')),
    );

    let failures = 0;
    results.forEach((result, index) => {
      const node = nodes[index];
      if (!node) return;

      if (result.status === 'fulfilled') {
        node.innerHTML = result.value.svg;
        // Let the SVG scale with the container rather than overflowing.
        const svg = node.querySelector('svg');
        svg?.removeAttribute('height');
        svg?.setAttribute('width', '100%');
        node.closest('.mermaid-wrap')?.classList.add('is-rendered');
      } else {
        failures += 1;
        // Show the source so a broken diagram is diagnosable, not just blank.
        node.classList.add('mermaid-error');
        console.error('Mermaid render failed:', result.reason);
      }
    });

    if (failures > 0) {
      document.documentElement.dataset.mermaidErrors = String(failures);
    }
  } catch (error) {
    console.error('Failed to load Mermaid:', error);
    document.documentElement.dataset.mermaidLoadError = '1';
  }
}

/** Live countdown for the expiry badge, so a long-open tab stays accurate. */
function startExpiryCountdown(): void {
  const el = document.querySelector<HTMLElement>('[data-expires-at]');
  if (!el) return;

  const expiresAt = Number(el.dataset.expiresAt);
  if (!Number.isFinite(expiresAt) || expiresAt <= 0) return;

  const tick = () => {
    const remaining = expiresAt - Date.now();
    if (remaining <= 0) {
      el.textContent = '此链接已过期';
      el.classList.add('is-expired');
      return;
    }
    const totalMinutes = Math.floor(remaining / 60_000);
    const hours = Math.floor(totalMinutes / 60);
    const minutes = totalMinutes % 60;
    el.textContent =
      hours >= 24
        ? `剩余 ${Math.floor(hours / 24)} 天 ${hours % 24} 小时`
        : hours >= 1
          ? `剩余 ${hours} 小时 ${minutes} 分钟`
          : `剩余 ${Math.max(minutes, 1)} 分钟`;
  };

  tick();
  window.setInterval(tick, 30_000);
}

renderDiagrams();
startExpiryCountdown();
