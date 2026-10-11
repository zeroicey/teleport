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
import {
  computed,
  nextTick,
  onBeforeUnmount,
  onMounted,
  ref,
  watch,
  type Ref,
} from "vue";
import {
  ApiError,
  api,
  type AgentKey,
  type IssuedAgentKey,
  type KeyApplication,
  type KeyHandoff,
  type KeyRenewal,
} from "../api";
import {
  copyText,
  formatDateTime,
  formatExpiry,
  relativeTime,
} from "../format";

const applications = ref<KeyApplication[]>([]);
const applicationsLoading = ref(true);
const applicationsError = ref("");

const keys = ref<AgentKey[]>([]);
const keysLoading = ref(true);
const keysError = ref("");

const renewals = ref<KeyRenewal[]>([]);
const renewalsLoading = ref(true);
const renewalsError = ref("");

/** `kind:id` of the row whose request is in flight, to disable its buttons. */
const busy = ref<string | null>(null);
const notice = ref("");
const actionError = ref("");

// -- dialogs ------------------------------------------------------------------
/** Plaintext token, kept in memory only. */
const issued = ref<IssuedAgentKey | null>(null);
const issuedCopied = ref(false);

const approveTarget = ref<KeyApplication | null>(null);
const approveForm = ref({ name: "", hours: 24, note: "" });

const editTarget = ref<AgentKey | null>(null);
const editForm = ref({ name: "", hours: 24 });

const renewalTarget = ref<KeyRenewal | null>(null);
const renewalForm = ref({ hours: 24 });

const dialogError = ref("");

const createForm = ref({ name: "", hours: 24 });

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
    (err.status === 404 || err.status === 405 || err.code === "bad_response")
  ) {
    return `${err.message}（该接口可能尚未部署）`;
  }
  return apiMessage(err, fallback);
}

function flash(message: string) {
  notice.value = message;
  window.setTimeout(() => {
    if (notice.value === message) notice.value = "";
  }, 2500);
}

function clearNotices() {
  notice.value = "";
  actionError.value = "";
}

async function loadApplications() {
  applicationsLoading.value = true;
  applicationsError.value = "";
  try {
    applications.value = await api.listKeyApplications("pending");
  } catch (err) {
    if (!(err instanceof ApiError && err.isAuthError)) {
      applicationsError.value = loadMessage(err, "加载申请失败");
    }
  } finally {
    applicationsLoading.value = false;
  }
}

async function loadKeys() {
  keysLoading.value = true;
  keysError.value = "";
  try {
    keys.value = await api.listAgentKeys();
  } catch (err) {
    if (!(err instanceof ApiError && err.isAuthError)) {
      keysError.value = loadMessage(err, "加载密钥失败");
    }
  } finally {
    keysLoading.value = false;
  }
}

