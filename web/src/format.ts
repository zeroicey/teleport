/**
 * Shared formatting helpers.
 *
 * The API stores epoch milliseconds (0 = never expires); rendering happens here
 * so both list and detail views stay consistent.
 */

export function formatDateTime(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  return new Date(ms).toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
}

/** Human-readable expiry, plus a coarse "time left" hint. */
export function formatExpiry(expiresAt: number): string {
  if (expiresAt === 0) return '永不过期';
  const remaining = expiresAt - Date.now();
  if (remaining <= 0) return '已过期';
  return `${formatDateTime(expiresAt)}（${formatDuration(remaining)}）`;
}

export function formatDuration(ms: number): string {
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 60) return `${Math.max(minutes, 1)} 分钟后`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时后`;
  return `${Math.floor(hours / 24)} 天后`;
}

export function expiryState(token: { expires_at: number; is_active: boolean }): 'active' | 'expired' | 'revoked' {
  if (!token.is_active) return 'revoked';
  if (token.expires_at !== 0 && Date.now() >= token.expires_at) return 'expired';
  return 'active';
}

export function relativeTime(ms: number): string {
  const diff = Date.now() - ms;
  if (diff < 60_000) return '刚刚';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;
  return `${Math.floor(diff / 86_400_000)} 天前`;
}

/**
 * Absolute share URL for copying to the clipboard.
 *
 * The share page is served by the Go backend under the application's route
 * prefix, so the URL is `<app base>/s/<token>` — NOT `<origin>/s/<token>`.
 * Using the bare origin here would produce a link that lands on the *other*
 * service sharing this host. `window.location` already includes the prefix (the
 * dashboard is served from under it), so stripping the dashboard path off the
 * current location is both correct and independent of how the app is mounted.
 *
 * `VITE_SHARE_BASE` overrides it, for pointing recipients at a different host
 * than the dashboard is served from. Trailing slashes are trimmed so the
 * configured base may be written with or without one.
 */
export function shareUrl(token: string): string {
  const configured = (import.meta.env.VITE_SHARE_BASE ?? '').replace(/\/+$/, '');
  if (configured) return `${configured}/s/${token}`;

  // `BASE_URL` is the route prefix with a trailing slash, e.g.
  // "/yeciorez/teleport/". Joining it to the current origin yields the public
  // share URL regardless of which route the dashboard is currently on.
  const base = import.meta.env.BASE_URL.replace(/\/+$/, '');
  return `${window.location.origin}${base}/s/${token}`;
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Clipboard API needs a secure context; fall back to a legacy selection.
    try {
      const area = document.createElement('textarea');
      area.value = text;
      area.style.position = 'fixed';
      area.style.opacity = '0';
      document.body.appendChild(area);
      area.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(area);
      return ok;
    } catch {
      return false;
    }
  }
}
