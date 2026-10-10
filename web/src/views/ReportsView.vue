<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { ApiError, api, type ReportSummary } from '../api';
import { relativeTime } from '../format';

const reports = ref<ReportSummary[]>([]);
const loading = ref(true);
const error = ref('');
const search = ref('');

async function load() {
  loading.value = true;
  error.value = '';
  try {
    reports.value = await api.listReports({ limit: 200 });
  } catch (err) {
    // 401 is handled globally by the session watcher in App.vue.
    if (!(err instanceof ApiError && err.isAuthError)) {
      error.value = err instanceof Error ? err.message : '加载失败';
    }
  } finally {
    loading.value = false;
  }
}

onMounted(load);

/** Client-side filter over the already-fetched page. */
const filtered = computed(() => {
  const q = search.value.trim().toLowerCase();
  if (!q) return reports.value;
  return reports.value.filter(
    (r) =>
      r.title.toLowerCase().includes(q) ||
      r.category.toLowerCase().includes(q) ||
      r.id.toLowerCase().includes(q),
  );
});

const categories = computed(() =>
  [...new Set(reports.value.map((r) => r.category))].sort(),
);
</script>

<template>
  <div class="page-head">
    <div>
      <h1>报告</h1>
      <p v-if="!loading">共 {{ reports.length }} 篇<span v-if="categories.length"> · {{ categories.join(' / ') }}</span></p>
      <p v-else>加载中…</p>
    </div>
    <div class="row">
      <input
        v-model="search"
        type="search"
        placeholder="搜索标题 / 分类 / ID"
        style="width: 240px"
        :disabled="loading"
      />
      <button type="button" :disabled="loading" @click="load">刷新</button>
    </div>
  </div>

  <div v-if="error" class="alert error" style="margin-bottom: 16px">{{ error }}</div>

  <div class="card" style="padding: 0">
    <div v-if="loading" class="empty"><span class="spin"></span> 正在加载报告…</div>

    <div v-else-if="filtered.length === 0" class="empty">
      <p v-if="reports.length === 0">
        还没有报告。通过 <code class="mono">POST /api/reports</code> 上报第一篇。
      </p>
      <p v-else>没有匹配「{{ search }}」的报告。</p>
    </div>

    <table v-else class="table">
      <thead>
        <tr>
          <th>标题</th>
          <th>分类</th>
          <th>发布者</th>
          <th>格式</th>
          <th>创建时间</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in filtered" :key="r.id">
          <td>
            <RouterLink :to="`/dashboard/reports/${r.id}`">{{ r.title }}</RouterLink>
            <div class="muted mono" style="font-size: 11px">{{ r.id }}</div>
          </td>
          <td><span class="badge">{{ r.category }}</span></td>
          <td class="muted">
            {{ r.owner_name || '未知' }}
            <div v-if="r.owner_key_id" class="muted mono" style="font-size: 11px">
              {{ r.owner_key_id.slice(0, 8) }}
            </div>
          </td>
          <td class="muted">{{ r.format }}</td>
          <td class="muted" :title="new Date(r.created_at).toISOString()">
            {{ relativeTime(r.created_at) }}
            <!--
              updated_at now moves, so the list has to show it: a report that
              says "3 days ago" while having been corrected an hour ago would
              mislead exactly the person checking whether the fix landed.
            -->
            <div
              v-if="r.updated_at > r.created_at"
              class="muted"
              style="font-size: 11px"
              :title="`更新于 ${new Date(r.updated_at).toISOString()}`"
            >
              已更新 · {{ relativeTime(r.updated_at) }}
            </div>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