async function loadRenewals() {
  renewalsLoading.value = true;
  renewalsError.value = "";
  try {
    renewals.value = await api.listKeyRenewals("pending");
  } catch (err) {
    if (!(err instanceof ApiError && err.isAuthError)) {
      renewalsError.value = loadMessage(err, "加载续期申请失败");
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
type KeyState = "active" | "expired" | "revoked";

const stateLabel: Record<KeyState, string> = {
  active: "有效",
  expired: "已过期",
  revoked: "已撤销",
};

function keyState(key: AgentKey): KeyState {
  if (key.revoked_at > 0) return "revoked";
  if (key.expires_at !== 0 && Date.now() >= key.expires_at) return "expired";
  return "active";
}

/**
 * Whether an expired key can still file a renewal request.
 *
 * This is the difference between "this agent is dead and its reports are
 * orphaned" and "this agent can ask you for more time" — the whole reason the
 * grace window exists — so the badge must not flatten the two into one grey
 * "已过期".
 *
 * The window length lives in the server's KEY_RENEWAL_GRACE and is deliberately
 * not exposed here: an admin endpoint that published the deployment's internal
 * policy would be a new disclosure surface for no real gain. The panel therefore
 * states what it actually knows — the key lapsed and can ask again — and lets
 * the server enforce the deadline. Whether a request is still inside the window
 * is visible where it matters: it either appears in the renewal queue or is
 * refused with a 401.
 */
function expiredMayStillRenew(key: AgentKey): boolean {
  return keyState(key) === "expired";
}

function formatLastUsed(ms: number): string {
  return ms > 0 ? relativeTime(ms) : "从未使用";
}

function formatLastUsedTitle(ms: number): string {
  return ms > 0 ? formatDateTime(ms) : "从未使用";
}

/** `requestedHours = 0` on an application means the agent did not specify one. */
function formatRequestedHours(hours: number): string {
  return hours > 0 ? `${hours} 小时` : "未指定";
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
    target.value = "有效期必须是不小于 0 的整数（0 = 永不过期）";
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
    note: "",
  };
  dialogError.value = "";
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
    const suffix = hours === 0 ? "（永不过期）" : `（${hours} 小时）`;
    flash(
      `已批准「${name || target.label}」${suffix}；等待 agent 领取，领取后才会出现在密钥列表`,
    );
  } catch (err) {
    dialogError.value = apiMessage(err, "批准失败");
  } finally {
    busy.value = null;
  }
}

async function rejectApplication(application: KeyApplication) {
  const reason = window.prompt(
    `拒绝「${application.label}」的申请？可填写拒绝原因（留空直接确定）：`,
    "",
  );
  if (reason === null) return;

  clearNotices();
  busy.value = `app:${application.id}`;
  try {
    const body = reason.trim() ? { reason: reason.trim() } : {};
    await api.rejectKeyApplication(application.id, body);
    await loadApplications();
    flash("已拒绝该申请");
  } catch (err) {
    actionError.value = apiMessage(err, "拒绝失败");
  } finally {
    busy.value = null;
  }
}

// -- manual key creation ------------------------------------------------------
async function createKey() {
  clearNotices();
  const name = createForm.value.name.trim();
  if (!name) {
    actionError.value = "请填写密钥名称";
    return;
  }

  const hours = parseHours(createForm.value.hours, actionError);
  if (hours === null) return;

  busy.value = "create";
  try {
    const result = await api.createAgentKey({ name, expiresInHours: hours });
    if (!result.token) {
      actionError.value = "服务器没有返回明文密钥，请刷新后重试";
    } else {
      // Kept in memory only; cleared as soon as the dialog closes.
      issued.value = result;
      issuedCopied.value = false;
      createForm.value = { name: "", hours: 24 };
    }
    await loadKeys();
  } catch (err) {
    actionError.value = apiMessage(err, "生成失败");
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
    dialogError.value = "复制失败，请手动选择并复制";
  }
}

/**
 * One-step hand-off.
 *
 * The administrator decides here — that is the whole point: no second visit to
 * the approval queue. What comes back is a link whose *fragment* carries a
 * one-time claim secret, so this panel never receives a usable credential and
 * the server never logs one; the recipient exchanges the secret for the
 * credential in their own browser, or uses the pasted prompt if the recipient
 * is an agent that cannot read a fragment.
 */
const showHandoff = ref(false);
const handoffForm = ref({ label: "", hours: 24, note: "" });
const handoff = ref<KeyHandoff | null>(null);
const handoffCopied = ref<"url" | "prompt" | null>(null);

function openHandoff() {
  handoff.value = null;
  handoffForm.value = { label: "", hours: 24, note: "" };
  handoffCopied.value = null;
  dialogError.value = "";
  showHandoff.value = true;
}

function closeHandoff() {
  showHandoff.value = false;
  handoff.value = null;
  handoffCopied.value = null;
}

async function submitHandoff() {
  const label = handoffForm.value.label.trim();
  if (!label) {
    dialogError.value = "请填写收件人名称";
    return;
  }
  const hours = parseHours(handoffForm.value.hours, dialogError);
  if (hours === null) return;

  busy.value = "handoff";
  try {
    const body: { label: string; expiresInHours: number; note?: string } = {
      label,
      expiresInHours: hours,
    };
    const note = handoffForm.value.note.trim();
    if (note) body.note = note;
    handoff.value = await api.createKeyHandoff(body);
    flash(`已生成给「${label}」的交接链接；对方领取后密钥才会出现在列表里`);
  } catch (err) {
    dialogError.value = apiMessage(err, "生成交接链接失败");
  } finally {
    busy.value = null;
  }
}

async function copyHandoff(which: "url" | "prompt") {
  if (!handoff.value) return;
  const text = which === "url" ? handoff.value.url : handoff.value.prompt;
  if (await copyText(text)) {
    handoffCopied.value = which;
    window.setTimeout(() => {
      if (handoffCopied.value === which) handoffCopied.value = null;
    }, 1800);
  } else {
    dialogError.value = "复制失败，请手动选择并复制";
  }
}

// -- key editing / revocation -------------------------------------------------
function openEdit(key: AgentKey) {
  editTarget.value = key;
  editForm.value = { name: key.name, hours: hoursLeft(key.expires_at) };
  dialogError.value = "";
}

async function submitEdit() {
  const target = editTarget.value;
  if (!target) return;

  const name = editForm.value.name.trim();
  if (!name) {
    dialogError.value = "请填写密钥名称";
    return;
  }

  const hours = parseHours(editForm.value.hours, dialogError);
  if (hours === null) return;

  busy.value = `key:${target.id}`;
  try {
    await api.updateAgentKey(target.id, { name, expiresInHours: hours });
    editTarget.value = null;
    await loadKeys();
    flash(
      hours === 0
        ? "已保存，有效期设为永不过期"
        : `已保存，有效期 ${hours} 小时`,
    );
  } catch (err) {
    dialogError.value = apiMessage(err, "保存失败");
  } finally {
    busy.value = null;
  }
}

async function revokeKey(key: AgentKey) {
  if (
    !window.confirm(
      `确定撤销密钥「${key.name}」？使用它的 agent 将立即无法鉴权，此操作不可恢复。`,
    )
  ) {
    return;
  }

  clearNotices();
  busy.value = `key:${key.id}`;
  try {
    await api.updateAgentKey(key.id, { revoked: true });
    await loadKeys();
    flash("密钥已撤销");
  } catch (err) {
    actionError.value = apiMessage(err, "撤销失败");
  } finally {
    busy.value = null;
  }
}

// -- renewals -----------------------------------------------------------------
function openRenewal(renewal: KeyRenewal) {
  renewalTarget.value = renewal;
  renewalForm.value = {
    hours: renewal.requested_hours > 0 ? renewal.requested_hours : 24,
  };
  dialogError.value = "";
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
    flash(
      hours === 0 ? "已批准续期，永不过期" : `已批准续期，有效期 ${hours} 小时`,
    );
  } catch (err) {
    dialogError.value = apiMessage(err, "批准失败");
  } finally {
    busy.value = null;
  }
}

