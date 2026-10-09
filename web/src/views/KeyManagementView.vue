<script setup lang="ts">
/**
 * Agent key management: the pending-application queue, the issued-key list and
 * the renewal queue.
 *
 * Security notes that must stay true:
 *  - A plaintext token lives only in `issued`, i.e. in memory, and only until
 *    its dialog is closed. It is never written to localStorage/sessionStorage,
 *    never put in the URL and never logged.
 *  - Stored keys are identified by `token_prefix` only; the API returns no hash
 *    and this view models none.
 *  - Every timestamp is epoch milliseconds, 0 meaning "never" (see format.ts).
 *
 * The three sections load independently, so a backend that is down, missing the
 * route, or has not been built yet leaves the rest of the page usable and shows
 * a per-section error instead of breaking the render.
 */
import { onMounted, ref, type Ref } from 'vue';
import {
  ApiError,
  api,
  type AgentKey,
  type IssuedAgentKey,
  type KeyApplication,
  type KeyRenewal,
} from '../api';
import { copyText, formatDateTime, formatExpiry, relativeTime } from '../format';

const applications = ref<KeyApplication[]>([]);
const applicationsLoading = ref(true);
const applicationsError = ref('');

const keys = ref<AgentKey[]>([]);
const keysLoading = ref(true);
const keysError = ref('');

const renewals = ref<KeyRenewal[]>([]);
const renewalsLoading = ref(true);
const renewalsError = ref('');

/** `kind:id` of the row whose request is in flight, to disable its buttons. */
const busy = ref<string | null>(null);
const notice = ref('');
const actionError = ref('');

// -- dialogs ------------------------------------------------------------------
/** Plaintext token, kept in memory only. */
const issued = ref<IssuedAgentKey | null>(null);
const issuedCopied = ref(false);

const approveTarget = ref<KeyApplication | null>(null);
const approveForm = ref({ name: '', hours: 24, note: '' });

const editTarget = ref<AgentKey | null>(null);
const editForm = ref({ name: '', hours: 24 });

const renewalTarget = ref<KeyRenewal | null>(null);
const renewalForm = ref({ hours: 24 });

const dialogError = ref('');

const createForm = ref({ name: '', hours: 24 });

// -- loading ------------------------------------------------------------------
/** 401 is handled globally by App.vue; everything else is rendered inline. */
function apiMessage(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.message;
  return err instanceof Error ? err.message : fallback;
}

/**
 * A 404/405 or an HTML body means the route is not served (backend not built
 * yet, or the SPA fallback answered) — worth naming instead of a bare failure.
 */
function loadMessage(err: unknown, fallback: string): string {
  if (
    err instanceof ApiError &&
    (err.status === 404 || err.status === 405 || err.code === 'bad_response')
  ) {
    return `${err.message}（该接口可能尚未部署）`;
  }
  return apiMessage(err, fallback);
}

function flash(message: string) {
  notice.value = message;
  window.setTimeout(() => {
    if (notice.value === message) notice.value = '';
  }, 2500);
}

function clearNotices() {
  notice.value = '';
  actionError.value = '';
}

async function loadApplications() {
  applicationsLoading.value = true;
  applicationsError.value = '';
  try {
    applications.value = await api.listKeyApplications('pending');
  } catch (err) {
    if (!(err instanceof ApiError && err.isAuthError)) {
      applicationsError.value = loadMessage(err, '加载申请失败');
    }
  } finally {
    applicationsLoading.value = false;
  }
}

async function loadKeys() {
  keysLoading.value = true;
  keysError.value = '';
  try {
    keys.value = await api.listAgentKeys();
  } catch (err) {
    if (!(err instanceof ApiError && err.isAuthError)) {
      keysError.value = loadMessage(err, '加载密钥失败');
    }
  } finally {
    keysLoading.value = false;
  }
}

async function loadRenewals() {
  renewalsLoading.value = true;
  renewalsError.value = '';
  try {
    renewals.value = await api.listKeyRenewals('pending');
  } catch (err) {
    if (!(err instanceof ApiError && err.isAuthError)) {
      renewalsError.value = loadMessage(err, '加载续期申请失败');
    }
  } finally {
    renewalsLoading.value = false;
  }
}

function loadAll() {
  clearNotices();
  return Promise.all([loadApplications(), loadKeys(), loadRenewals()]);
}

