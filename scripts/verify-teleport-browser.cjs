#!/usr/bin/env node
/**
 * Real-browser verification of teleport, for stage P4.
 *
 * The Go tests and scripts/verify-p4-contract.sh cover the HTTP contract. This covers
 * what only a browser can answer: does the SPA bundle actually execute, does the
 * router guard send an anonymous visitor to the login form, does logging in really
 * create a session and load the data, and does the share page's client script render
 * Mermaid under the CSP it is served with.
 *
 * It deliberately drives raw CDP rather than adding puppeteer/playwright to the repo:
 * the dependency would exist for this one file, Node 22 has a global WebSocket, and
 * the same choice was already made for clip (scripts/verify-clip-browser.cjs). Only
 * the Chromium binary is borrowed, from the Playwright cache.
 *
 * Usage:
 *   node scripts/verify-teleport-browser.cjs <base-url> <prefix> <admin-password> [mermaid-share-token]
 *
 * Exit codes: 0 all checks passed, 1 a check failed, 2 the browser could not start.
 */
'use strict';

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const net = require('node:net');
const path = require('node:path');

const BASE = process.argv[2] || 'http://127.0.0.1:8788';
const PREFIX = process.argv[3] || '/yeciorez/teleport';
const PASSWORD = process.argv[4];
const SHARE = process.argv[5] || '';

if (!PASSWORD) {
  console.error('usage: verify-teleport-browser.cjs <base-url> <prefix> <admin-password> [share-token]');
  process.exit(2);
}

// The Playwright cache ships a revision-pinned directory; a hardcoded revision stops
// working for everyone else (and for us after the next browser update), so it is
// overridable and the newest cached revision is discovered rather than assumed.
function findChrome() {
  if (process.env.CHROME) return process.env.CHROME;
  const root = path.join(process.env.HOME || '', '.cache', 'ms-playwright');
  let entries = [];
  try {
    entries = fs.readdirSync(root).filter((d) => /^chromium-\d+$/.test(d)).sort();
  } catch {
    return '';
  }
  for (const dir of [...entries].reverse()) {
    const p = path.join(root, dir, 'chrome-linux64', 'chrome');
    if (fs.existsSync(p)) return p;
  }
  return '';
}

const OUT = process.env.VERIFY_OUT || path.join(__dirname, '..', 'bin', 'e2e');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const results = [];
const check = (name, ok, detail = '') => {
  results.push({ name, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  — ${detail}` : ''}`);
};

async function getJSON(url, method = 'GET') {
  const res = await fetch(url, { method });
  if (!res.ok) throw new Error(`${method} ${url} -> ${res.status}`);
  return res.json();
}

/** A CDP session over one page target. */
class Session {
  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.pending = new Map();
    this.console = [];
    this.failed = [];
    this.dialogs = [];
    ws.onmessage = (ev) => {
      // A frame that does not parse must not throw inside the socket handler: that
      // would surface as an unhandled error in the middle of a verification run and
      // mask the check that was actually in flight.
      let m;
      try {
        m = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (m.id && this.pending.has(m.id)) {
        const { resolve, reject } = this.pending.get(m.id);
        this.pending.delete(m.id);
        if (m.error) reject(new Error(`${m.error.message} (${JSON.stringify(m.error.data ?? '')})`));
        else resolve(m.result);
        return;
      }
      if (m.method === 'Runtime.consoleAPICalled' && ['error', 'warning'].includes(m.params.type)) {
        this.console.push(m.params.args.map((a) => a.value ?? a.description ?? '').join(' '));
      }
      if (m.method === 'Log.entryAdded' && ['error', 'warning'].includes(m.params.entry.level)) {
        const e = m.params.entry;
        // The URL is carried through so the final assertion can name the *expected*
        // 401 instead of having to filter on a message that omits where it came from.
        this.console.push(`${e.source}: ${e.text}${e.url ? ` [${e.url}]` : ''}`);
      }
      if (m.method === 'Network.loadingFailed') {
        this.failed.push(`${m.params.type} ${m.params.errorText}`);
      }
      if (m.method === 'Page.javascriptDialogOpening') {
        this.dialogs.push(m.params.message);
        this.ws.send(JSON.stringify({ id: ++this.id, method: 'Page.handleJavaScriptDialog', params: { accept: true } }));
      }
    };
  }

  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error(`CDP timeout: ${method}`));
        }
      }, 30000);
    });
  }

  async eval(expression) {
    const r = await this.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 300));
    return r.result.value;
  }

  async goto(url) {
    await this.send('Page.navigate', { url });
    for (let i = 0; i < 120; i += 1) {
      await sleep(150);
      try {
        if ((await this.eval('document.readyState')) === 'complete') return;
      } catch {
        /* still navigating */
      }
    }
    throw new Error(`page never loaded: ${url}`);
  }

  /** Real input events: a synthetic .click() produces no user activation. */
  async realClick(selectorExpr) {
    const box = await this.eval(`(() => {
      const el = ${selectorExpr};
      if (!el) return null;
      el.scrollIntoView({ block: 'center' });
      const r = el.getBoundingClientRect();
      return r.width || r.height ? { x: r.left + r.width / 2, y: r.top + r.height / 2 } : null;
    })()`);
    if (!box) throw new Error(`nothing to click: ${selectorExpr}`);
    for (const type of ['mousePressed', 'mouseReleased']) {
      await this.send('Input.dispatchMouseEvent', { type, x: box.x, y: box.y, button: 'left', clickCount: 1 });
    }
  }

  async shot(name) {
    try {
      const { data } = await this.send('Page.captureScreenshot', { format: 'png' });
      fs.writeFileSync(path.join(OUT, name), Buffer.from(data, 'base64'));
    } catch {
      // A screenshot is evidence, not an assertion: a slow frame must not fail a run.
    }
  }
}

