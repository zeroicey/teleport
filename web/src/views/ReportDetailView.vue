<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { ApiError, api, type ReportDetail, type ShareToken } from "../api";
import {
  copyText,
  expiryState,
  formatDateTime,
  formatExpiry,
  shareUrl,
} from "../format";

const props = defineProps<{ id: string }>();
const router = useRouter();

const report = ref<ReportDetail | null>(null);
const loading = ref(true);
const error = ref("");

const newShareHours = ref<number>(24);
const busyToken = ref<string | null>(null);
const notice = ref("");
const copiedToken = ref<string | null>(null);

async function load() {
  loading.value = true;
  error.value = "";
  try {
    report.value = await api.getReport(props.id);
  } catch (err) {
    if (err instanceof ApiError && err.isAuthError) return;
    error.value =
      err instanceof ApiError && err.status === 404
        ? "报告不存在"
        : err instanceof Error
          ? err.message
          : "加载失败";
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function flash(message: string) {
  notice.value = message;
  window.setTimeout(() => {
    if (notice.value === message) notice.value = "";
  }, 2500);
}

async function createShare() {
  if (!report.value) return;
  try {
    await api.createShare(report.value.id, Number(newShareHours.value));
    await load();
    flash("已生成新的分享链接");
  } catch (err) {
    error.value = err instanceof Error ? err.message : "生成失败";
  }
}

async function toggleActive(token: ShareToken) {
  busyToken.value = token.token;
  error.value = "";
  try {
    await api.updateShare(token.token, { isActive: !token.is_active });
    await load();
    flash(token.is_active ? "链接已禁用" : "链接已恢复");
  } catch (err) {
    error.value = err instanceof Error ? err.message : "操作失败";
  } finally {
    busyToken.value = null;
  }
}

async function extend(token: ShareToken, hours: number) {
  busyToken.value = token.token;
  error.value = "";
  try {
    await api.updateShare(token.token, { expiresInHours: hours });
    await load();
    flash(hours === 0 ? "已设为永不过期" : `有效期已设为 ${hours} 小时`);
  } catch (err) {
    error.value = err instanceof Error ? err.message : "操作失败";
  } finally {
    busyToken.value = null;
  }
}

async function revoke(token: ShareToken) {
  if (
    !window.confirm("确定禁用此分享链接？该操作不可撤销（但可稍后恢复启用）。")
  )
    return;
  busyToken.value = token.token;
  error.value = "";
  try {
    await api.revokeShare(token.token);
    await load();
    flash("链接已禁用");
  } catch (err) {
    error.value = err instanceof Error ? err.message : "操作失败";
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
    error.value = "复制失败，请手动选择链接";
  }
}

const activeCount = computed(
  () =>
    report.value?.share_tokens.filter((t) => expiryState(t) === "active")
      .length ?? 0,
);

// -- edit ---------------------------------------------------------------------
//
// The form is seeded from the loaded report and then owned by the user. It is
// NOT re-seeded on every load(), so a background refresh cannot silently discard
// half-typed edits.
const editing = ref(false);
const saving = ref(false);
const form = ref({ title: "", category: "", content: "" });

function startEdit() {
  if (!report.value) return;
  form.value = {
    title: report.value.title,
    category: report.value.category,
    content: report.value.content,
  };
  editing.value = true;
  error.value = "";
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
  if (form.value.category !== current.category)
    patch.category = form.value.category;
  if (form.value.content !== current.content)
    patch.content = form.value.content;

  if (Object.keys(patch).length === 0) {
    editing.value = false;
    flash("没有改动");
    return;
  }

  saving.value = true;
  error.value = "";
  try {
    report.value = await api.updateReport(current.id, patch);
    editing.value = false;
    flash("已保存。已有分享链接立即展示新内容。");
  } catch (err) {
    error.value = err instanceof Error ? err.message : "保存失败";
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
  error.value = "";
  try {
    await api.deleteReport(report.value.id);
    await router.push("/dashboard/reports");
  } catch (err) {
    error.value = err instanceof Error ? err.message : "删除失败";
    deleting.value = false;
  }
}

/** True once the report has been edited after publication. */
const wasEdited = computed(
  () => !!report.value && report.value.updated_at > report.value.created_at,
);
</script>

<template>
  <div
    v-if="loading"
    class="px-5 py-12 text-center text-ink-muted"
    role="status"
  >
    <span class="spinner mr-2" aria-hidden="true"></span> 正在加载…
  </div>

  <div v-else-if="!report" class="flex flex-col gap-4">
    <div class="alert alert-error" role="alert">
      {{ error || "报告不存在" }}
    </div>
    <div>
      <button
        class="btn"
        type="button"
        @click="router.push('/dashboard/reports')"
      >
        返回列表
      </button>
    </div>
  </div>

  <template v-else>
    <div class="page-head">
      <div>
        <RouterLink
          to="/dashboard/reports"
          class="text-[13px] text-ink-muted no-underline hover:text-ink hover:underline"
        >
          ← 返回列表
        </RouterLink>
        <h1 class="mt-2">{{ report.title }}</h1>
        <p class="flex flex-wrap items-center gap-x-2.5 gap-y-1">
          <span class="badge">{{ report.category }}</span>
          <span class="badge">{{ report.format }}</span>
          <span class="text-ink-muted">
            {{ activeCount }} / {{ report.share_tokens.length }} 条链接有效
          </span>
          <span
            v-if="wasEdited"
            class="text-ink-muted"
            :title="formatDateTime(report.updated_at)"
          >
            已于 {{ formatDateTime(report.updated_at) }} 更新
          </span>
        </p>
      </div>

      <div class="flex items-center gap-2">
        <button
          class="btn"
          type="button"
          @click="editing ? (editing = false) : startEdit()"
        >
          {{ editing ? "取消编辑" : "编辑内容" }}
        </button>
        <button
          class="btn btn-danger"
          type="button"
          :disabled="deleting"
          @click="removeReport"
        >
          {{ deleting ? "正在删除…" : "删除报告" }}
        </button>
      </div>
    </div>

    <!-- ---- edit ---- -->
    <section v-if="editing" class="card mb-5 flex flex-col gap-3">
      <h2 class="m-0 text-lg font-semibold">编辑报告</h2>
      <p class="m-0 text-[13px] text-ink-muted">
        保存后<strong class="font-semibold text-ink">立即生效</strong>：报告 ID
        不变，因此
        <strong class="font-semibold text-ink"
          >已经发出去的分享链接会直接展示新内容</strong
        >，不会失效也不会换成新链接。
        如果你需要「发出即冻结」的那一版，请改为重新发布一篇。
      </p>

      <div>
        <label class="field-label" for="edit-title">标题</label>
        <input
          id="edit-title"
          name="edit_title"
          autocomplete="off"
          v-model="form.title"
          class="input"
          type="text"
          maxlength="300"
        />
      </div>

      <div>
        <label class="field-label" for="edit-category">分类</label>
        <input
          id="edit-category"
          name="edit_category"
          autocomplete="off"
          v-model="form.category"
          class="input"
          type="text"
          placeholder="general / pentest / architecture / progress"
          aria-describedby="edit-category-hint"
        />
        <p id="edit-category-hint" class="mt-1.5 mb-0 text-xs text-ink-muted">
          小写字母、数字、下划线与连字符，最长 32 字符
        </p>
      </div>

      <div>
        <label class="field-label" for="edit-content">正文（Markdown）</label>
        <textarea
          id="edit-content"
          name="edit_content"
          autocomplete="off"
          v-model="form.content"
          class="input font-mono text-[13px]"
          rows="18"
        ></textarea>
      </div>

      <div class="flex justify-end gap-2">
        <button
          class="btn"
          type="button"
          :disabled="saving"
          @click="editing = false"
        >
          取消
        </button>
        <button
          class="btn btn-primary"
          type="button"
          :disabled="saving"
          @click="saveEdit"
        >
          {{ saving ? "保存中…" : "保存" }}
        </button>
      </div>
    </section>

    <div v-if="error" class="alert alert-error mb-4" role="alert">
      {{ error }}
    </div>
    <div v-if="notice" class="alert alert-ok mb-4" role="status">
      {{ notice }}
    </div>

    <!-- ---- share tokens ---- -->
    <section class="card mb-5 flex flex-col gap-3">
      <div class="flex flex-wrap items-center gap-2">
        <h2 class="m-0 text-lg font-semibold">分享链接</h2>
        <div class="flex-1"></div>
        <label class="sr-only" for="new-share-hours">新链接有效期</label>
        <select
          id="new-share-hours"
          name="new_share_hours"
          autocomplete="off"
          v-model.number="newShareHours"
          class="input w-auto"
        >
          <option :value="1">1 小时</option>
          <option :value="6">6 小时</option>
          <option :value="24">24 小时</option>
          <option :value="168">7 天</option>
          <option :value="720">30 天</option>
          <option :value="0">永不过期</option>
        </select>
        <button class="btn btn-primary" type="button" @click="createShare">
          生成新链接
        </button>
      </div>

      <p
        v-if="report.share_tokens.length === 0"
        class="m-0 text-sm text-ink-muted"
      >
        暂无分享链接，生成一条以便分享。
      </p>

      <div v-else class="overflow-x-auto">
        <table class="data-table">
          <caption class="sr-only">
            该报告的分享链接
          </caption>
          <thead>
            <tr>
              <th scope="col">链接</th>
              <th scope="col">状态</th>
              <th scope="col">有效期</th>
              <th scope="col">浏览</th>
              <th scope="col" class="text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in report.share_tokens" :key="t.token">
              <td class="min-w-[260px]">
                <div class="token-url">{{ shareUrl(t.token) }}</div>
                <button
                  class="btn btn-ghost mt-1"
                  type="button"
                  @click="copy(t.token)"
                >
                  {{ copiedToken === t.token ? "✓ 已复制" : "复制链接" }}
                </button>
              </td>
              <td>
                <span
                  class="badge"
                  :class="{
                    'badge-ok': expiryState(t) === 'active',
                    'badge-warn': expiryState(t) === 'expired',
                    'badge-danger': expiryState(t) === 'revoked',
                  }"
                >
                  {{
                    expiryState(t) === "active"
                      ? "有效"
                      : expiryState(t) === "expired"
                        ? "已过期"
                        : "已禁用"
                  }}
                </span>
              </td>
              <td class="text-[13px] text-ink-muted">
                {{ formatExpiry(t.expires_at) }}
              </td>
              <td class="text-ink-muted">{{ t.view_count }}</td>
              <td>
                <div class="flex items-center justify-end gap-1">
                  <label class="sr-only" :for="`extend-${t.token}`"
                    >调整有效期</label
                  >
                  <select
                    :id="`extend-${t.token}`"
                    name="`extend_${t.token}`"
                    autocomplete="off"
                    class="input w-auto px-2 py-1 text-[13px]"
                    :disabled="busyToken === t.token"
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
                    class="btn btn-ghost"
                    type="button"
                    :disabled="busyToken === t.token"
                    @click="toggleActive(t)"
                  >
                    {{ t.is_active ? "禁用" : "恢复" }}
                  </button>
                  <button
                    class="btn btn-ghost btn-danger"
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
      </div>
    </section>

    <!-- ---- metadata ---- -->
    <section class="card mb-5 flex flex-col gap-3">
      <h2 class="m-0 text-lg font-semibold">元数据</h2>

      <dl
        class="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3 text-[13px]"
      >
        <div>
          <dt class="mb-0.5 text-ink-muted">报告 ID</dt>
          <dd class="m-0 font-mono">{{ report.id }}</dd>
        </div>
        <div>
          <dt class="mb-0.5 text-ink-muted">创建时间</dt>
          <dd class="m-0">{{ formatDateTime(report.created_at) }}</dd>
        </div>
        <div>
          <dt class="mb-0.5 text-ink-muted">更新时间</dt>
          <dd class="m-0">{{ formatDateTime(report.updated_at) }}</dd>
        </div>
        <div>
          <dt class="mb-0.5 text-ink-muted">分类</dt>
          <dd class="m-0">{{ report.category }}</dd>
        </div>
      </dl>

      <!--
        This block used to be a lone `<dt>` sitting outside any `<dl>` — invalid
        HTML (a definition term with no list around it) that assistive tech had
        to guess at. It is really a labelled block of raw JSON, so it now says
        that: a real `<h3>` heading tied to the `<pre>` via `aria-labelledby`.
      -->
      <div v-if="Object.keys(report.metadata).length">
        <h3
          id="metadata-json-heading"
          class="mt-0 mb-1.5 text-[13px] font-medium text-ink-muted"
        >
          metadata
        </h3>
        <pre
          class="code-block max-h-[200px]"
          aria-labelledby="metadata-json-heading"
          >{{ JSON.stringify(report.metadata, null, 2) }}</pre>
      </div>
      <p v-else class="m-0 text-[13px] text-ink-muted">无附加元数据</p>
    </section>

    <!-- ---- content ---- -->
    <section class="card flex flex-col gap-3">
      <h2 class="m-0 text-lg font-semibold">正文</h2>
      <p class="m-0 text-xs text-ink-muted">
        此处显示原始源文（只读，不执行）。Markdown 由 Go 服务端渲染；
        <code>html</code> 格式的正文在分享页里<strong
          class="font-semibold text-ink"
          >原样执行</strong
        >
        —— 下面看到的脚本真的会跑。
      </p>
      <pre class="code-block">{{ report.content }}</pre>
    </section>
  </template>
</template>
