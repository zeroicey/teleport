package views

import (
	"strconv"
	"strings"

	"github.com/zeroicey/teleport/backend/internal/markdown"
)

// HandoffState is how a hand-off link looks to the person opening it.
type HandoffState string

const (
	// HandoffOpen means the credential has not been collected yet.
	HandoffOpen HandoffState = "open"
	// HandoffClaimed means somebody already took the credential. It cannot be
	// shown again — only its hash was ever stored — so the page says so
	// instead of offering a button that is guaranteed to fail.
	HandoffClaimed HandoffState = "claimed"
	// HandoffExpired means the link ran out of time before it was used.
	HandoffExpired HandoffState = "expired"
	// HandoffUnknown covers an id that does not exist. The page is still
	// rendered (never a JSON error): the recipient followed a link and needs
	// an explanation, not an API envelope.
	HandoffUnknown HandoffState = "unknown"
)

// KeyHandoffOptions is everything the one-step hand-off page needs.
type KeyHandoffOptions struct {
	// ID is the application id from the link path.
	ID string
	// Label is who the credential is for, empty when the id is unknown.
	Label string
	// State decides which of the three panels the page shows.
	State HandoffState
	// ClaimDeadline is when the link stops being claimable, epoch ms.
	ClaimDeadline int64
	// LinkExpiresAt is the same instant as ClaimDeadline, exposed for the live
	// countdown element.
	LinkExpiresAt int64
	// AppBaseURL is the absolute application root, e.g. https://host/prefix —
	// the same value the share links and the guide are built from.
	//
	// Absolute rather than relative on purpose: the page lives at `/k/{id}`, so
	// a relative `api/...` would resolve under `/k/`. It is the *app* root and
	// not the API root so that the client composes `/api/...` itself; passing
	// the API root here produced `/api/api/...` and every claim 404'd (caught by
	// a real Chromium run).
	AppBaseURL string
	// AssetBaseURL is where the built frontend assets are served from.
	AssetBaseURL string
	// GuideURL is the agent-facing guide the recipient is pointed at.
	GuideURL string
	// KeyLifetime is the human-readable granted lifetime, e.g. "24 小时" or
	// "不过期". Empty when unknown.
	KeyLifetime string
}

// RenderKeyHandoffPage renders the page a one-step hand-off link opens.
//
// It is deliberately a *shell*: the claim secret is in the URL fragment, which
// the server never receives, so no credential can be shown here. Everything the
// page knows about the link's state comes from the application row, which holds
// no secret.
func RenderKeyHandoffPage(opts KeyHandoffOptions) string {
	assetBase := strings.TrimRight(opts.AssetBaseURL, "/")
	appBase := strings.TrimRight(opts.AppBaseURL, "/")

	claimable := opts.State == HandoffOpen
	// A literal "0"/"1" rather than presence/absence: the client reads this
	// attribute to decide whether a claim is even possible.
	spent := "0"
	if !claimable {
		spent = "1"
	}

	var b strings.Builder
	b.Grow(8192)

	b.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="robots" content="noindex, nofollow, noarchive" />
<meta name="referrer" content="no-referrer" />
<meta name="color-scheme" content="light dark" />
<title>领取 Teleport 发布密钥</title>
<link rel="stylesheet" href="` + markdown.Escape(assetBase) + `/fonts/fonts.css" />
<link rel="icon" type="image/svg+xml" href="` + markdown.Escape(assetBase) + `/favicon.svg" />
<style>
  :root {
    color-scheme: light dark;
    --fg:#18181b; --fg-muted:#71717a; --bg:#ffffff; --surface:#fafafa;
    --border:#e4e4e7; --accent:#2563eb; --code-bg:#f6f8fa; --danger:#b91c1c;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --fg:#e4e4e7; --fg-muted:#a1a1aa; --bg:#0f1115; --surface:#17171c;
      --border:#2a2a32; --accent:#60a5fa; --code-bg:#161b22; --danger:#fca5a5;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin:0; background:var(--bg); color:var(--fg);
    font:16px/1.75 "Outfit","Noto Sans SC",sans-serif;
    -webkit-text-size-adjust:100%;
  }
  .wrap { max-width: 720px; margin: 0 auto; padding: 48px 20px 96px; }
  .eyebrow { display:flex; gap:8px; align-items:center; flex-wrap:wrap; font-size:13px; color:var(--fg-muted); margin-bottom:14px; }
  .badge { border:1px solid var(--border); border-radius:999px; padding:2px 10px; font-size:12px; white-space:nowrap; }
  h1 { font-size:1.7rem; line-height:1.3; margin:0 0 16px; letter-spacing:-0.01em; }
  h2 { font-size:1.1rem; margin:2em 0 .6em; }
  p { margin:0 0 1em; }
  ol, ul { padding-left:1.4em; margin:0 0 1em; }
  li { margin:.35em 0; }
  a { color:var(--accent); }
  code, .mono { font-family:"JetBrains Mono",ui-monospace,monospace; font-size:.9em; }
  code { background:var(--code-bg); border:1px solid var(--border); border-radius:4px; padding:.1em .35em; }
  pre {
    background:var(--code-bg); border:1px solid var(--border); border-radius:8px;
    padding:14px 16px; overflow-x:auto; font-size:13px; line-height:1.6; margin:0 0 1.2em;
  }
  pre code { background:none; border:0; padding:0; }
  .card { border:1px solid var(--border); border-radius:10px; padding:20px 22px; background:var(--surface); margin:0 0 20px; }
  button {
    font:inherit; font-weight:600; color:#fff; background:var(--accent);
    border:0; border-radius:8px; padding:10px 20px; cursor:pointer;
  }
  button[disabled] { opacity:.6; cursor:default; }
  button.secondary { background:transparent; color:var(--accent); border:1px solid var(--border); font-weight:500; padding:6px 12px; }
  #handoff-status { margin:14px 0 0; font-size:14px; color:var(--fg-muted); }
  #handoff-status[data-kind="error"] { color:var(--danger); }
  #handoff-token {
    display:block; width:100%; margin:0 0 12px; word-break:break-all;
    font-size:13.5px; padding:12px 14px;
  }
  .hint { font-size:13px; color:var(--fg-muted); }
  footer { margin-top:56px; font-size:12px; color:var(--fg-muted); border-top:1px solid var(--border); padding-top:16px; }
