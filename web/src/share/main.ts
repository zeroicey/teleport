/**
 * Share-page client bootstrap.
 *
 * Only three jobs, and each is deliberately lazy:
 *
 *   1. Copy the "expires at" countdown (cheap, no dependencies).
 *   2. If — and only if — the document contains a Mermaid diagram, dynamically
 *      import Mermaid and render it.
 *   3. If — and only if — the document frames a `format: "html"` report, grow
 *      that frame to the height of the report.
 *
 * Mermaid is ~5 MB of source. Statically importing it would put that cost on
 * every report view, including the (common) text-only ones. The dynamic import
 * below means the browser fetches Mermaid's chunk graph only when a diagram is
 * actually present, and Vite emits the rest as separate lazy chunks.
 *
 * Markdown itself is rendered server-side by the Go backend
 * (backend/internal/markdown/render.go), so this bundle stays tiny.
 */

/**
 * Render every `.mermaid` element in the document.
 *
 * Mermaid is given `securityLevel: 'strict'`, which sanitizes diagram source and
 * output through the DOMPurify copy Mermaid bundles, and disables interactive
 * click handlers — important because report content is untrusted input.
 *
 * Note that `strict` does NOT wrap the diagram in an iframe (only
 * `securityLevel: 'sandbox'` does), which is what makes it compatible with the
 * share page's `default-src 'none'` CSP whose only `frame-src` allowance is the
 * report frame this same page creates.
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

/**
 * Grow a `format: "html"` report's frame to fit the report.
 *
 * The frame is same-origin (see `views.HTMLReportSandbox`, which is empty), so
 * the document inside can simply be measured. That is the whole reason the
 * frame is not sandboxed: a sandboxed frame is an opaque origin, its
 * `contentDocument` is unreachable, and the page would be stuck at a fixed
 * height with an inner scrollbar.
 *
 * `min-height: 70vh` in the server-rendered CSS keeps the page readable when
 * this script never runs, so failure here is a cosmetic regression only.
 */
function fitReportFrames(): void {
  const frames = Array.from(document.querySelectorAll<HTMLIFrameElement>('iframe.report-frame'));
  if (frames.length === 0) return;

  const fit = (frame: HTMLIFrameElement): void => {
    // Cross-origin access throws; a frame the page did not create is not ours.
    let doc: Document | null = null;
    try {
      doc = frame.contentDocument;
    } catch {
      return;
    }
    if (!doc?.documentElement) return;

    const height = Math.max(
      doc.documentElement.scrollHeight,
      doc.body?.scrollHeight ?? 0,
      // Never shrink below the CSS fallback, or a short report would collapse.
      Math.round(window.innerHeight * 0.7),
    );
    frame.style.height = `${height}px`;
  };

  for (const frame of frames) {
    frame.addEventListener('load', () => fit(frame));
    // Already loaded before this module ran (module scripts are deferred).
    fit(frame);
    // Late layout inside the report (fonts, charts, images) changes its height.
    try {
      const doc = frame.contentDocument;
      if (doc?.documentElement) {
        new ResizeObserver(() => fit(frame)).observe(doc.documentElement);
      }
    } catch {
      /* cross-origin frame: the load handler above is the best we can do */
    }
  }

  window.addEventListener('resize', () => frames.forEach(fit));
}

/**
 * Claim a credential from a one-step hand-off link (`/k/{id}#{secret}`).
 *
 * The page itself is only a shell with instructions: the claim secret lives in
 * the URL *fragment*, which is never sent to the server, so the exchange has to
 * happen here. Two consequences shape this code:
 *
 *   - Nothing is claimed on load. A link pasted into a chat app gets fetched by
 *     that app's preview bot; claiming automatically would let a bot burn a
 *     single-use link. A human (or her agent) has to press the button, and the
 *     bot — which does not run this script, and cannot see the fragment anyway
 *     — cannot.
 *   - The result is cached in sessionStorage for this tab. The server issues a
 *     credential exactly once, so without the cache a reflexive refresh would
 *     look like "someone stole my key" when in fact the key was already shown.
 *
 * The claim endpoint is the existing public application poll, whose one-time
 * semantics (approved → claimed, then 404) are deliberate and unchanged.
 */
