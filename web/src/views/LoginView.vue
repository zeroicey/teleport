<script setup lang="ts">
import { ref } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { ApiError, api } from '../api';

const route = useRoute();
const router = useRouter();

const password = ref('');
const error = ref('');
const loading = ref(false);

async function submit() {
  if (!password.value) {
    error.value = '请输入密码';
    return;
  }

  loading.value = true;
  error.value = '';

  try {
    await api.login(password.value);
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/dashboard/reports';
    await router.push(redirect);
  } catch (err) {
    error.value =
      err instanceof ApiError && err.status === 401
        ? '密码错误'
        : err instanceof Error
          ? err.message
          : '登录失败';
    password.value = '';
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <div class="login-shell">
    <form class="card login-card" @submit.prevent="submit">
      <div>
        <h1 class="m-0 text-3xl font-semibold tracking-tight">Teleport</h1>
        <p class="mt-1 mb-0 text-sm text-ink-muted">登录以管理报告与分享链接</p>
      </div>

      <!--
        `role="alert"` rather than `aria-live`: a failed sign-in is exactly the
        kind of update that must interrupt, and the region only exists in the
        DOM while there is something to say, so it is never a standing live
        region announcing nothing.
      -->
      <div v-if="error" class="alert alert-error" role="alert">{{ error }}</div>

      <div>
        <label class="field-label" for="password">管理密码</label>
        <input
          id="password"
          v-model="password"
          class="input"
          type="password"
          name="password"
          autocomplete="current-password"
          :aria-invalid="error ? 'true' : undefined"
          :disabled="loading"
          autofocus
        />
      </div>

      <button class="btn btn-primary w-full py-2.5" type="submit" :disabled="loading">
        <span v-if="loading" class="spinner" aria-hidden="true"></span>
        <span v-if="loading">登录中…</span>
        <span v-else>登录</span>
      </button>

      <p class="m-0 text-xs leading-relaxed text-ink-muted">
        会话使用签名的 HttpOnly Cookie 保存。也可通过 Cloudflare Access 前置保护。
      </p>
    </form>
  </div>
</template>
