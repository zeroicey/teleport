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
  <div class="login-wrap">
    <form class="card login-card stack" @submit.prevent="submit">
      <div>
        <h1>Teleport</h1>
        <p class="muted" style="margin: 0">登录以管理报告与分享链接</p>
      </div>

      <div v-if="error" class="alert error">{{ error }}</div>

      <div>
        <label for="password">管理密码</label>
        <input
          id="password"
          v-model="password"
          type="password"
          autocomplete="current-password"
          autofocus
          :disabled="loading"
        />
      </div>

      <button class="primary" type="submit" :disabled="loading">
        <span v-if="loading" class="spin" style="margin-right: 8px"></span>
        {{ loading ? '登录中…' : '登录' }}
      </button>

      <p class="muted" style="font-size: 12px; margin: 0">
        会话使用签名的 HttpOnly Cookie 保存。也可通过 Cloudflare Access 前置保护。
      </p>
    </form>
  </div>
</template>