async function rejectRenewal(renewal: KeyRenewal) {
  if (!window.confirm("确定拒绝这条续期申请？")) return;

  clearNotices();
  busy.value = `renewal:${renewal.id}`;
  try {
    await api.rejectKeyRenewal(renewal.id);
    await loadRenewals();
    flash("已拒绝该续期申请");
  } catch (err) {
    actionError.value = apiMessage(err, "拒绝失败");
  } finally {
    busy.value = null;
  }
}

// -- dialog keyboard behaviour ------------------------------------------------
//
// Declared last on purpose: these helpers read every piece of dialog state
// above, so keeping them after all of it removes any ordering question (and any
// temporal-dead-zone trap) between the computeds and the refs they close over.
//
// Every dialog here is a real `role="dialog"` that covers the page, so leaving
// focus on the button that opened it means Tab walks the *hidden* content
// behind the overlay. These helpers give all five dialogs the same treatment:
// focus moves in on open, Tab cycles inside, and Escape dismisses.
//
// The one exception is the one-time plaintext-token dialog: Escape must NOT
// close it. The token is unrecoverable once that dialog is gone, and an Escape
// keypress is far too easy to make by reflex — losing a credential to a stray
// keystroke is not a trade worth making for consistency. It closes only via its
// explicit buttons, and the same reasoning is why its backdrop is not
// click-to-dismiss either.
const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/** Set on whichever dialog is currently mounted; at most one is. */
const dialogEl = ref<HTMLElement | null>(null);

const anyDialogOpen = computed(
  () =>
    issued.value !== null ||
    showHandoff.value ||
    approveTarget.value !== null ||
    editTarget.value !== null ||
    renewalTarget.value !== null,
);

/** Closes the topmost dialog. Never closes `issued` (see above). */
function closeTopDialog() {
  if (showHandoff.value) {
    closeHandoff();
    return;
  }
  if (renewalTarget.value) {
    renewalTarget.value = null;
    return;
  }
  if (editTarget.value) {
    editTarget.value = null;
    return;
  }
  if (approveTarget.value) {
    approveTarget.value = null;
  }
}

function onDialogKeydown(event: KeyboardEvent) {
  if (!anyDialogOpen.value || !dialogEl.value) return;

  if (event.key === "Escape") {
    if (issued.value) return; // unrecoverable content — explicit buttons only
    event.preventDefault();
    closeTopDialog();
    return;
  }

  if (event.key !== "Tab") return;

  // `offsetParent !== null` skips controls hidden by a `v-if` branch, so the
  // cycle only ever visits things the user can actually see.
  const items = Array.from(
    dialogEl.value.querySelectorAll<HTMLElement>(FOCUSABLE),
  ).filter((node) => node.offsetParent !== null);
  if (items.length === 0) return;

  const first = items[0]!;
  const last = items[items.length - 1]!;
  const active = document.activeElement;

  if (event.shiftKey && (active === first || active === dialogEl.value)) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && active === last) {
    event.preventDefault();
    first.focus();
  }
}

