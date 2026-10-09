import { createRouter, createWebHistory } from 'vue-router';
import { api } from '../api';

/**
 * The dashboard lives under the /dashboard URL prefix.
 *
 * `createWebHistory` is given Vite's `BASE_URL` — the application's route
 * prefix (`/yeciorez/teleport/`) — rather than `/`. This deployment does not sit
 * at an origin root: the host is shared with other services and Caddy routes
 * only the prefix to this app, so with `/` the router would try to resolve
 * `/yeciorez/teleport/dashboard` as a route path and fail, and every
 * `router.push` would navigate outside the app.
 *
 * Route paths stay relative to that base, so `/dashboard` is the URL
 * `<prefix>/dashboard` in the address bar. The Go backend has no server-side
 * routing beyond the prefix: it serves index.html for any unmatched path under
 * it, which is what makes deep links survive a hard refresh.
 *
 * The auth guard is a UX convenience only — real enforcement is server-side in
 * `requireSession`. Never treat a client-side guard as security.
 */
export const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/', redirect: '/dashboard' },
    {
      path: '/dashboard',
      name: 'login',
      component: () => import('../views/LoginView.vue'),
      meta: { public: true },
    },
    {
      path: '/dashboard/reports',
      name: 'reports',
      component: () => import('../views/ReportsView.vue'),
    },
    {
      path: '/dashboard/reports/:id',
      name: 'report-detail',
      component: () => import('../views/ReportDetailView.vue'),
      props: true,
    },
    { path: '/:pathMatch(.*)*', redirect: '/dashboard' },
  ],
});

router.beforeEach(async (to) => {
  if (to.meta.public) return true;

  try {
    await api.checkSession();
    return true;
  } catch {
    // Not logged in (or the session expired): send them to the login screen.
    return { name: 'login', query: { redirect: to.fullPath } };
  }
});