async function newPage(port) {
  const t = await getJSON(`http://127.0.0.1:${port}/json/new?about:blank`, 'PUT');
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = rej;
  });
  const s = new Session(ws);
  await s.send('Page.enable');
  await s.send('Runtime.enable');
  await s.send('Log.enable').catch(() => {});
  await s.send('Network.enable').catch(() => {});
  return s;
}

async function waitFor(session, expr, timeoutMs = 12000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      if (await session.eval(expr)) return true;
    } catch {
      /* page busy */
    }
    await sleep(200);
  }
  return false;
}

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const chromePath = findChrome();
  if (!chromePath) {
    console.error('no Chromium found; set CHROME=/path/to/chrome or install Playwright browsers');
    process.exit(2);
  }
  console.log(`chromium: ${chromePath}`);

  const port = await new Promise((res) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => {
      const p = s.address().port;
      s.close(() => res(p));
    });
  });

  const chrome = spawn(
    chromePath,
    ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--disable-crash-reporter',
     `--remote-debugging-port=${port}`, `--user-data-dir=${path.join(OUT, `teleport-${port}`)}`, 'about:blank'],
    { stdio: 'ignore', detached: true },
  );

  let session;
  try {
    for (let i = 0; i < 60; i += 1) {
      try {
        await getJSON(`http://127.0.0.1:${port}/json/version`);
        break;
      } catch {
        await sleep(250);
      }
    }
    session = await newPage(port);

    // ---- the SPA actually boots --------------------------------------------
    await session.goto(`${BASE}${PREFIX}/dashboard/reports`);
    check('the SPA shell is served at the prefix root',
      await session.eval('!!document.querySelector("#app")'));
    const booted = await waitFor(session, '!!document.querySelector("#app").children.length');
    check('the Vue bundle executes (the app mounts children)', booted,
      `${session.console.slice(-3).join(' | ')}`);

    // The router guard must send an anonymous visitor to the login form. This is the
    // client guard, which is UX only — the real check is that the API refuses, and
    // the contract script asserts that.
    const onLogin = await waitFor(session, '!!document.querySelector("#password")');
    check('an anonymous visit lands on the login form', onLogin,
      `url=${await session.eval('location.pathname')}`);
    check('the login form has a submit button and a password field',
      await session.eval('!!document.querySelector("#password") && !!document.querySelector("button[type=submit]")'));
    await session.shot('teleport-01-login.png');

    // ---- logging in really creates a session --------------------------------
    await session.realClick('document.querySelector("#password")');
    await session.send('Input.insertText', { text: PASSWORD });
    await session.realClick('document.querySelector("button[type=submit]")');

    const listed = await waitFor(session, '!!document.querySelector("table.data-table")');
    check('a real login lands on the reports list', listed,
      `path=${await session.eval('location.pathname')} err=${await session.eval('(document.querySelector(".alert-error")||{}).textContent||""')}`);

    // The session cookie is scoped to the route prefix — config defaults CookiePath to
    // RoutePrefix so it is never sent to the unrelated service that shares the public
    // host. It must therefore be looked up with a URL *under* the prefix: asking for
    // the origin root returns nothing, which is indistinguishable from "the cookie was
    // never set".
    const cookies = await session.send('Network.getCookies', { urls: [`${BASE}${PREFIX}/`] });
    const names = (cookies.cookies || []).map((c) => c.name);
    check('the session cookie is set under its frozen name', names.includes('teleport_session'),
      `cookies=${names.join(',') || '(none)'}`);

    // ---- the data the API wrote is really rendered --------------------------
    const rows = await session.eval('document.querySelectorAll("table.data-table tbody tr").length');
    check('the report list is not empty (API + DB + SPA agree)', rows > 0, `rows=${rows}`);
    const bodyText = await session.eval('document.body.innerText');
    check('a report created over the API appears in the dashboard', /p4-/.test(bodyText));
    await session.shot('teleport-02-reports.png');

    // ---- the share page, and Mermaid under the real CSP --------------------
    if (SHARE) {
      await session.goto(`${BASE}${PREFIX}/s/${SHARE}`);
      check('the share page is server-rendered (not the SPA shell)',
        !(await session.eval('!!document.querySelector("#app")')));
      check('the share page shows the report content',
        await session.eval('!!document.querySelector("#report-root")'));

      // The signal is the rendered <svg> inside a .mermaid element, not Mermaid's
      // `data-processed` attribute: v12 replaces the element's contents with the SVG
      // and does not reliably set that attribute (the stylesheet's
      // `.mermaid[data-processed] svg` rule simply never matches on this version).
      // Asserting the attribute version made a working diagram look like a failure.
      const mermaidDone = await waitFor(session, '!!document.querySelector(".mermaid svg")', 20000);
      check('Mermaid rendered a diagram to SVG in a real browser (so the CSP allows its script)', mermaidDone,
        await session.eval('(document.querySelector(".mermaid")||{}).outerHTML?.slice(0,140) || "(no .mermaid element)"'));
      check('exactly one diagram was rendered',
        (await session.eval('document.querySelectorAll(".mermaid svg").length')) === 1,
        `svg count=${await session.eval('document.querySelectorAll(".mermaid svg").length')}`);
      // The page's own error channels, so a swallowed render failure cannot look like
      // a success just because some SVG exists.
      check('the renderer reported no Mermaid errors',
        await session.eval('!document.documentElement.dataset.mermaidErrors && !document.documentElement.dataset.mermaidLoadError'),
        await session.eval('JSON.stringify({errors:document.documentElement.dataset.mermaidErrors||null,load:document.documentElement.dataset.mermaidLoadError||null})'));
      await session.shot('teleport-03-share.png');
    } else {
      console.log('SKIP  share-page checks (no share token supplied)');
    }

    // Any console error is a real defect — with one deliberate exception. The SPA
    // probes GET /api/admin/session when it boots, and before login that is a 401 by
    // design: it is how the client asks "am I already authenticated?". Chrome logs it
    // as a failed resource. The filter is narrow (that path AND a 401), so a genuine
    // auth failure anywhere else still fails this check.
    const expectedUnauthenticated = (c) => /\/api\/admin\/session/.test(c) && /\b401\b/.test(c);
    const errors = session.console.filter((c) => !/favicon/i.test(c) && !expectedUnauthenticated(c));
    check('no unexpected console errors or warnings during the whole run', errors.length === 0,
      errors.slice(0, 4).join(' | '));
    const failed = session.failed.filter((f) => !/favicon/i.test(f));
    check('no failed network requests', failed.length === 0, failed.slice(0, 4).join(' | '));
  } finally {
    try {
      if (session) session.ws.close();
    } catch { /* ignore */ }
    try {
      process.kill(-chrome.pid, 'SIGKILL');
    } catch {
      try { chrome.kill('SIGKILL'); } catch { /* ignore */ }
    }
  }

  const failedChecks = results.filter((r) => !r.ok);
  console.log(`\n----- browser: ${results.length - failedChecks.length} passed, ${failedChecks.length} failed -----`);
  process.exit(failedChecks.length === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error(`browser verification crashed: ${err && err.stack ? err.stack : err}`);
  process.exit(1);
});