onMounted(loadAll);

// -- formatting helpers -------------------------------------------------------
type KeyState = 'active' | 'expired' | 'revoked';

const stateLabel: Record<KeyState, string> = {
  active: '有效',
  expired: '已过期',
  revoked: '已撤销',
};

function keyState(key: AgentKey): KeyState {
  if (key.revoked_at > 0) return 'revoked';
  if (key.expires_at !== 0 && Date.now() >= key.expires_at) return 'expired';
  return 'active';
}

function formatLastUsed(ms: number): string {
  return ms > 0 ? relativeTime(ms) : '从未使用';
}

function formatLastUsedTitle(ms: number): string {
  return ms > 0 ? formatDateTime(ms) : '从未使用';
}

/** `requestedHours = 0` on an application means the agent did not specify one. */
function formatRequestedHours(hours: number): string {
  return hours > 0 ? `${hours} 小时` : '未指定';
}

/** Whole hours left on a key, for prefilling the edit form (1 = expired). */
function hoursLeft(expiresAt: number): number {
  if (expiresAt === 0) return 0;
  return Math.max(1, Math.ceil((expiresAt - Date.now()) / 3_600_000));
}

/**
 * Validates the hours field. Returns null and sets `target` on bad input;
 * 0 means never expires and is only sent when the user actually typed 0.
 */
function parseHours(raw: number, target: Ref<string>): number | null {
  const hours = Number(raw);
  if (!Number.isFinite(hours) || hours < 0) {
    target.value = '有效期必须是不小于 0 的整数（0 = 永不过期）';
    return null;
  }
  return Math.floor(hours);
}

// -- applications -------------------------------------------------------------
function openApprove(application: KeyApplication) {
  approveTarget.value = application;
  approveForm.value = {
    name: application.label,
    hours: application.requested_hours > 0 ? application.requested_hours : 24,
    note: '',
  };
  dialogError.value = '';
}

async function submitApprove() {
  const target = approveTarget.value;
  if (!target) return;

  const hours = parseHours(approveForm.value.hours, dialogError);
  if (hours === null) return;

  busy.value = `app:${target.id}`;
  try {
    const body: { name?: string; expiresInHours: number; note?: string } = {
      expiresInHours: hours,
    };
    const name = approveForm.value.name.trim();
    if (name) body.name = name;
    const note = approveForm.value.note.trim();
    if (note) body.note = note;

    await api.approveKeyApplication(target.id, body);
    approveTarget.value = null;
    await Promise.all([loadApplications(), loadKeys()]);
    // Approval alone does not mint a key: the token is derived from the claim
    // secret, which the server only has as a hash, so the key row appears when
    // the agent claims it. Saying so avoids "the button did nothing".
    const suffix = hours === 0 ? '（永不过期）' : `（${hours} 小时）`;
    flash(`已批准「${name || target.label}」${suffix}；等待 agent 领取，领取后才会出现在密钥列表`);
  } catch (err) {
    dialogError.value = apiMessage(err, '批准失败');
  } finally {
    busy.value = null;
  }
}

async function rejectApplication(application: KeyApplication) {
  const reason = window.prompt(
    `拒绝「${application.label}」的申请？可填写拒绝原因（留空直接确定）：`,
    '',
  );
  if (reason === null) return;

  clearNotices();
  busy.value = `app:${application.id}`;
  try {
    const body = reason.trim() ? { reason: reason.trim() } : {};
    await api.rejectKeyApplication(application.id, body);
    await loadApplications();
    flash('已拒绝该申请');
  } catch (err) {
    actionError.value = apiMessage(err, '拒绝失败');
  } finally {
    busy.value = null;
  }
}

// -- manual key creation ------------------------------------------------------
async function createKey() {
  clearNotices();
  const name = createForm.value.name.trim();
  if (!name) {
    actionError.value = '请填写密钥名称';
    return;
  }

  const hours = parseHours(createForm.value.hours, actionError);
  if (hours === null) return;

  busy.value = 'create';
  try {
    const result = await api.createAgentKey({ name, expiresInHours: hours });
    if (!result.token) {
      actionError.value = '服务器没有返回明文密钥，请刷新后重试';
    } else {
      // Kept in memory only; cleared as soon as the dialog closes.
      issued.value = result;
      issuedCopied.value = false;
      createForm.value = { name: '', hours: 24 };
    }
    await loadKeys();
  } catch (err) {
    actionError.value = apiMessage(err, '生成失败');
  } finally {
    busy.value = null;
  }
}