/** Focus the first control once a dialog has actually rendered. */
watch(anyDialogOpen, async (open) => {
  if (!open) return;
  await nextTick();
  const el = dialogEl.value;
  if (!el) return;
  (el.querySelector<HTMLElement>(FOCUSABLE) ?? el).focus();
});

/**
 * The hand-off dialog swaps its form for a result panel in place, so the
 * "opened" watcher above does not fire. Without this, the button that was just
 * submitted disappears and focus falls back to `<body>`, stranding a keyboard
 * user at the top of the page. Focus the panel itself instead.
 */
watch(handoff, async (created) => {
  if (!created) return;
  await nextTick();
  dialogEl.value?.focus();
});

onMounted(() => window.addEventListener("keydown", onDialogKeydown));
onBeforeUnmount(() => window.removeEventListener("keydown", onDialogKeydown));
</script>

<template>
  <div class="page-head">
    <div>
      <h1>密钥管理</h1>
      <p>审批 agent 的密钥申请与续期，管理已颁发的密钥。</p>
    </div>
    <button
      class="btn"
      type="button"
      :disabled="applicationsLoading || keysLoading || renewalsLoading"
      @click="loadAll"
    >
      刷新
    </button>
  </div>

  <div v-if="notice" class="alert alert-ok mb-4" role="status">
    {{ notice }}
  </div>
  <div v-if="actionError" class="alert alert-error mb-4" role="alert">
    {{ actionError }}
  </div>

  <!-- ---- pending applications ---- -->
  <section class="card mb-5 flex flex-col gap-3">
    <div class="flex flex-wrap items-center gap-2">
      <h2 class="m-0 text-lg font-semibold">待批准申请</h2>
      <span v-if="!applicationsLoading" class="badge">{{
        applications.length
      }}</span>
      <div class="flex-1"></div>
      <span class="text-xs text-ink-muted"
        >批准只授权；密钥在 agent 领取时生成</span
      >
    </div>

    <div
      v-if="applicationsLoading"
      class="px-5 py-12 text-center text-ink-muted"
      role="status"
    >
      <span class="spinner mr-2" aria-hidden="true"></span> 正在加载申请…
    </div>

    <template v-else-if="applicationsError">
      <div class="alert alert-error" role="alert">{{ applicationsError }}</div>
      <div>
        <button class="btn" type="button" @click="loadApplications">
          重试
        </button>
      </div>
    </template>

    <p
      v-else-if="applications.length === 0"
      class="m-0 px-5 py-12 text-center text-ink-muted"
    >
      暂无待批准的申请。agent 调用
      <code
        class="rounded bg-surface-sunken px-1.5 py-0.5 font-mono text-[13px]"
        >POST /api/agent-keys/applications</code
      >
      提交后会出现在这里。
    </p>

    <div v-else class="overflow-x-auto">
      <table class="data-table">
        <caption class="sr-only">
          待批准的密钥申请
        </caption>
        <thead>
          <tr>
            <th scope="col">申请方</th>
            <th scope="col">申请时长</th>
            <th scope="col">来源</th>
            <th scope="col">申请时间</th>
            <th scope="col" class="text-right">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="a in applications" :key="a.id">
            <td>
              <div class="font-medium">{{ a.label }}</div>
              <div v-if="a.purpose" class="text-xs text-ink-muted">
                {{ a.purpose }}
              </div>
              <div class="font-mono text-[11px] text-ink-muted">{{ a.id }}</div>
            </td>
            <td class="text-ink-muted">
              {{ formatRequestedHours(a.requested_hours) }}
            </td>
            <td class="text-xs text-ink-muted">
              <div class="font-mono">{{ a.requester_ip || "—" }}</div>
              <div class="max-w-[220px] truncate" :title="a.user_agent">
                {{ a.user_agent || "—" }}
              </div>
            </td>
            <td class="text-ink-muted" :title="formatDateTime(a.created_at)">
              {{ relativeTime(a.created_at) }}
            </td>
            <td>
              <div class="flex items-center justify-end gap-1">
                <button
                  class="btn btn-primary btn-sm"
                  type="button"
                  :disabled="busy === `app:${a.id}`"
                  @click="openApprove(a)"
                >
                  批准
                </button>
                <button
                  class="btn btn-ghost btn-danger"
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
    </div>
  </section>

  <!-- ---- keys ---- -->
  <section class="card mb-5 flex flex-col gap-3">
    <div class="flex flex-wrap items-center gap-2">
      <h2 class="m-0 text-lg font-semibold">密钥</h2>
      <span v-if="!keysLoading" class="badge">{{ keys.length }}</span>
      <div class="flex-1"></div>

      <label class="sr-only" for="create-name">手动生成：名称</label>
      <input
        id="create-name"
        name="create_name"
        autocomplete="off"
        v-model="createForm.name"
        class="input w-44"
        type="text"
        placeholder="手动生成：名称"
      />
      <label class="sr-only" for="create-hours"
        >有效期（小时，0 = 永不过期）</label
      >
      <input
        id="create-hours"
        name="create_hours"
        autocomplete="off"
        v-model.number="createForm.hours"
        class="input w-28"
        type="number"
        min="0"
        step="1"
      />
      <span class="text-xs text-ink-muted">小时（0 = 永不过期）</span>
      <button
        class="btn btn-primary"
        type="button"
        :disabled="busy === 'create'"
        @click="createKey"
      >
        手动生成密钥
      </button>
      <button class="btn" type="button" @click="openHandoff">
        生成交接链接
      </button>
    </div>

    <p class="m-0 text-xs text-ink-muted">
      文件名以
      <span class="font-mono">token_prefix</span>
      辨识；明文密钥只在生成时显示一次。
      「生成交接链接」把授权和交接合并成一步：对方自己领取，你不需要再回来点批准。
    </p>

    <div
      v-if="keysLoading"
      class="px-5 py-12 text-center text-ink-muted"
      role="status"
    >
      <span class="spinner mr-2" aria-hidden="true"></span> 正在加载密钥…
    </div>

    <template v-else-if="keysError">
      <div class="alert alert-error" role="alert">{{ keysError }}</div>
      <div>
        <button class="btn" type="button" @click="loadKeys">重试</button>
      </div>
    </template>

    <p
      v-else-if="keys.length === 0"
      class="m-0 px-5 py-12 text-center text-ink-muted"
    >
      还没有密钥。可手动生成并转交；已批准的申请要等 agent
      领取后才会出现在这里。
    </p>

    <div v-else class="overflow-x-auto">
      <table class="data-table">
        <caption class="sr-only">
          已颁发的密钥
        </caption>
        <thead>
          <tr>
            <th scope="col">名称</th>
            <th scope="col">前缀</th>
            <th scope="col">创建时间</th>
            <th scope="col">到期时间</th>
            <th scope="col">最后使用</th>
            <th scope="col">调用次数</th>
            <th scope="col">状态</th>
            <th scope="col" class="text-right">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="k in keys" :key="k.id">
            <td>
              <div class="font-medium">{{ k.name || "（未命名）" }}</div>
              <div v-if="k.note" class="text-xs text-ink-muted">
                {{ k.note }}
              </div>
              <div class="font-mono text-[11px] text-ink-muted">{{ k.id }}</div>
            </td>
            <td class="font-mono">{{ k.token_prefix || "—" }}…</td>
            <td class="text-ink-muted" :title="formatDateTime(k.created_at)">
              {{ formatDateTime(k.created_at) }}
            </td>
            <td class="text-[13px] text-ink-muted">
              {{ formatExpiry(k.expires_at) }}
            </td>
            <td
              class="text-[13px] text-ink-muted"
              :title="formatLastUsedTitle(k.last_used_at)"
            >
              {{ formatLastUsed(k.last_used_at) }}
            </td>
            <td class="text-ink-muted">{{ k.request_count }}</td>
            <td>
              <span
                class="badge"
                :class="{
                  'badge-ok': keyState(k) === 'active',
                  'badge-warn': keyState(k) === 'expired',
                  'badge-danger': keyState(k) === 'revoked',
                }"
              >
                {{ stateLabel[keyState(k)] }}
              </span>
              <div
                v-if="expiredMayStillRenew(k)"
                class="mt-0.5 text-[11px] text-ink-muted"
                title="宽限期内该密钥仍可提交续期申请；超过宽限期后彻底失效"
              >
                仍可申请续期
              </div>
            </td>
            <td>
              <div class="flex items-center justify-end gap-1">
                <button
                  class="btn btn-ghost"
                  type="button"
                  :disabled="busy === `key:${k.id}`"
                  @click="openEdit(k)"
                >
                  编辑
                </button>
                <button
                  class="btn btn-ghost btn-danger"
                  type="button"
                  :disabled="
                    busy === `key:${k.id}` || keyState(k) === 'revoked'
                  "
                  @click="revokeKey(k)"
                >
                  撤销
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>

  <!-- ---- renewals ---- -->
  <section class="card flex flex-col gap-3">
    <div class="flex flex-wrap items-center gap-2">
      <h2 class="m-0 text-lg font-semibold">续期申请</h2>
      <span v-if="!renewalsLoading" class="badge">{{ renewals.length }}</span>
    </div>

    <div
      v-if="renewalsLoading"
      class="px-5 py-12 text-center text-ink-muted"
      role="status"
    >
      <span class="spinner mr-2" aria-hidden="true"></span> 正在加载续期申请…
    </div>

    <template v-else-if="renewalsError">
      <div class="alert alert-error" role="alert">{{ renewalsError }}</div>
      <div>
        <button class="btn" type="button" @click="loadRenewals">重试</button>
      </div>
    </template>

    <p
      v-else-if="renewals.length === 0"
      class="m-0 px-5 py-12 text-center text-ink-muted"
    >
      暂无待处理的续期申请。
    </p>

    <div v-else class="overflow-x-auto">
      <table class="data-table">
        <caption class="sr-only">
          待处理的续期申请
        </caption>
        <thead>
          <tr>
            <th scope="col">密钥 ID</th>
            <th scope="col">申请时长</th>
            <th scope="col">申请时间</th>
            <th scope="col" class="text-right">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in renewals" :key="r.id">
            <td class="font-mono">{{ r.key_id }}</td>
            <td class="text-ink-muted">
              {{ formatRequestedHours(r.requested_hours) }}
            </td>
            <td class="text-ink-muted" :title="formatDateTime(r.created_at)">
              {{ relativeTime(r.created_at) }}
            </td>
            <td>
              <div class="flex items-center justify-end gap-1">
                <button
                  class="btn btn-primary btn-sm"
                  type="button"
                  :disabled="busy === `renewal:${r.id}`"
                  @click="openRenewal(r)"
                >
                  批准
                </button>
                <button
                  class="btn btn-ghost btn-danger"
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
    </div>
  </section>

  <!-- ---- approve application dialog ---- -->
  <div
    v-if="approveTarget"
    class="modal-backdrop"
    @click.self="approveTarget = null"
  >
    <form
      ref="dialogEl"
      class="card modal flex flex-col gap-3"
      role="dialog"
      aria-modal="true"
      aria-labelledby="approve-dialog-title"
      tabindex="-1"
      @submit.prevent="submitApprove"
    >
      <h2 id="approve-dialog-title">批准申请</h2>
      <p class="m-0 text-[13px] text-ink-muted">
        {{ approveTarget.label }} · 申请
        {{ formatRequestedHours(approveTarget.requested_hours) }}
      </p>
      <p class="m-0 text-xs text-ink-muted">
        批准不会立刻生成密钥：token 由 agent 的 claim secret 派生，只有 agent
        在领取窗口内自取时
        服务端才算得出来。领取成功后该密钥才会出现在下面的「密钥」列表里。
      </p>

      <div v-if="dialogError" class="alert alert-error" role="alert">
        {{ dialogError }}
      </div>

      <div>
        <label class="field-label" for="approve-name">密钥名称</label>
        <input
          id="approve-name"
          name="approve_name"
          autocomplete="off"
          v-model="approveForm.name"
          class="input"
          type="text"
          :disabled="busy !== null"
        />
      </div>
      <div>
        <label class="field-label" for="approve-hours"
          >有效期（小时，0 = 永不过期）</label
        >
        <input
          id="approve-hours"
          name="approve_hours"
          autocomplete="off"
          v-model.number="approveForm.hours"
          class="input"
          type="number"
          min="0"
          step="1"
          :disabled="busy !== null"
        />
        <p class="mt-1.5 mb-0 text-xs text-ink-muted">
          {{
            approveForm.hours === 0
              ? "0 = 永不过期，密钥将不再有到期时间。"
              : `到期时间：${approveForm.hours} 小时后`
          }}
        </p>
      </div>
      <div>
        <label class="field-label" for="approve-note">备注（可选）</label>
        <input
          id="approve-note"
          name="approve_note"
          autocomplete="off"
          v-model="approveForm.note"
          class="input"
          type="text"
          :disabled="busy !== null"
        />
      </div>

      <div class="flex justify-end gap-2">
        <button
          class="btn"
          type="button"
          :disabled="busy !== null"
          @click="approveTarget = null"
        >
          取消
        </button>
        <button class="btn btn-primary" type="submit" :disabled="busy !== null">
          <span v-if="busy !== null" class="spinner" aria-hidden="true"></span>
          批准
        </button>
      </div>
    </form>
  </div>

  <!-- ---- edit key dialog ---- -->
  <div v-if="editTarget" class="modal-backdrop" @click.self="editTarget = null">
    <form
      ref="dialogEl"
      class="card modal flex flex-col gap-3"
      role="dialog"
      aria-modal="true"
      aria-labelledby="edit-dialog-title"
      tabindex="-1"
      @submit.prevent="submitEdit"
    >
      <h2 id="edit-dialog-title">编辑密钥</h2>
      <p class="m-0 font-mono text-xs text-ink-muted">{{ editTarget.id }}</p>

      <div v-if="dialogError" class="alert alert-error" role="alert">
        {{ dialogError }}
      </div>

      <div>
        <label class="field-label" for="edit-name">名称</label>
        <input
          id="edit-name"
          name="edit_name"
          autocomplete="off"
          v-model="editForm.name"
          class="input"
          type="text"
          :disabled="busy !== null"
        />
      </div>
      <div>
        <label class="field-label" for="edit-hours"
          >有效期（小时，0 = 永不过期）</label
        >
        <input
          id="edit-hours"
          name="edit_hours"
          autocomplete="off"
          v-model.number="editForm.hours"
          class="input"
          type="number"
          min="0"
          step="1"
          :disabled="busy !== null"
        />
        <p class="mt-1.5 mb-0 text-xs text-ink-muted">
          {{
            editForm.hours === 0
              ? "0 = 永不过期；保存后立即生效（等于直接续期）。"
              : `保存后从此刻起重新计算：${editForm.hours} 小时后到期。`
          }}
        </p>
      </div>

      <div class="flex justify-end gap-2">
        <button
          class="btn"
          type="button"
          :disabled="busy !== null"
          @click="editTarget = null"
        >
          取消
        </button>
        <button class="btn btn-primary" type="submit" :disabled="busy !== null">
          <span v-if="busy !== null" class="spinner" aria-hidden="true"></span>
          保存
        </button>
      </div>
    </form>
  </div>

  <!-- ---- approve renewal dialog ---- -->
  <div
    v-if="renewalTarget"
    class="modal-backdrop"
    @click.self="renewalTarget = null"
  >
    <form
      ref="dialogEl"
      class="card modal flex flex-col gap-3"
      role="dialog"
      aria-modal="true"
      aria-labelledby="renewal-dialog-title"
      tabindex="-1"
      @submit.prevent="submitRenewal"
    >
      <h2 id="renewal-dialog-title">批准续期</h2>
      <p class="m-0 font-mono text-xs text-ink-muted">
        {{ renewalTarget.key_id }}
      </p>

      <div v-if="dialogError" class="alert alert-error" role="alert">
        {{ dialogError }}
      </div>

      <div>
        <label class="field-label" for="renewal-hours"
          >有效期（小时，0 = 永不过期）</label
        >
        <input
          id="renewal-hours"
          name="renewal_hours"
          autocomplete="off"
          v-model.number="renewalForm.hours"
          class="input"
          type="number"
          min="0"
          step="1"
          :disabled="busy !== null"
        />
        <p class="mt-1.5 mb-0 text-xs text-ink-muted">
          {{
            renewalForm.hours === 0
              ? "0 = 永不过期。"
              : `批准后到期时间：${renewalForm.hours} 小时后`
          }}
        </p>
      </div>

      <div class="flex justify-end gap-2">
        <button
          class="btn"
          type="button"
          :disabled="busy !== null"
          @click="renewalTarget = null"
        >
          取消
        </button>
        <button class="btn btn-primary" type="submit" :disabled="busy !== null">
          <span v-if="busy !== null" class="spinner" aria-hidden="true"></span>
          批准
        </button>
      </div>
    </form>
  </div>

  <!-- ---- one-step hand-off: generate, then forward ---- -->
  <div v-if="showHandoff" class="modal-backdrop">
    <form
      v-if="!handoff"
      ref="dialogEl"
      class="card modal flex flex-col gap-3"
      role="dialog"
      aria-modal="true"
      aria-labelledby="handoff-dialog-title"
      tabindex="-1"
      @submit.prevent="submitHandoff"
    >
      <h2 id="handoff-dialog-title">生成交接链接</h2>
      <p class="m-0 text-[13px] text-ink-muted">
        生成后把链接（或提示词）直接发给对方即可，<strong
          class="font-semibold text-ink"
          >你不需要再做任何确认</strong
        >。链接里的凭证 只能领取一次，领取时才会真正生成密钥。
      </p>
      <div v-if="dialogError" class="alert alert-error" role="alert">
        {{ dialogError }}
      </div>

      <div>
        <label class="field-label" for="handoff-label">收件人名称</label>
        <input
          id="handoff-label"
          name="handoff_label"
          autocomplete="off"
          v-model="handoffForm.label"
          class="input"
          type="text"
          placeholder="例如：A姐"
          maxlength="100"
        />
      </div>
      <div>
        <label class="field-label" for="handoff-hours"
          >密钥有效期（小时，0 = 永不过期）</label
        >
        <input
          id="handoff-hours"
          name="handoff_hours"
          autocomplete="off"
          v-model.number="handoffForm.hours"
          class="input"
          type="number"
          min="0"
          step="1"
        />
      </div>
      <div>
        <label class="field-label" for="handoff-note"
          >备注（可选，只有你能看到）</label
        >
        <input
          id="handoff-note"
          name="handoff_note"
          autocomplete="off"
          v-model="handoffForm.note"
          class="input"
          type="text"
          placeholder="例如：月报发布用"
          maxlength="500"
        />
      </div>

      <div class="flex justify-end gap-2">
        <button
          class="btn btn-primary"
          type="submit"
          :disabled="busy === 'handoff'"
        >
          {{ busy === "handoff" ? "生成中…" : "生成" }}
        </button>
        <button class="btn" type="button" @click="closeHandoff">取消</button>
      </div>
    </form>

    <div
      v-else
      ref="dialogEl"
      class="card modal flex flex-col gap-3"
      role="dialog"
      aria-modal="true"
      aria-labelledby="handoff-result-title"
      tabindex="-1"
    >
      <h2 id="handoff-result-title">交接链接已生成</h2>
      <div class="alert alert-ok" role="status">
        把下面任意一项发给「{{ handoff.label }}」。<strong class="font-semibold"
          >链接只能用一次</strong
        >，领取后就失效； 链接有效期至
        {{ formatExpiry(handoff.link_expires_at) }}。
      </div>
      <div v-if="dialogError" class="alert alert-error" role="alert">
        {{ dialogError }}
      </div>

      <div>
        <span class="field-label">转发链接（人打开后点「领取密钥」）</span>
        <div class="token-url">{{ handoff.url }}</div>
      </div>
      <button class="btn btn-primary" type="button" @click="copyHandoff('url')">
        {{ handoffCopied === "url" ? "✓ 已复制链接" : "复制链接" }}
      </button>

      <div class="mt-2">
        <label class="field-label" for="handoff-prompt"
          >提示词（直接发给 AI 助手用）</label
        >
        <textarea
          id="handoff-prompt"
          name="handoff_prompt"
          autocomplete="off"
          class="input font-mono text-[13px]"
          rows="6"
          readonly
          spellcheck="false"
          :value="handoff.prompt"
        ></textarea>
      </div>
      <button class="btn" type="button" @click="copyHandoff('prompt')">
        {{ handoffCopied === "prompt" ? "✓ 已复制提示词" : "复制提示词" }}
      </button>

      <p class="m-0 text-xs text-ink-muted">
        <span class="font-mono">#</span>
        后面那一段就是一次性凭证，等价于密码：只通过可信渠道转发。
        对方领取后，新密钥会出现在下面的密钥列表里。
      </p>

      <div class="flex justify-end gap-2">
        <button class="btn" type="button" @click="closeHandoff">完成</button>
      </div>
    </div>
  </div>

  <!-- ---- one-time plaintext token dialog ---- -->
  <!--
    No Escape-to-close and no backdrop click: the token below is shown once and
    is unrecoverable, so it must not be dismissible by a stray keypress or a
    mis-aimed click. Only the two explicit buttons close it.
  -->
  <div v-if="issued" class="modal-backdrop">
    <div
      ref="dialogEl"
      class="card modal flex flex-col gap-3"
      role="dialog"
      aria-modal="true"
      aria-labelledby="issued-dialog-title"
      tabindex="-1"
    >
      <h2 id="issued-dialog-title">密钥已生成</h2>

      <div class="alert alert-error" role="alert">
        明文密钥<strong class="font-semibold">只显示这一次</strong
        >，关闭此窗口后无法再查看，也无法找回。请立即复制并妥善保存。
      </div>
      <div v-if="dialogError" class="alert alert-error" role="alert">
        {{ dialogError }}
      </div>

      <div class="token-url">{{ issued.token }}</div>
      <p v-if="issued.key" class="m-0 text-[13px] text-ink-muted">
        名称：{{ issued.key.name || "（未命名）" }} · 前缀：{{
          issued.key.token_prefix || "—"
        }}<br />
        到期：{{ formatExpiry(issued.key.expires_at) }}
      </p>

      <div class="flex justify-end gap-2">
        <button class="btn btn-primary" type="button" @click="copyIssued">
          {{ issuedCopied ? "✓ 已复制" : "复制密钥" }}
        </button>
        <button class="btn" type="button" @click="closeIssued">
          我已保存，关闭
        </button>
      </div>
    </div>
  </div>
</template>
