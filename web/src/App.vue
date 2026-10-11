<script setup lang="ts">
import { watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { api, sessionExpired } from './api';

const route = useRoute();
const router = useRouter();

// When any API call reports 401, drop the user back to the login screen once.
watch(sessionExpired, async (expired) => {
  if (!expired) return;
  sessionExpired.value = false;
  if (route.name !== 'login') {
    await router.push({ name: 'login', query: { redirect: route.fullPath } });
  }
});

async function handleLogout() {
  try {
    await api.logout();
  } catch {
    // Logging out locally is enough even if the request fails.
  }
  await router.push({ name: 'login' });
}
</script>

<template>
  <div v-if="route.name === 'login'">
    <RouterView />
  </div>

  <div v-else>
    <!--
      Skip link. It is off-screen until focused, and it exists because the
      header carries a nav and a sign-out button *before* the content: without
      it, every keyboard user pays two extra tab stops on every page. `sr-only`
      + `focus:not-sr-only` keeps it invisible until it is wanted.
    -->
    <a
      href="#main"
      class="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-3 focus:z-50 focus:rounded-control focus:border focus:border-line focus:bg-surface focus:px-3 focus:py-1.5 focus:text-sm focus:no-underline"
    >
      跳到主要内容
    </a>

    <header class="topbar">
      <div class="flex items-center gap-5">
        <RouterLink to="/dashboard/reports" class="brand">
          Teleport<span class="ml-2 text-[13px] font-normal text-ink-muted">报告控制台</span>
        </RouterLink>
        <nav class="topnav" aria-label="主导航">
          <RouterLink to="/dashboard/reports">报告</RouterLink>
          <RouterLink to="/dashboard/keys">密钥</RouterLink>
        </nav>
      </div>

      <button class="btn btn-ghost" type="button" @click="handleLogout">退出登录</button>
    </header>

    <!-- `tabindex="-1"` so the skip link actually moves focus here rather than
         only scrolling, which is what makes it useful to a screen reader. -->
    <main id="main" tabindex="-1" class="container">
      <RouterView />
    </main>
  </div>
</template>