function closeIssued() {
  issued.value = null;
  issuedCopied.value = false;
}

async function copyIssued() {
  if (!issued.value) return;
  if (await copyText(issued.value.token)) {
    issuedCopied.value = true;
    window.setTimeout(() => {
      issuedCopied.value = false;
    }, 1800);
  } else {
    dialogError.value = '复制失败，请手动选择并复制';
  }
}

// -- key editing / revocation -------------------------------------------------
function openEdit(key: AgentKey) {
  editTarget.value = key;
  editForm.value = { name: key.name, hours: hoursLeft(key.expires_at) };
  dialogError.value = '';
}

async function submitEdit() {
  const target = editTarget.value;
  if (!target) return;

  const name = editForm.value.name.trim();
  if (!name) {
    dialogError.value = '请填写密钥名称';
    return;
  }

  const hours = parseHours(editForm.value.hours, dialogError);
  if (hours === null) return;

  busy.value = `key:${target.id}`;
  try {
    await api.updateAgentKey(target.id, { name, expiresInHours: hours });
    editTarget.value = null;
    await loadKeys();
    flash(hours === 0 ? '已保存，有效期设为永不过期' : `已保存，有效期 ${hours} 小时`);
  } catch (err) {
    dialogError.value = apiMessage(err, '保存失败');
  } finally {
    busy.value = null;
  }
}

async function revokeKey(key: AgentKey) {
  if (
    !window.confirm(`确定撤销密钥「${key.name}」？使用它的 agent 将立即无法鉴权，此操作不可恢复。`)
  ) {
    return;
  }

  clearNotices();
  busy.value = `key:${key.id}`;
  try {
    await api.updateAgentKey(key.id, { revoked: true });
    await loadKeys();
    flash('密钥已撤销');
  } catch (err) {
    actionError.value = apiMessage(err, '撤销失败');
  } finally {
    busy.value = null;
  }
}

// -- renewals -----------------------------------------------------------------
function openRenewal(renewal: KeyRenewal) {
  renewalTarget.value = renewal;
  renewalForm.value = { hours: renewal.requested_hours > 0 ? renewal.requested_hours : 24 };
  dialogError.value = '';
}

async function submitRenewal() {
  const target = renewalTarget.value;
  if (!target) return;

  const hours = parseHours(renewalForm.value.hours, dialogError);
  if (hours === null) return;

  busy.value = `renewal:${target.id}`;
  try {
    await api.approveKeyRenewal(target.id, { expiresInHours: hours });
    renewalTarget.value = null;
    await Promise.all([loadRenewals(), loadKeys()]);
    flash(hours === 0 ? '已批准续期，永不过期' : `已批准续期，有效期 ${hours} 小时`);
  } catch (err) {
    dialogError.value = apiMessage(err, '批准失败');
  } finally {
    busy.value = null;
  }
}

async function rejectRenewal(renewal: KeyRenewal) {
  if (!window.confirm('确定拒绝这条续期申请？')) return;

  clearNotices();
  busy.value = `renewal:${renewal.id}`;
  try {
    await api.rejectKeyRenewal(renewal.id);
    await loadRenewals();
    flash('已拒绝该续期申请');
  } catch (err) {
    actionError.value = apiMessage(err, '拒绝失败');
  } finally {
    busy.value = null;
  }
}
</script>

