/// <reference types="vite/client" />
/**
 * Typed access to the frontend's Vite environment variables.
 *
 * `vite/client` already declares a loose `ImportMetaEnv`, so this file only
 * *narrows* it with the vars this app actually reads. Declaring them here
 * (rather than sprinkling casts over `import.meta.env`) keeps `vue-tsc` honest:
 * a typo in a variable name becomes a compile error instead of `undefined`.
 *
 * Both overrides are OPTIONAL by design. The normal deployment is same-origin
 * and leaves them unset, so requests are relative to `BASE_URL` (the route
 * prefix) and `shareUrl()` derives its answer from `window.location`.
 */
interface ImportMetaEnv {
  /**
   * Optional API origin override, e.g. `http://127.0.0.1:8788`.
   *
   * Unset → same-origin requests under `BASE_URL`, which is the production
   * behaviour. Note this is an ORIGIN, not a full path: the route prefix is
   * always applied from `BASE_URL` so the two cannot disagree.
   * Trailing slashes are normalised away in `api.ts`.
   */
  readonly VITE_API_BASE?: string;
  /**
   * Optional public share base, e.g. `https://teleport.zeroicey.me`. Should
   * include any route prefix, since it replaces the whole app base.
   * Unset → current origin plus `BASE_URL`.
   */
  readonly VITE_SHARE_BASE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
