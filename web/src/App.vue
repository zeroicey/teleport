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
    <header class="topbar">
      <RouterLink to="/dashboard/reports" class="brand">
        Teleport<span>报告控制台</span>
      </RouterLink>
      <div class="row">
        <button class="ghost" type="button" @click="handleLogout">退出登录</button>
      </div>
    </header>
    <main class="container">
      <RouterView />
    </main>
  </div>
</template>
