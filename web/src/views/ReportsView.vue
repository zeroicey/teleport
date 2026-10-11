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
      <p v-if="!loading">
        共 {{ reports.length }} 篇<span v-if="categories.length"> · {{ categories.join(' / ') }}</span>
      </p>
      <p v-else>加载中…</p>
    </div>

    <div class="flex flex-wrap items-center gap-2">
      <!--
        The placeholder is not a label: it disappears the moment the field has
        content, which is precisely when a screen-reader user needs to know what
        the field is. `sr-only` gives it a real one without adding visual noise.
      -->
      <label for="report-search" class="sr-only">搜索报告</label>
      <input
        id="report-search"
        v-model="search"
        class="input w-60"
        type="search"
        name="report-search"
        placeholder="搜索标题 / 分类 / ID"
        :disabled="loading"
        autocomplete="off"
        enterkeyhint="search"
        spellcheck="false"
      />
      <button class="btn" type="button" :disabled="loading" @click="load">刷新</button>
    </div>
  </div>

  <div v-if="error" class="alert alert-error mb-4" role="alert">{{ error }}</div>

  <div class="card overflow-hidden p-0">
    <div v-if="loading" class="px-5 py-12 text-center text-ink-muted" role="status">
      <span class="spinner mr-2" aria-hidden="true"></span> 正在加载报告…
    </div>

    <div v-else-if="filtered.length === 0" class="px-5 py-12 text-center text-ink-muted">
      <p v-if="reports.length === 0" class="m-0">
        还没有报告。通过
        <code class="rounded bg-surface-sunken px-1.5 py-0.5 font-mono text-[13px]"
          >POST /api/reports</code
        >
        上报第一篇。
      </p>
      <p v-else class="m-0">没有匹配「{{ search }}」的报告。</p>
    </div>

    <!--
      `overflow-x-auto` on the wrapper: the table has five columns and a long
      title, and without it the whole page scrolls sideways on a phone instead
      of just the table.
    -->
    <div v-else class="overflow-x-auto">
      <table class="data-table">
        <caption class="sr-only">
          报告列表
        </caption>
        <thead>
          <tr>
            <th scope="col">标题</th>
            <th scope="col">分类</th>
            <th scope="col">发布者</th>
            <th scope="col">格式</th>
            <th scope="col">创建时间</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in filtered" :key="r.id">
            <td>
              <RouterLink :to="`/dashboard/reports/${r.id}`">{{ r.title }}</RouterLink>
              <div class="mt-0.5 font-mono text-[11px] text-ink-muted">{{ r.id }}</div>
            </td>
            <td><span class="badge">{{ r.category }}</span></td>
            <td class="text-ink-muted">
              {{ r.owner_name || '未知' }}
              <div v-if="r.owner_key_id" class="font-mono text-[11px] text-ink-muted">
                {{ r.owner_key_id.slice(0, 8) }}
              </div>
            </td>
            <td class="text-ink-muted">{{ r.format }}</td>
            <td class="text-ink-muted" :title="new Date(r.created_at).toISOString()">
              {{ relativeTime(r.created_at) }}
              <!--
                updated_at now moves, so the list has to show it: a report that
                says "3 days ago" while having been corrected an hour ago would
                mislead exactly the person checking whether the fix landed.
              -->
              <div
                v-if="r.updated_at > r.created_at"
                class="text-[11px]"
                :title="`更新于 ${new Date(r.updated_at).toISOString()}`"
              >
                已更新 · {{ relativeTime(r.updated_at) }}
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
