import { createRouter, createWebHistory } from 'vue-router';
import { api } from '../api';

/**
 * The dashboard lives under the /dashboard URL prefix.
 *
 * `createWebHistory('/')` (rather than `'/dashboard'`) keeps routing independent
 * of the history base: the assets binding serves this SPA for every unmatched
 * path via `not_found_handling = "single-page-application"`, so the router must
 * resolve correctly whether a user lands on `/`, `/dashboard` or a deep link.
 * The prefix is therefore part of each route path instead of the history base.
 *
 * The auth guard is a UX convenience only — real enforcement is server-side in
 * `requireSession`. Never treat a client-side guard as security.
 */
export const router = createRouter({
  history: createWebHistory('/'),
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
