<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { useRouter } from 'vue-router';
import { ApiError, api, type ReportDetail, type ShareToken } from '../api';
import {
  copyText,
  expiryState,
  formatDateTime,
  formatExpiry,
  shareUrl,
} from '../format';

const props = defineProps<{ id: string }>();
const router = useRouter();

const report = ref<ReportDetail | null>(null);
const loading = ref(true);
const error = ref('');

const newShareHours = ref<number>(24);
const busyToken = ref<string | null>(null);
const notice = ref('');
const copiedToken = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = '';
  try {
    report.value = await api.getReport(props.id);
  } catch (err) {
    if (err instanceof ApiError && err.isAuthError) return;
    error.value =
      err instanceof ApiError && err.status === 404
        ? '报告不存在'
        : err instanceof Error
          ? err.message
          : '加载失败';
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function flash(message: string) {
  notice.value = message;
  window.setTimeout(() => {
    if (notice.value === message) notice.value = '';
  }, 2500);
}

async function createShare() {
  if (!report.value) return;
  try {
    await api.createShare(report.value.id, Number(newShareHours.value));
    await load();
    flash('已生成新的分享链接');
  } catch (err) {
    error.value = err instanceof Error ? err.message : '生成失败';
  }
}

async function toggleActive(token: ShareToken) {
  busyToken.value = token.token;
  error.value = '';
  try {
    await api.updateShare(token.token, { isActive: !token.is_active });
    await load();
    flash(token.is_active ? '链接已禁用' : '链接已恢复');
  } catch (err) {
    error.value = err instanceof Error ? err.message : '操作失败';
  } finally {
    busyToken.value = null;
  }
}

async function extend(token: ShareToken, hours: number) {
  busyToken.value = token.token;
  error.value = '';
  try {
    await api.updateShare(token.token, { expiresInHours: hours });
    await load();
    flash(hours === 0 ? '已设为永不过期' : `有效期已设为 ${hours} 小时`);
  } catch (err) {
    error.value = err instanceof Error ? err.message : '操作失败';
  } finally {
    busyToken.value = null;
  }
}

async function revoke(token: ShareToken) {
  if (!window.confirm('确定禁用此分享链接？该操作不可撤销（但可稍后恢复启用）。')) return;
  busyToken.value = token.token;
  error.value = '';
  try {
    await api.revokeShare(token.token);
    await load();
    flash('链接已禁用');
  } catch (err) {
    error.value = err instanceof Error ? err.message : '操作失败';
  } finally {
    busyToken.value = null;
  }
}

async function copy(token: string) {
  const url = shareUrl(token);
  if (await copyText(url)) {
    copiedToken.value = token;
    window.setTimeout(() => {
      if (copiedToken.value === token) copiedToken.value = null;
    }, 1800);
  } else {
    error.value = '复制失败，请手动选择链接';
  }
}

const activeCount = computed(
  () => report.value?.share_tokens.filter((t) => expiryState(t) === 'active').length ?? 0,
);

// -- edit ---------------------------------------------------------------------
//
// The form is seeded from the loaded report and then owned by the user. It is
// NOT re-seeded on every load(), so a background refresh cannot silently discard
// half-typed edits.
const editing = ref(false);
const saving = ref(false);
const form = ref({ title: '', category: '', content: '' });

function startEdit() {
  if (!report.value) return;
  form.value = {
    title: report.value.title,
    category: report.value.category,
    content: report.value.content,
  };
  editing.value = true;
  error.value = '';
}

/**
 * Only the fields that actually changed are sent.
 *
 * Two reasons, both load-bearing: a PATCH carrying a field the server rejects is
 * a 400, and sending an unchanged `content` of a large report would re-upload
 * megabytes for nothing. The server's own rule is that an empty patch is a 400 —
 * which is correct, since it would otherwise bump updated_at and make the share
 * page advertise an edit nobody made.
 */
async function saveEdit() {
  if (!report.value) return;
  const current = report.value;
  const patch: { title?: string; category?: string; content?: string } = {};
  if (form.value.title !== current.title) patch.title = form.value.title;
  if (form.value.category !== current.category) patch.category = form.value.category;
  if (form.value.content !== current.content) patch.content = form.value.content;

  if (Object.keys(patch).length === 0) {
    editing.value = false;
    flash('没有改动');
    return;
  }

  saving.value = true;
  error.value = '';
  try {
    report.value = await api.updateReport(current.id, patch);
    editing.value = false;
    flash('已保存。已有分享链接立即展示新内容。');
  } catch (err) {
    error.value = err instanceof Error ? err.message : '保存失败';
  } finally {
    saving.value = false;
  }
}

// -- delete -------------------------------------------------------------------

const deleting = ref(false);

/**
 * Deleting takes the share links with it, so the confirmation has to name that
 * consequence rather than ask a generic "are you sure?". The count comes from
 * the loaded report, which already lists the links.
 */
async function removeReport() {
  if (!report.value) return;
  const total = report.value.share_tokens.length;
  const live = activeCount.value;
  const warning =
    total === 0
      ? `删除报告「${report.value.title}」？此操作不可撤销。`
      : `删除报告「${report.value.title}」？\n\n` +
        `它名下的 ${total} 条分享链接（其中 ${live} 条当前有效）会一并删除，` +
        `已经拿到链接的人会立刻看到 404。\n\n此操作不可撤销，也没有回收站。`;
  if (!window.confirm(warning)) return;

  deleting.value = true;
  error.value = '';
  try {
    await api.deleteReport(report.value.id);
    await router.push('/dashboard/reports');
  } catch (err) {
    error.value = err instanceof Error ? err.message : '删除失败';
    deleting.value = false;
  }
}

/** True once the report has been edited after publication. */
const wasEdited = computed(
  () => !!report.value && report.value.updated_at > report.value.created_at,
);
</script>

<template>
  <div v-if="loading" class="empty"><span class="spin"></span> 正在加载…</div>

  <div v-else-if="!report" class="stack">
    <div class="alert error">{{ error || '报告不存在' }}</div>
    <button type="button" @click="router.push('/dashboard/reports')">返回列表</button>
  </div>

  <template v-else>
    <div class="page-head">
      <div>
        <RouterLink to="/dashboard/reports" class="muted" style="font-size: 13px; text-decoration: none">← 返回列表</RouterLink>
        <h1 style="margin-top: 8px">{{ report.title }}</h1>
        <p>
          <span class="badge">{{ report.category }}</span>
          <span class="badge" style="margin-left: 6px">{{ report.format }}</span>
          <span class="muted" style="margin-left: 10px">
            {{ activeCount }} / {{ report.share_tokens.length }} 条链接有效
          </span>
          <span v-if="wasEdited" class="muted" style="margin-left: 10px" :title="formatDateTime(report.updated_at)">
            已于 {{ formatDateTime(report.updated_at) }} 更新
          </span>
        </p>
      </div>
      <div class="row" style="gap: 8px">
        <button type="button" @click="editing ? (editing = false) : startEdit()">
          {{ editing ? '取消编辑' : '编辑内容' }}
        </button>
        <button class="danger" type="button" :disabled="deleting" @click="removeReport">
          {{ deleting ? '正在删除…' : '删除报告' }}
        </button>
      </div>
    </div>

    <!-- ---- edit ---- -->
    <section v-if="editing" class="card stack" style="margin-bottom: 20px">
      <h2 style="margin: 0; font-size: 1.05rem">编辑报告</h2>
      <p class="muted" style="margin: 0; font-size: 13px">
        保存后<strong>立即生效</strong>：报告 ID 不变，因此
        <strong>已经发出去的分享链接会直接展示新内容</strong>，不会失效也不会换成新链接。
        如果你需要「发出即冻结」的那一版，请改为重新发布一篇。
      </p>

      <label for="edit-title">标题</label>
      <input id="edit-title" v-model="form.title" type="text" maxlength="300" />

      <label for="edit-category">分类</label>
      <input
        id="edit-category"
        v-model="form.category"
        type="text"
        placeholder="general / pentest / architecture / progress"
      />
      <span class="muted" style="font-size: 12px">
        小写字母、数字、下划线与连字符，最长 32 字符
      </span>

      <label for="edit-content">正文（Markdown）</label>
      <textarea id="edit-content" v-model="form.content" rows="18" class="mono"></textarea>

      <div class="row" style="justify-content: flex-end; gap: 8px">
        <button type="button" :disabled="saving" @click="editing = false">取消</button>
        <button class="primary" type="button" :disabled="saving" @click="saveEdit">
          {{ saving ? '保存中…' : '保存' }}
        </button>
      </div>
    </section>

    <div v-if="error" class="alert error" style="margin-bottom: 16px">{{ error }}</div>
    <div v-if="notice" class="alert ok" style="margin-bottom: 16px">{{ notice }}</div>

    <!-- ---- share tokens ---- -->
    <section class="card stack" style="margin-bottom: 20px">
      <div class="row">
        <h2 style="margin: 0; font-size: 1.05rem">分享链接</h2>
        <div class="spacer"></div>
        <div class="row" style="gap: 8px">
          <select v-model.number="newShareHours" style="width: auto" aria-label="有效期">
            <option :value="1">1 小时</option>
            <option :value="6">6 小时</option>
            <option :value="24">24 小时</option>
            <option :value="168">7 天</option>
            <option :value="720">30 天</option>
            <option :value="0">永不过期</option>
          </select>
          <button class="primary" type="button" @click="createShare">生成新链接</button>
        </div>
      </div>

      <p v-if="report.share_tokens.length === 0" class="muted" style="margin: 0">
        暂无分享链接，生成一条以便分享。
      </p>

      <table v-else class="table">
        <thead>
          <tr>
            <th>链接</th>
            <th>状态</th>
            <th>有效期</th>
            <th>浏览</th>
            <th style="text-align: right">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="t in report.share_tokens" :key="t.token">
            <td style="min-width: 260px">
              <div class="mono token-url">{{ shareUrl(t.token) }}</div>
              <button class="ghost" type="button" style="margin-top: 4px" @click="copy(t.token)">
                {{ copiedToken === t.token ? '✓ 已复制' : '复制链接' }}
              </button>
            </td>
            <td>
              <span
                class="badge"
                :class="{
                  ok: expiryState(t) === 'active',
                  warn: expiryState(t) === 'expired',
                  danger: expiryState(t) === 'revoked',
                }"
              >
                {{ expiryState(t) === 'active' ? '有效' : expiryState(t) === 'expired' ? '已过期' : '已禁用' }}
              </span>
            </td>
            <td class="muted" style="font-size: 13px">{{ formatExpiry(t.expires_at) }}</td>
            <td class="muted">{{ t.view_count }}</td>
            <td>
              <div class="row" style="justify-content: flex-end; gap: 4px">
                <select
                  style="width: auto; font-size: 13px; padding: 4px 8px"
                  :disabled="busyToken === t.token"
                  aria-label="调整有效期"
                  @change="
                    (e) => {
                      const v = (e.target as HTMLSelectElement).value;
                      if (v !== '') {
                        extend(t, Number(v));
                        (e.target as HTMLSelectElement).value = '';
                      }
                    }
                  "
                >
                  <option value="">调整…</option>
                  <option value="1">1 小时</option>
                  <option value="24">24 小时</option>
                  <option value="168">7 天</option>
                  <option value="0">永不过期</option>
                </select>
                <button
                  class="ghost"
                  type="button"
                  :disabled="busyToken === t.token"
                  @click="toggleActive(t)"
                >
                  {{ t.is_active ? '禁用' : '恢复' }}
                </button>
                <button
                  class="ghost danger"
                  type="button"
                  :disabled="busyToken === t.token || !t.is_active"
                  @click="revoke(t)"
                >
                  吊销
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </section>

    <!-- ---- metadata ---- -->
    <section class="card stack" style="margin-bottom: 20px">
      <h2 style="margin: 0; font-size: 1.05rem">元数据</h2>
      <dl class="meta-grid">
        <div><dt>报告 ID</dt><dd class="mono">{{ report.id }}</dd></div>
        <div><dt>创建时间</dt><dd>{{ formatDateTime(report.created_at) }}</dd></div>
        <div><dt>更新时间</dt><dd>{{ formatDateTime(report.updated_at) }}</dd></div>
        <div><dt>分类</dt><dd>{{ report.category }}</dd></div>
      </dl>
      <div v-if="Object.keys(report.metadata).length">
        <dt class="muted" style="font-size: 13px; margin-bottom: 6px">metadata</dt>
        <pre class="content" style="max-height: 200px">{{ JSON.stringify(report.metadata, null, 2) }}</pre>
      </div>
      <p v-else class="muted" style="margin: 0; font-size: 13px">无附加元数据</p>
    </section>

    <!-- ---- content ---- -->
    <section class="card stack">
      <h2 style="margin: 0; font-size: 1.05rem">正文</h2>
      <p class="muted" style="margin: 0; font-size: 12px">
        此处显示原始源文。分享页面由 Worker 服务端渲染，Markdown 与 Mermaid 将在此渲染管线中处理。
      </p>
      <pre class="content">{{ report.content }}</pre>
    </section>
  </template>
</template>