<template>
  <div class="page-head">
    <div>
      <h1>密钥管理</h1>
      <p>审批 agent 的密钥申请与续期，管理已颁发的密钥。</p>
    </div>
    <div class="row">
      <button type="button" :disabled="applicationsLoading || keysLoading || renewalsLoading" @click="loadAll">
        刷新
      </button>
    </div>
  </div>

  <div v-if="notice" class="alert ok" style="margin-bottom: 16px">{{ notice }}</div>
  <div v-if="actionError" class="alert error" style="margin-bottom: 16px">{{ actionError }}</div>

  <!-- ---- pending applications ---- -->
  <section class="card stack" style="margin-bottom: 20px">
    <div class="row">
      <h2 style="margin: 0; font-size: 1.05rem">待批准申请</h2>
      <span v-if="!applicationsLoading" class="badge">{{ applications.length }}</span>
      <div class="spacer"></div>
      <span class="muted" style="font-size: 12px">批准只授权；密钥在 agent 领取时生成</span>
    </div>

    <div v-if="applicationsLoading" class="empty"><span class="spin"></span> 正在加载申请…</div>

    <template v-else-if="applicationsError">
      <div class="alert error">{{ applicationsError }}</div>
      <div>
        <button type="button" @click="loadApplications">重试</button>
      </div>
    </template>

    <p v-else-if="applications.length === 0" class="empty" style="margin: 0">
      暂无待批准的申请。agent 调用
      <code class="mono">POST /api/agent-keys/applications</code> 提交后会出现在这里。
    </p>

    <table v-else class="table">
      <thead>
        <tr>
          <th>申请方</th>
          <th>申请时长</th>
          <th>来源</th>
          <th>申请时间</th>
          <th style="text-align: right">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="a in applications" :key="a.id">
          <td>
            <div style="font-weight: 550">{{ a.label }}</div>
            <div v-if="a.purpose" class="muted" style="font-size: 12px">{{ a.purpose }}</div>
            <div class="muted mono" style="font-size: 11px">{{ a.id }}</div>
          </td>
          <td class="muted">{{ formatRequestedHours(a.requested_hours) }}</td>
          <td class="muted" style="font-size: 12px">
            <div class="mono">{{ a.requester_ip || '—' }}</div>
            <div class="ua" :title="a.user_agent">{{ a.user_agent || '—' }}</div>
          </td>
          <td class="muted" :title="formatDateTime(a.created_at)">{{ relativeTime(a.created_at) }}</td>
          <td>
            <div class="row" style="justify-content: flex-end; gap: 4px">
              <button
                class="primary"
                type="button"
                :disabled="busy === `app:${a.id}`"
                @click="openApprove(a)"
              >
                批准
              </button>
              <button
                class="ghost danger"
                type="button"
                :disabled="busy === `app:${a.id}`"
                @click="rejectApplication(a)"
              >
                拒绝
              </button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
  </section>

  <!-- ---- keys ---- -->
  <section class="card stack" style="margin-bottom: 20px">
    <div class="row">
      <h2 style="margin: 0; font-size: 1.05rem">密钥</h2>
      <span v-if="!keysLoading" class="badge">{{ keys.length }}</span>
      <div class="spacer"></div>
      <input v-model="createForm.name" type="text" placeholder="手动生成：名称" style="width: 180px" />
      <input
        v-model.number="createForm.hours"
        type="number"
        min="0"
        step="1"
        aria-label="有效期（小时，0 = 永不过期）"
        style="width: 110px"
      />
      <span class="muted" style="font-size: 12px">小时（0 = 永不过期）</span>
      <button class="primary" type="button" :disabled="busy === 'create'" @click="createKey">
        手动生成密钥
      </button>
    </div>

    <p class="muted" style="margin: 0; font-size: 12px">
      文件名以 <span class="mono">token_prefix</span> 辨识；明文密钥只在生成时显示一次。
    </p>

    <div v-if="keysLoading" class="empty"><span class="spin"></span> 正在加载密钥…</div>

    <template v-else-if="keysError">
      <div class="alert error">{{ keysError }}</div>
      <div>
        <button type="button" @click="loadKeys">重试</button>
      </div>
    </template>

    <p v-else-if="keys.length === 0" class="empty" style="margin: 0">
      还没有密钥。可手动生成并转交；已批准的申请要等 agent 领取后才会出现在这里。
    </p>

    <table v-else class="table">
      <thead>
        <tr>
          <th>名称</th>
          <th>前缀</th>
          <th>创建时间</th>
          <th>到期时间</th>
          <th>最后使用</th>
          <th>调用次数</th>
          <th>状态</th>
          <th style="text-align: right">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="k in keys" :key="k.id">
          <td>
            <div style="font-weight: 550">{{ k.name || '（未命名）' }}</div>
            <div v-if="k.note" class="muted" style="font-size: 12px">{{ k.note }}</div>
            <div class="muted mono" style="font-size: 11px">{{ k.id }}</div>
          </td>
          <td class="mono">{{ k.token_prefix || '—' }}…</td>
          <td class="muted" :title="formatDateTime(k.created_at)">
            {{ formatDateTime(k.created_at) }}
          </td>
          <td class="muted" style="font-size: 13px">{{ formatExpiry(k.expires_at) }}</td>
          <td class="muted" style="font-size: 13px" :title="formatLastUsedTitle(k.last_used_at)">
            {{ formatLastUsed(k.last_used_at) }}
          </td>
          <td class="muted">{{ k.request_count }}</td>
          <td>
            <span
              class="badge"
              :class="{
                ok: keyState(k) === 'active',
                warn: keyState(k) === 'expired',
                danger: keyState(k) === 'revoked',
              }"
            >
              {{ stateLabel[keyState(k)] }}
            </span>
          </td>
          <td>
            <div class="row" style="justify-content: flex-end; gap: 4px">
              <button class="ghost" type="button" :disabled="busy === `key:${k.id}`" @click="openEdit(k)">
                编辑
              </button>
              <button
                class="ghost danger"
                type="button"
                :disabled="busy === `key:${k.id}` || keyState(k) === 'revoked'"
                @click="revokeKey(k)"
              >
                撤销
              </button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
  </section>

  <!-- ---- renewals ---- -->
  <section class="card stack">
    <div class="row">
      <h2 style="margin: 0; font-size: 1.05rem">续期申请</h2>
      <span v-if="!renewalsLoading" class="badge">{{ renewals.length }}</span>
    </div>

    <div v-if="renewalsLoading" class="empty"><span class="spin"></span> 正在加载续期申请…</div>

    <template v-else-if="renewalsError">
      <div class="alert error">{{ renewalsError }}</div>
      <div>
        <button type="button" @click="loadRenewals">重试</button>
      </div>
    </template>

    <p v-else-if="renewals.length === 0" class="empty" style="margin: 0">暂无待处理的续期申请。</p>

    <table v-else class="table">
      <thead>
        <tr>
          <th>密钥 ID</th>
          <th>申请时长</th>
          <th>申请时间</th>
          <th style="text-align: right">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in renewals" :key="r.id">
          <td class="mono">{{ r.key_id }}</td>
          <td class="muted">{{ formatRequestedHours(r.requested_hours) }}</td>
          <td class="muted" :title="formatDateTime(r.created_at)">{{ relativeTime(r.created_at) }}</td>
          <td>
            <div class="row" style="justify-content: flex-end; gap: 4px">
              <button
                class="primary"
                type="button"
                :disabled="busy === `renewal:${r.id}`"
                @click="openRenewal(r)"
              >
                批准
              </button>
              <button
                class="ghost danger"
                type="button"
                :disabled="busy === `renewal:${r.id}`"
                @click="rejectRenewal(r)"
              >
                拒绝
              </button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
  </section>

  <!-- ---- approve application dialog ---- -->
  <div v-if="approveTarget" class="modal-backdrop" @click.self="approveTarget = null">
    <form class="card modal stack" @submit.prevent="submitApprove">
      <h2>批准申请</h2>
      <p class="muted" style="margin: 0; font-size: 13px">
        {{ approveTarget.label }} · 申请 {{ formatRequestedHours(approveTarget.requested_hours) }}
      </p>
      <p class="muted" style="margin: 0; font-size: 12px">
        批准不会立刻生成密钥：token 由 agent 的 claim secret 派生，只有 agent 在领取窗口内自取时
        服务端才算得出来。领取成功后该密钥才会出现在下面的「密钥」列表里。
      </p>

      <div v-if="dialogError" class="alert error">{{ dialogError }}</div>

      <div>
        <label for="approve-name">密钥名称</label>
        <input id="approve-name" v-model="approveForm.name" type="text" :disabled="busy !== null" />
      </div>
      <div>
        <label for="approve-hours">有效期（小时，0 = 永不过期）</label>
        <input
          id="approve-hours"
          v-model.number="approveForm.hours"
          type="number"
          min="0"
          step="1"
          :disabled="busy !== null"
        />
        <p class="muted" style="font-size: 12px; margin: 6px 0 0">
          {{ approveForm.hours === 0 ? '0 = 永不过期，密钥将不再有到期时间。' : `到期时间：${approveForm.hours} 小时后` }}
        </p>
      </div>
      <div>
        <label for="approve-note">备注（可选）</label>
        <input id="approve-note" v-model="approveForm.note" type="text" :disabled="busy !== null" />
      </div>

      <div class="row" style="justify-content: flex-end">
        <button type="button" :disabled="busy !== null" @click="approveTarget = null">取消</button>
        <button class="primary" type="submit" :disabled="busy !== null">
          <span v-if="busy !== null" class="spin" style="margin-right: 8px"></span>
          批准
        </button>
      </div>
    </form>
  </div>

  <!-- ---- edit key dialog ---- -->
  <div v-if="editTarget" class="modal-backdrop" @click.self="editTarget = null">
    <form class="card modal stack" @submit.prevent="submitEdit">
      <h2>编辑密钥</h2>
      <p class="muted mono" style="margin: 0; font-size: 12px">{{ editTarget.id }}</p>

      <div v-if="dialogError" class="alert error">{{ dialogError }}</div>

      <div>
        <label for="edit-name">名称</label>
        <input id="edit-name" v-model="editForm.name" type="text" :disabled="busy !== null" />
      </div>
      <div>
        <label for="edit-hours">有效期（小时，0 = 永不过期）</label>
        <input
          id="edit-hours"
          v-model.number="editForm.hours"
          type="number"
          min="0"
          step="1"
          :disabled="busy !== null"
        />
        <p class="muted" style="font-size: 12px; margin: 6px 0 0">
          {{
            editForm.hours === 0
              ? '0 = 永不过期；保存后立即生效（等于直接续期）。'
              : `保存后从此刻起重新计算：${editForm.hours} 小时后到期。`
          }}
        </p>
      </div>

      <div class="row" style="justify-content: flex-end">
        <button type="button" :disabled="busy !== null" @click="editTarget = null">取消</button>
        <button class="primary" type="submit" :disabled="busy !== null">
          <span v-if="busy !== null" class="spin" style="margin-right: 8px"></span>
          保存
        </button>
      </div>
    </form>
  </div>

  <!-- ---- approve renewal dialog ---- -->
  <div v-if="renewalTarget" class="modal-backdrop" @click.self="renewalTarget = null">
    <form class="card modal stack" @submit.prevent="submitRenewal">
      <h2>批准续期</h2>
      <p class="muted mono" style="margin: 0; font-size: 12px">{{ renewalTarget.key_id }}</p>

      <div v-if="dialogError" class="alert error">{{ dialogError }}</div>

      <div>
        <label for="renewal-hours">有效期（小时，0 = 永不过期）</label>
        <input
          id="renewal-hours"
          v-model.number="renewalForm.hours"
          type="number"
          min="0"
          step="1"
          :disabled="busy !== null"
        />
        <p class="muted" style="font-size: 12px; margin: 6px 0 0">
          {{ renewalForm.hours === 0 ? '0 = 永不过期。' : `批准后到期时间：${renewalForm.hours} 小时后` }}
        </p>
      </div>

      <div class="row" style="justify-content: flex-end">
        <button type="button" :disabled="busy !== null" @click="renewalTarget = null">取消</button>
        <button class="primary" type="submit" :disabled="busy !== null">
          <span v-if="busy !== null" class="spin" style="margin-right: 8px"></span>
          批准
        </button>
      </div>
    </form>
  </div>

  <!-- ---- one-time plaintext token dialog ---- -->
  <div v-if="issued" class="modal-backdrop">
    <div class="card modal stack">
      <h2>密钥已生成</h2>

      <div class="alert error">
        明文密钥<b>只显示这一次</b>，关闭此窗口后无法再查看，也无法找回。请立即复制并妥善保存。
      </div>
      <div v-if="dialogError" class="alert error">{{ dialogError }}</div>

      <div class="mono token-url">{{ issued.token }}</div>
      <p v-if="issued.key" class="muted" style="margin: 0; font-size: 13px">
        名称：{{ issued.key.name || '（未命名）' }} · 前缀：{{ issued.key.token_prefix || '—' }}<br />
        到期：{{ formatExpiry(issued.key.expires_at) }}
      </p>

      <div class="row" style="justify-content: flex-end">
        <button class="primary" type="button" @click="copyIssued">
          {{ issuedCopied ? '✓ 已复制' : '复制密钥' }}
        </button>
        <button type="button" @click="closeIssued">我已保存，关闭</button>
      </div>
    </div>
  </div>
</template>