</style>
</head>
<body>
<div class="wrap" data-handoff-id="`)
	b.WriteString(markdown.Escape(opts.ID))
	b.WriteString(`" data-app-base="`)
	b.WriteString(markdown.Escape(appBase))
	b.WriteString(`" data-handoff-spent="`)
	b.WriteString(spent)
	b.WriteString(`">
  <div class="eyebrow">
    <span class="badge">一次性领取</span>`)
	if opts.Label != "" {
		b.WriteString(`
    <span class="badge">给 `)
		b.WriteString(markdown.Escape(opts.Label))
		b.WriteString(`</span>`)
	}
	if opts.LinkExpiresAt > 0 {
		b.WriteString(`
    <span class="expiry" data-expires-at="`)
		b.WriteString(strconv.FormatInt(opts.LinkExpiresAt, 10))
		b.WriteString(`">计算中…</span>`)
	}
	b.WriteString(`
  </div>
  <h1>领取 Teleport 发布密钥</h1>
`)

	switch opts.State {
	case HandoffOpen:
		b.WriteString(`
  <p>这个链接可以换取一个发布密钥。密钥<b>只能领取一次</b>，领取后请立刻保存或直接交给你的 AI 助手。</p>

  <div class="card stack">
    <button id="handoff-claim" type="button" hidden>领取密钥</button>
    <p id="handoff-status">正在检查链接…</p>
    <div id="handoff-result" hidden>
      <h2>你的发布密钥</h2>
      <code id="handoff-token"></code>
      <button id="handoff-copy" class="secondary" type="button">复制</button>
      <p class="hint" style="margin-top:14px">把它当成密码：任何拿到它的人都能发布报告。
      如果它泄漏了，让管理员在面板里撤销。</p>
    </div>
  </div>
`)
	case HandoffClaimed:
		b.WriteString(`
  <p>这条链接的密钥<b>已经被领取过了</b>。密钥只发放一次，服务器上只保存它的哈希，所以这里无法再次显示。</p>
  <div class="card stack">
    <p id="handoff-status">如果你是刚才在同一个标签页里刷新了页面，密钥可能还能在这里找回来：</p>
    <div id="handoff-result" hidden>
      <code id="handoff-token"></code>
      <button id="handoff-copy" class="secondary" type="button">复制</button>
    </div>
  </div>
  <p>如果确实没有保存到，请让管理员在面板里重新生成一条交接链接（面板 → 密钥 → 生成交接链接）。</p>
`)
	case HandoffExpired:
		b.WriteString(`
  <p>这条链接已经<b>过期</b>了，里面的密钥没有被领取。</p>
  <p id="handoff-status">请让管理员在面板里重新生成一条交接链接（面板 → 密钥 → 生成交接链接）。过期是为了安全：链接一旦泄漏，窗口是有限的。</p>
`)
	default:
		b.WriteString(`
  <p>这个链接<b>无效</b>：它指向的申请不存在（可能被管理员撤销，或者链接被截断了）。</p>
  <p id="handoff-status">请让管理员把完整链接重发一次，注意 <code>#</code> 后面的那段也是链接的一部分。</p>
`)
	}

	if claimable {
		b.WriteString(`
  <h2>拿到密钥之后</h2>
  <ol>
    <li>读 <a href="`)
		b.WriteString(markdown.Escape(opts.GuideURL))
		b.WriteString(`">使用指南</a>（`)
		b.WriteString(markdown.Escape(opts.GuideURL))
		b.WriteString(`），里面有发布接口的完整说明。</li>
    <li>带 <code>Authorization: Bearer &lt;密钥&gt;</code> 调用 <code>POST `)
		b.WriteString(markdown.Escape(appBase))
		b.WriteString(`/api/reports</code> 就能发布报告，返回的分享链接可以直接发给别人看。</li>
  </ol>`)
		if opts.KeyLifetime != "" {
			b.WriteString(`
  <p class="hint">密钥有效期：`)
			b.WriteString(markdown.Escape(opts.KeyLifetime))
			b.WriteString(`。</p>`)
		}
		b.WriteString(`
  <h2>如果你的 AI 助手可以直接调用接口</h2>
  <p class="hint">领取后这里会显示一段可以直接粘贴给 AI 的说明（含接口地址与密钥）。</p>
  <pre id="handoff-prompt-wrap" hidden><code id="handoff-prompt"></code></pre>`)
	}

	b.WriteString(`
  <footer>
    <p>由 Teleport 生成 · 此页面不缓存、不被搜索引擎收录。</p>
  </footer>
</div>
<script type="module" src="`)
	b.WriteString(markdown.Escape(assetBase))
	b.WriteString(`/assets/share.js"></script>
</body>
</html>
`)
	return b.String()
}
