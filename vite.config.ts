import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

/**
 * Route prefix: the single source of truth for where the app is mounted.
 *
 * This deployment does NOT serve the app at an origin root. The public host
 * (`api.hcyj.xyz`) is shared with other services and is fronted by Caddy, which
 * routes everything under `/yeciorez/teleport` to the Go process; the Go
 * process in turn serves the SPA, the API and the share pages beneath that same
 * prefix. Setting Vite's `base` to the prefix makes every generated URL —
 * the hashed bundle references in index.html, the runtime asset imports, and
 * `import.meta.env.BASE_URL` — agree with that layout automatically, so the
 * prefix is written down exactly once.
 *
 * The route prefix is overridable with `VITE_ROUTE_PREFIX` so the same config
 * can produce a build for a different mount point without editing code.
 */
const ROUTE_PREFIX = (process.env.VITE_ROUTE_PREFIX ?? '/yeciorez/teleport').replace(/\/+$/, '');
const BASE = `${ROUTE_PREFIX}/`;

/** Two entry points, built straight into the Go embed directory:
 *
 *   1. `web/index.html`       — the private Vue dashboard (SPA).
 *   2. `web/src/share/main.ts` — the public share-page bootstrap. Kept as a
 *      *stable* filename (`assets/share.js`) because the Go backend hardcodes
 *      that URL when it server-renders the share page. The backend sends
 *      `Cache-Control: no-cache` for it, so browsers always revalidate and
 *      never run stale JS after a deploy.
 *
 * The output path IS the `//go:embed` target, so there is no copy step to get
 * wrong and no window where a stale build sits in one directory while the
 * binary embeds another.
 *
 * Mermaid is not imported statically anywhere; the share bootstrap uses a
 * dynamic `import('mermaid')`, so Vite emits it as lazy chunks that are only
 * fetched when a report actually contains a diagram.
 *
 * Development
 *   `pnpm run dev:web` -> Vite on :5173<prefix>/ with HMR, proxying the API and
 *   share pages to the local Go backend.
 * Production
 *   `pnpm run build`      -> emits here, then
 *   `pnpm run backend:build` -> compiles both into one self-contained binary.
 */
const SHARE_ENTRY_NAME = 'share';
const SHARE_ENTRY_FILE = 'assets/share.js';

/**
 * Local Go backend.
 *
 * The Go process mounts every route under the prefix, and there is no rewrite
 * in the proxy below: the URL the browser requests is byte-for-byte the URL the
 * backend sees, in development and in production alike.
 */
const DEV_API_TARGET = 'http://127.0.0.1:8788';

export default defineConfig({
  root: 'web',
  base: BASE,
  plugins: [vue()],

  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./web/src', import.meta.url)),
    },
  },

  build: {
    // Directly into the //go:embed source directory (see
    // backend/internal/webui/webui_embed.go). Keeping these in sync by
    // construction beats a copy step that can silently lag.
    outDir: fileURLToPath(new URL('./backend/internal/webui/dist', import.meta.url)),
    emptyOutDir: true,
    sourcemap: false,
    assetsDir: 'assets',
    chunkSizeWarningLimit: 600,
    rollupOptions: {
      input: {
        index: fileURLToPath(new URL('./web/index.html', import.meta.url)),
        [SHARE_ENTRY_NAME]: fileURLToPath(new URL('./web/src/share/main.ts', import.meta.url)),
      },
      output: {
        // The dashboard leaks nothing by being content-hashed; the share entry
        // must keep a stable path so the Worker can reference it.
        entryFileNames: (chunk) =>
          chunk.name === SHARE_ENTRY_NAME ? SHARE_ENTRY_FILE : 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },

  server: {
    // Bind IPv4 explicitly: Vite's default host resolves to ::1 only on some
    // systems, which makes http://127.0.0.1:5173 (the documented URL, and what
    // the proxy below targets) unreachable.
    host: '127.0.0.1',
    port: 5173,
    strictPort: false,
    // Same-origin API calls in dev, so cookies and CORS behave exactly as in
    // production (no cross-origin cookie/timing surprises).
    //
    // No `rewrite`: the Go process and the dev server both mount everything
    // under the prefix, so the browser's URL is passed through unchanged. The
    // dev server handles the SPA shell and static assets from `base`; only the
    // API and the server-rendered share pages are forwarded.
    proxy: {
      [`${ROUTE_PREFIX}/api`]: {
        target: DEV_API_TARGET,
        changeOrigin: false,
      },
      [`${ROUTE_PREFIX}/s`]: {
        target: DEV_API_TARGET,
        changeOrigin: false,
      },
    },
  },
});
