import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

/**
 * Two entry points, built into `public/` for the Worker's `[assets]` binding:
 *
 *   1. `web/index.html`      — the private Vue dashboard (SPA).
 *   2. `web/src/share/main.ts` — the public share-page bootstrap. Kept as a
 *      *stable* filename (`assets/share.js`) because the Worker hardcodes its
 *      URL when it server-renders the share page. `public/_headers` marks it
 *      `no-cache` so browsers always revalidate and never run stale JS after a
 *      deploy.
 *
 * Mermaid is not imported statically anywhere; the share bootstrap uses a
 * dynamic `import('mermaid')`, so Vite emits it as lazy chunks that are only
 * fetched when a report actually contains a diagram.
 *
 * Development
 *   `pnpm run dev:web` -> Vite on :5173 with HMR, proxying /api to the Worker.
 * Production
 *   `pnpm run build`   -> emits into public/, then `wrangler deploy`.
 */
const SHARE_ENTRY_NAME = 'share';
const SHARE_ENTRY_FILE = 'assets/share.js';

export default defineConfig({
  root: 'web',
  plugins: [vue()],

  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./web/src', import.meta.url)),
    },
  },

  build: {
    outDir: fileURLToPath(new URL('./public', import.meta.url)),
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
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: false,
      },
    },
  },
});