async function setupKeyHandoff(): Promise<void> {
  const root = document.querySelector<HTMLElement>('[data-handoff-id]');
  if (!root) return;

  const id = root.dataset.handoffId ?? '';
  // The *app* base (…/prefix), not the API base: the endpoints below add
  // `/api/` themselves.
  const appBase = (root.dataset.appBase ?? '').replace(/\/+$/, '');
  const spent = root.dataset.handoffSpent === '1';
  const status = document.getElementById('handoff-status');
  const button = document.getElementById('handoff-claim');
  const result = document.getElementById('handoff-result');
  const tokenEl = document.getElementById('handoff-token');
  const copyButton = document.getElementById('handoff-copy');
  const promptWrap = document.getElementById('handoff-prompt-wrap');
  const promptEl = document.getElementById('handoff-prompt');

  const say = (message: string, kind: 'info' | 'error' = 'info'): void => {
    if (!status) return;
    status.textContent = message;
    status.dataset.kind = kind;
  };

  const showToken = (token: string): void => {
    if (tokenEl) tokenEl.textContent = token;
    result?.removeAttribute('hidden');
    button?.setAttribute('hidden', '');
    // The paste-ready brief for an AI assistant, built here because only the
    // browser ever sees the token: the server derives it at claim time and
    // keeps only its hash.
    if (promptEl) {
      promptEl.textContent =
        `你有 Teleport 的发布权限，密钥是：${token}\n` +
        `请先读 ${appBase}/ai.md 了解接口，然后带 Authorization: Bearer ${token} ` +
        `调用 POST ${appBase}/api/reports 发布报告。`;
    }
    promptWrap?.removeAttribute('hidden');
    say('密钥只显示这一次，请立刻复制保存。');
  };

  const cacheKey = `teleport:handoff:${id}`;

  // The fragment is not available to the server, so a missing one means the
  // link was truncated in transit (chat apps often wrap long URLs).
  //
  // The format is the bare secret (`#<secret>`), not a key/value pair, because
  // that is the shape the administrator forwards and the one the guide and the
  // generated prompt both show. A `#k=<secret>` link is accepted as well so that
  // a hand-edited variant does not silently present an unclaimable page.
  const rawFragment = window.location.hash.slice(1);
  const secret = rawFragment.startsWith('k=')
    ? rawFragment.slice(2)
    : rawFragment;
  if (!secret) {
    const cached = sessionStorage.getItem(cacheKey) ?? '';
    if (cached) {
      showToken(cached);
      say('这是本标签页已领取过的密钥（刷新前的副本）。');
    } else if (spent) {
      // The server-rendered text already explains claimed/expired/unknown; a
      // missing fragment on top of that is not worth a second complaint.
      say('这条链接没有可用的领取凭证（# 后面的那段丢了），且当前不可领取。', 'error');
    } else {
      say('链接不完整：缺少 # 后面的领取凭证。请让管理员把完整链接重发一次。', 'error');
    }
    return;
  }

  // A spent link can still be rescued from this tab's cache; otherwise there is
  // nothing to claim, and the server's explanation stands.
  if (spent) {
    const cached = sessionStorage.getItem(cacheKey) ?? '';
    if (cached) {
      showToken(cached);
      say('这是本标签页已领取过的密钥（刷新前的副本）。');
    }
    return;
  }

  button?.removeAttribute('hidden');
  button?.addEventListener('click', () => {
    void (async () => {
      button.setAttribute('disabled', '');
      say('正在领取…');
      try {
        const response = await fetch(
          `${appBase}/api/agent-keys/applications/${encodeURIComponent(id)}`,
          { headers: { 'X-Teleport-Claim': secret } },
        );
        if (response.status === 404) {
          say(
            '这条链接已经领取过了，或者已经失效（密钥只能领取一次）。如果密钥没保存，请让管理员重新生成一条。',
            'error',
          );
          return;
        }
        if (response.status === 410) {
          say('这条链接已过期（或对应的申请已过期）。请让管理员重新生成一条。', 'error');
          return;
        }
        if (!response.ok) {
          say(`领取失败（HTTP ${response.status}）。请稍后重试或联系管理员。`, 'error');
          return;
        }
        const payload = (await response.json()) as {
          data?: { key?: { token?: string } };
        };
        const token = payload.data?.key?.token ?? '';
        if (!token) {
          say('领取失败：服务器没有返回密钥。请让管理员重新生成一条。', 'error');
          return;
        }
        sessionStorage.setItem(cacheKey, token);
        // Drop the secret from the address bar; it is spent now.
        history.replaceState(null, '', window.location.pathname + window.location.search);
        showToken(token);
      } catch (error) {
        console.error('Claim failed:', error);
        say('领取失败：网络错误。请稍后重试。', 'error');
      } finally {
        button.removeAttribute('disabled');
      }
    })();
  });

  copyButton?.addEventListener('click', () => {
    void (async () => {
      const token = tokenEl?.textContent ?? '';
      if (!token) return;
      try {
        await navigator.clipboard.writeText(token);
        copyButton.textContent = '已复制';
      } catch {
        // Clipboard access needs a secure context; selecting the text is the
        // honest fallback rather than a lie about having copied it.
        window.getSelection()?.selectAllChildren(tokenEl as Node);
        copyButton.textContent = '请手动复制';
      }
      window.setTimeout(() => {
        copyButton.textContent = '复制';
      }, 2000);
    })();
  });
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
fitReportFrames();
void setupKeyHandoff();
