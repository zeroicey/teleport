// Package api wires the HTTP surface.
//
// The routing table is mounted under the configured route prefix so the exact
// same paths work both behind Caddy (api.hcyj.xyz/yeciorez/teleport/...) and
// directly against this process.
package api

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/aidoc"
	"github.com/zeroicey/teleport/backend/internal/config"
	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/httpx"
	"github.com/zeroicey/teleport/backend/internal/markdown"
	"github.com/zeroicey/teleport/backend/internal/password"
	"github.com/zeroicey/teleport/backend/internal/spa"
	"github.com/zeroicey/teleport/backend/internal/store"
	"github.com/zeroicey/teleport/backend/internal/views"
	"github.com/zeroicey/teleport/backend/internal/webui"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	cfg    *config.Config
	store  *store.Store
	render *markdown.Renderer
	// kDerive is the agent-key derivation subkey, computed once from the session
	// secret. Deriving it per request would be an HMAC on the hot path for no
	// benefit; keeping it here also means no handler has to reach for the root
	// secret directly.
	kDerive []byte
	// applyLimiter throttles the public application endpoint. In-process by
	// design: one binary, and a restart merely resets the window.
	applyLimiter *applyLimiter
}

// New builds the HTTP handler for the whole API.
//
// It returns an error rather than exiting so the caller controls startup
// failure: a misconfigured StaticDir should stop the process with a clear
// message, not silently serve an API with no frontend.
func New(cfg *config.Config, st *store.Store) (http.Handler, error) {
	s := &Server{
		cfg:          cfg,
		store:        st,
		render:       markdown.New(),
		kDerive:      agentkey.KDerive(cfg.SessionSecret),
		applyLimiter: newApplyLimiter(cfg.KeyApplyPerHour, time.Hour),
	}

	authCfg := httpx.AuthConfig{
		AgentSecretKey: cfg.AgentSecretKey,
		SessionSecret:  cfg.SessionSecret,
		CookieSecure:   cfg.CookieSecure,
		CookiePath:     cfg.CookiePath,
		SessionTTL:     cfg.SessionTTL,
	}
	// The resolver is what makes a named key in agent_keys usable at all: the
	// middleware hashes the bearer token and asks the store whether that hash is
	// a live key. Root stays a property of the environment credential and never
	// passes through here.
	//
	// The grace window is passed into the resolver rather than applied in a
	// route: it decides whether an expired key is *reported at all*, and the
	// middleware decides whether that report is admitted. Only the renewal route
	// uses the admitting middleware, so every other endpoint stays strict
	// without anyone having to remember to keep it strict.
	keyAuth := keyResolver{store: st, graceMS: int64(cfg.KeyRenewalGrace / time.Millisecond)}
	requireAgent := httpx.GinMiddleware(httpx.RequireAgent(authCfg, keyAuth))
	// Used by exactly one route: filing a renewal request. An expired key may ask
	// a human for more time and may do nothing else.
	requireAgentRenewal := httpx.GinMiddleware(httpx.RequireAgentAllowExpired(authCfg, keyAuth))
	requireSession := httpx.GinMiddleware(httpx.RequireSession(authCfg))

	// Gin, not gin.Default(): the default engine installs Gin's logger and its
	// recovery middleware, and both are wrong here. Access logging is LogRequests
	// in the outer chain (one line per request, carrying our request id), and
	// recovery is httpx.Recover, which turns a panic into our JSON 500 and logs it
	// exactly once. Gin's defaults would duplicate both, and its logger would
	// write a second, differently-shaped line per request.
	engine := gin.New()

	// Gin's own conveniences are all switched OFF, because each one changes the
	// wire in a way the frozen tests do not see.
	//
	//   - RedirectTrailingSlash / RedirectFixedPath: Gin answers an unclean or
	//     trailing-slash-mismatched path with a 301. ServeMux also redirected such
	//     paths, but with a **307** — and the difference matters, because a 301 on
	//     a POST is how a POST silently becomes a GET. Rather than accept Gin's 301,
	//     the same redirect is reproduced with the correct status in NoRoute below
	//     (see serveMuxRedirect).
	//   - HandleMethodNotAllowed: Gin would answer a known path with the wrong
	//     method with a 405. ServeMux never did here, because the per-prefix
	//     catch-alls matched every method first — so a wrong verb is a 404 with the
	//     JSON envelope, which is what TestMethodNotAllowed asserts.
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	engine.HandleMethodNotAllowed = false

	p := cfg.RoutePrefix

	// route registers one middleware+handler chain for each of the given methods.
	// Middleware comes first, the handler last.
	route := func(path string, methods []string, chain ...gin.HandlerFunc) {
		for _, m := range methods {
			engine.Handle(m, path, chain...)
		}
	}
	// get registers a GET route and answers HEAD with the same chain, because
	// that is what net/http.ServeMux does and this codebase depends on it.
	//
	// The dependency is security-relevant: HEAD on the claim endpoint must reach
	// the handler so it can answer 405. A HEAD has no body to carry the credential
	// back, so letting it through would burn a one-time hand-off link. Gin does
	// not route HEAD to a GET handler, so without this the guard in
	// handlePollKeyApplication would become dead code and the frozen 405 would
	// silently become a 404.
	get := func(path string, chain ...gin.HandlerFunc) {
		route(path, []string{http.MethodGet, http.MethodHead}, chain...)
	}

	// -- public ---------------------------------------------------------------
	get(p+"/api/health", s.handleHealth)
	get(p+"/api/share/:token", s.handleReadShare)
	get(p+"/s/:token", s.handleSharePage)
	// The body of a `format: "html"` report, framed by the share page above.
	// Registered explicitly because unmatched /s/ paths are a JSON 404: an
	// unregistered sub-path would answer the iframe with JSON.
	get(p+"/s/:token/html", s.handleShareHTML)

	// The one-step key hand-off page. Public by design: the link is forwarded to
	// whoever should get the credential, and the thing that authorises the claim
	// is the fragment, which the server never sees.
	//
	// The bare /k/ is registered too, so a truncated link renders the "this link
	// is incomplete" page instead of falling through to the SPA shell.
	get(p+"/k/:id", s.handleKeyHandoffPage)
	get(p+"/k/", s.handleKeyHandoffPage)

	// -- machine-readable usage guide -----------------------------------------
	//
	// Served by this process, not the SPA, because an agent fetching a URL gets
	// only what the server sends: the SPA renders client-side and would hand back
	// an empty shell.
	//
	// These sit at stable paths rather than behind a share link on purpose. A
	// share link expires, so a guide distributed as one would eventually 410 on
	// every agent that had cached it — the opposite of what this is for.
	get(p+"/ai", s.handleAIGuideHTML)
	get(p+"/ai.md", s.handleAIGuideMarkdown)
	get(p+"/llms.txt", s.handleLLMsTxt)

	// -- agent ----------------------------------------------------------------
	route(p+"/api/reports", []string{http.MethodPost}, requireAgent, s.handleCreateReport)
	get(p+"/api/reports", requireAgent, s.handleListOwnReports)
	get(p+"/api/reports/:id", requireAgent, s.handleGetReport)
	route(p+"/api/reports/:id", []string{http.MethodPatch}, requireAgent, s.handleUpdateReport)
	route(p+"/api/reports/:id", []string{http.MethodDelete}, requireAgent, s.handleDeleteReport)
	route(p+"/api/share/:token/revoke", []string{http.MethodPost}, requireAgent, s.handleRevoke)

	// Key self-service. The application and renewal poll endpoints are their own
	// public surface, protected by the claim secret in X-Teleport-Claim rather
	// than a bearer token — an agent has no credential yet when it applies.
	//
	// POST applications is the one write an anonymous caller may make, so it is
	// the one route behind the per-IP limiter.
	route(p+"/api/agent-keys/applications", []string{http.MethodPost},
		s.limitKeyApplications(), s.handleCreateKeyApplication)
	get(p+"/api/agent-keys/applications/:id", s.handlePollKeyApplication)
	get(p+"/api/agent-keys/renewals/:id", s.handlePollRenewal)

	get(p+"/api/agent-keys/me", requireAgent, s.handleKeyMe)
	// The one route that admits an expired key, so that letting a key lapse is
	// recoverable by the agent itself instead of orphaning every report it ever
	// published. Everything it can reach is a *request*; a human still decides.
	route(p+"/api/agent-keys/renewals", []string{http.MethodPost},
		requireAgentRenewal, s.handleCreateRenewal)

	// -- dashboard ------------------------------------------------------------
	route(p+"/api/admin/login", []string{http.MethodPost}, s.handleLogin)
	route(p+"/api/admin/logout", []string{http.MethodPost}, s.handleLogout)
	get(p+"/api/admin/session", requireSession, s.handleSession)
	get(p+"/api/admin/reports", requireSession, s.handleListReports)
	get(p+"/api/admin/reports/:id", requireSession, s.handleAdminGetReport)
	route(p+"/api/admin/reports/:id", []string{http.MethodPatch}, requireSession, s.handleAdminUpdateReport)
	route(p+"/api/admin/reports/:id", []string{http.MethodDelete}, requireSession, s.handleAdminDeleteReport)
	route(p+"/api/admin/reports/:id/shares", []string{http.MethodPost}, requireSession, s.handleCreateShare)
	route(p+"/api/admin/shares/:token", []string{http.MethodPatch}, requireSession, s.handlePatchShare)
	route(p+"/api/admin/shares/:token", []string{http.MethodDelete}, requireSession, s.handleDeleteShare)

	// Key administration. Every one of these is Session-gated: a human manages all
	// keys, and no agent credential reaches this surface at all.
	get(p+"/api/admin/keys", requireSession, s.handleAdminListKeys)
	route(p+"/api/admin/keys", []string{http.MethodPost}, requireSession, s.handleAdminCreateKey)
	// One-step issuance: the administrator's decision is made here, when the link
	// is generated, so the recipient never waits for a second action.
	route(p+"/api/admin/key-handoffs", []string{http.MethodPost}, requireSession, s.handleCreateKeyHandoff)
	route(p+"/api/admin/keys/:id", []string{http.MethodPatch}, requireSession, s.handleAdminPatchKey)
	get(p+"/api/admin/key-applications", requireSession, s.handleAdminListKeyApplications)
	route(p+"/api/admin/key-applications/:id/approve", []string{http.MethodPost},
		requireSession, s.handleAdminApproveKeyApplication)
	route(p+"/api/admin/key-applications/:id/reject", []string{http.MethodPost},
		requireSession, s.handleAdminRejectKeyApplication)
	get(p+"/api/admin/key-renewals", requireSession, s.handleAdminListRenewals)
	route(p+"/api/admin/key-renewals/:id/approve", []string{http.MethodPost},
		requireSession, s.handleAdminApproveRenewal)
	route(p+"/api/admin/key-renewals/:id/reject", []string{http.MethodPost},
		requireSession, s.handleAdminRejectRenewal)

	// Serve the frontend from this same process when one is available. It is
	// mounted as the unmatched-path fallback, so the SPA shell, its hashed assets
	// and the API all share one origin — which is what makes the session cookie
	// first-party and keeps the share page's `script-src 'self'` meaningful.
	//
	// Two sources, in order of preference:
	//
	//   1. Assets embedded in the binary (the normal deployment: one file to copy,
	//      and the assets can never drift from the code).
	//   2. STATIC_DIR on disk, which makes `pnpm run build` + restart enough to
	//      ship a frontend change without recompiling.
	build, buildSource, err := resolveFrontend(cfg)
	if err != nil {
		return nil, err
	}
	var static http.Handler
	if build != nil {
		static, err = spa.New(build, p, buildSource)
		if err != nil {
			return nil, fmt.Errorf("frontend build at %s is unusable: %w", buildSource, err)
		}
		slog.Info("serving frontend", "source", buildSource, "prefix", p)
	} else {
		slog.Warn("no frontend build available; serving API only")
	}

	// Everything no route matched lands here — including a known path with an
	// unregistered method, since Gin's 405 is switched off above. That is exactly
	// what the old per-prefix catch-alls did, and it is what keeps an unknown
	// /api/... path a JSON 404 instead of the SPA's HTML.
	engine.NoRoute(func(c *gin.Context) {
		w, r := c.Writer, c.Request
		path := r.URL.Path

		// The two redirects ServeMux used to perform, reproduced exactly.
		// Without this a request to the *documented* entry URL — the bare prefix,
		// which is what Caddy's `handle /yeciorez/teleport*` matches and what a
		// human types — answers a JSON 404 instead of redirecting to the panel.
		if serveMuxRedirect(c, p) {
			return
		}

		// The one subtree pattern ServeMux had that a Gin route cannot express
		// alongside its :id sibling: /k/ matched every path beneath it, so a link
		// truncated to /k/a/b still rendered the friendly "incomplete link" page
		// rather than a 404. The handler reads an empty id there, exactly as the
		// old {id}-less subtree pattern did.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			strings.HasPrefix(path, p+"/k/") {
			s.handleKeyHandoffPage(c)
			c.Abort()
			return
		}

		// API and share paths keep the JSON envelope even when the SPA owns the
		// prefix root. Without this an unknown /api/... path is answered with the
		// SPA shell, which is worse than it sounds: an XHR caller doing
		// response.json() on HTML fails with a parse error and reports "bad
		// response" instead of the actual 404.
		if isEnvelopePath(path, p) {
			httpx.Fail(w, r, http.StatusNotFound, "not_found", "Not found", nil)
			return
		}
		if static != nil && strings.HasPrefix(path, p+"/") {
			static.ServeHTTP(w, r)
			return
		}
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "Not found", nil)
	})

	return httpx.Chain(engine,
		httpx.Recover,
		httpx.RequestID,
		httpx.LogRequests,
		httpx.SecurityHeaders,
		httpx.CORS(cfg.FrontendOrigins),
		// Before the engine, because routing is exactly what must not see it:
		// Gin decodes %2F into a path separator while ServeMux did not, so
		// /api%2Fhealth used to 404 and would otherwise now match /api/health —
		// a refusal turned into a success.
		rejectEncodedSlashes,
		// Innermost, so it sees the final Content-Type before deciding. It only
		// buffers compressible responses; everything else streams through.
		httpx.Compress,
	), nil
}

// subtreePatterns are the prefix subtree (trailing-slash) patterns this router
// registers, i.e. the paths ServeMux matched as "this path and everything under
// it". They are listed explicitly because that is what ServeMux's redirect rule
// needs: a request for the *parent* of a subtree pattern was redirected to the
// subtree form.
//
// Note /k/ is here even though its Gin route is exact — NoRoute handles the
// subtree case, and the redirect for the bare /k must still happen.
func subtreePatterns(prefix string) []string {
	return []string{prefix + "/", prefix + "/api/", prefix + "/s/", prefix + "/k/"}
}

// serveMuxRedirect performs the redirect net/http.ServeMux used to perform, and
// reports whether it wrote one. Two rules, both of which ServeMux applied and
// neither of which is obvious:
//
//  1. An unclean path (doubled slashes, "." or "..", or a missing leading slash)
//     is redirected to its cleaned form. Measured on the pre-Gin implementation:
//     {prefix}//api/health and {prefix}/api/../api/health both answered
//     **307** with Location set to the cleaned path.
//  2. A path that is not itself registered but is the parent of a registered
//     subtree pattern — the bare prefix, {prefix}/api, {prefix}/s, {prefix}/k —
//     is redirected to the trailing-slash form. This is why the documented entry
//     URL worked: {prefix} redirected to {prefix}/ and served the panel.
//
// The status is 307, not 301, because ServeMux used 307 and the difference is
// load-bearing: 307 preserves the method, so a POST to the bare prefix stays a
// POST. (Gin's own RedirectTrailingSlash emits a 301, which is why it is off.)
func serveMuxRedirect(c *gin.Context, prefix string) bool {
	r := c.Request
	path := r.URL.Path

	if cleaned := cleanPath(path); cleaned != path {
		redirect307(c, cleaned)
		return true
	}
	for _, sub := range subtreePatterns(prefix) {
		if parent := strings.TrimSuffix(sub, "/"); path == parent {
			redirect307(c, sub)
			return true
		}
	}
	return false
}

// redirect307 emits the redirect ServeMux emitted, query string and all.
//
// It defers to http.Redirect rather than writing Location by hand, because that is
// exactly what ServeMux does: server.go's findHandler returns
// `RedirectHandler(u.String(), StatusTemporaryRedirect)`, and RedirectHandler calls
// http.Redirect. Writing the header directly looked equivalent and was not — it
// dropped the query string and the Content-Type/body.
//
// Both losses were measured against the pre-Gin implementation:
//
//	GET {p}?a=1&b=2        -> 307 Location "{p}/?a=1&b=2"  Content-Type text/html  body 67B
//	GET {p}//api/health?x=9 -> 307 Location "{p}/api/health?x=9"
//
// The query matters: {p}?a=1 is a plausible SPA deep link, and a redirect that
// silently discards it lands the user on the wrong view. The body matters less, but
// reproducing it is free once http.Redirect is doing the writing.
func redirect307(c *gin.Context, target string) {
	// http.Redirect escapes non-ASCII in the value it sets, so build the full
	// target — path *and* query — the same way ServeMux's urlFromEscaped did.
	u := urlFromEscaped(target, c.Request.URL.RawQuery)
	http.Redirect(c.Writer, c.Request, u, http.StatusTemporaryRedirect)
	c.Abort()
}

// urlFromEscaped mirrors net/http's own helper: it parses an already-escaped path
// and attaches the raw query verbatim, falling back to a path-only URL.
func urlFromEscaped(escapedPath, rawQuery string) string {
	u, err := url.ParseRequestURI(escapedPath)
	if err != nil {
		u = &url.URL{Path: escapedPath}
	}
	u.RawQuery = rawQuery
	return u.String()
}

// cleanPath is net/http's own path cleaner, reproduced so the redirect target is
// byte-identical to the one ServeMux computed. It differs from path.Clean in one
// way that matters: a trailing slash is preserved, so {prefix}/api/ is not
// rewritten to {prefix}/api.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes the trailing slash except for the root; put it back.
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

// rejectEncodedSlashes answers 404 to any request whose escaped path contains an
// encoded slash, so routing never sees one.
//
// It exists because Gin's radix tree matches on the **decoded** path while
// net/http.ServeMux matched on the escaped one: /yeciorez/teleport/api%2Fhealth
// answered 404 before the port and would answer 200 after it, turning a refusal
// into a success. That is the wrong direction to be wrong in, and it also means
// the router and anything reading the URL (Caddy's `handle /yeciorez/teleport*`,
// a future audit rule) would disagree about what path this request has.
//
// It is deliberately blunt: no legitimate client of this service sends %2F —
// share tokens and ids are URL-safe base64 or UUIDs, and neither contains a
// slash.
func rejectEncodedSlashes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") {
			httpx.Fail(w, r, http.StatusNotFound, "not_found", "Not found", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isEnvelopePath reports whether an unmatched path must still be answered with
// the JSON envelope rather than the SPA shell.
//
// The API and the share links are machine-read: a caller that gets HTML where it
// expected JSON reports a parse error instead of the real 404. Everything else
// under the prefix belongs to the SPA, which handles its own client-side routes.
func isEnvelopePath(path, prefix string) bool {
	for _, sub := range []string{"/api", "/s"} {
		if path == prefix+sub || strings.HasPrefix(path, prefix+sub+"/") {
			return true
		}
	}
	return false
}

// resolveFrontend picks the frontend build to serve, returning a nil fs.FS when
// there is none. Preferring the embedded copy means a binary built for release
// is self-contained; the STATIC_DIR fallback exists so a frontend-only change
// can ship without a recompile, and so API-only development needs no build.
func resolveFrontend(cfg *config.Config) (fs.FS, string, error) {
	if fsys, ok := webui.FS(); ok {
		return fsys, "embedded", nil
	}
	if cfg.StaticDir != "" {
		return os.DirFS(cfg.StaticDir), cfg.StaticDir, nil
	}
	return nil, "", nil
}

// ---------------------------------------------------------------------------
// public
// ---------------------------------------------------------------------------

// handleAIGuideMarkdown serves the usage guide as raw Markdown.
//
// This is the primary entry point for agents: plain text, no HTML to strip, and
// safe to hand straight to a model.
func (s *Server) handleAIGuideMarkdown(c *gin.Context) {
	// This handler never touches the request, so it binds only the writer.
	w := c.Writer
	body := aidoc.Markdown(s.aiFacts())
	w.Header().Set("Content-Type", aidoc.ContentTypeMarkdown)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The guide is regenerated from config on every request and is tiny, but it
	// is also a contract: a stale cached copy would describe an old prefix.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// handleAIGuideHTML serves the same guide as a readable, script-free page.
func (s *Server) handleAIGuideHTML(c *gin.Context) {
	// This handler never touches the request, so it binds only the writer.
	w := c.Writer
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", aidoc.CSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, aidoc.HTML(s.aiFacts()))
}

// handleLLMsTxt serves the discovery index, so an agent that knows the
// `llms.txt` convention can find the guide without being told the exact path.
//
// NOTE: this is mounted under ROUTE_PREFIX, not at the origin root. The root of
// this host belongs to a different service that shares api.hcyj.xyz, and
// claiming /llms.txt there would hijack another application's path.
func (s *Server) handleLLMsTxt(c *gin.Context) {
	// This handler never touches the request, so it binds only the writer.
	w := c.Writer
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, aidoc.LLMSTxt(s.aiFacts()))
}

func (s *Server) aiFacts() aidoc.Facts {
	return aidoc.FactsFrom(s.cfg)
}

func (s *Server) handleHealth(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	status := "ok"
	code := http.StatusOK
	// A cheap liveness probe for the database. A failure is reported as 503 so
	// the systemd/Caddy health checks can act on it.
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		status = "degraded"
		code = http.StatusServiceUnavailable
		httpx.Fail(w, r, code, "unavailable", "Database unavailable", nil)
		return
	}
	httpx.OK(w, r, map[string]any{
		"status":      status,
		"environment": s.cfg.Environment,
		"time":        store.NowMS(),
	}, code)
}

func (s *Server) handleReadShare(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	token, err := s.shareToken(c.Param("token"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	resolution, err := s.store.ResolveShare(token)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to resolve share link").WithCause(err))
		return
	}
	switch resolution.Status {
	case domain.StatusMissing:
		httpx.WriteError(w, r, httpx.NotFound("Report or share link not found"))
		return
	case domain.StatusGone:
		httpx.WriteError(w, r, httpx.Gone("This share link has expired"))
		return
	}

	// Analytics must not be able to fail the request.
	_ = s.store.RecordView(token)

	httpx.OK(w, r, sharePayload(resolution))
}

// handleKeyHandoffPage serves the page a one-step hand-off link opens.
//
// It is a page rather than a JSON endpoint because the link is meant to be
// forwarded to a person. The claim secret is in the URL fragment, which never
// reaches this handler — the page only reports the link's *state* from the
// application row, which stores no secret, and the exchange itself happens in
// the browser (see setupKeyHandoff in web/src/share/main.ts).
//
// Unknown, claimed and expired links all render 200 with an explanation, and
// returning an error envelope would show a person an API message for a normal,
// expected outcome (a link used twice).
//
// What this page discloses is deliberately *more* than the API does. The claim
// endpoint folds "claimed" into 404 so a guessed id tells an attacker nothing;
// this page, by contrast, tells the holder of an id whether the link is still
// claimable, when it lapses, and the label the administrator typed — because
// that is exactly what the person the link was forwarded to needs to read (an
// id alone is not the capability; the fragment is). The bound is the id itself,
// a 122-bit UUID that is never enumerable, so this is an accepted disclosure
// rather than an oversight. It is pinned by TestKeyHandoffPageStateVisibility:
// dropping the distinction is a product decision, not a cleanup.
//
// The clearing exception is the secret: nothing on this page can show, derive or
// be made to reveal the fragment.
func (s *Server) handleKeyHandoffPage(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	id := c.Param("id")
	nowMS := store.NowMS()
	s.expireApplications(nowMS)

	opts := views.KeyHandoffOptions{
		ID:           id,
		State:        views.HandoffUnknown,
		AppBaseURL:   s.cfg.AppBaseURL(),
		AssetBaseURL: s.cfg.AssetBaseURL,
		GuideURL:     s.cfg.AppBaseURL() + "/ai.md",
	}

	app, err := s.store.GetApplication(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load key handoff").WithCause(err))
		return
	}
	if app != nil {
		opts.Label = app.Label
		opts.LinkExpiresAt = app.ClaimDeadline
		opts.KeyLifetime = humanizeHours(app.ApprovedHours)
		switch app.Status {
		case domain.ApplicationApproved:
			opts.State = views.HandoffOpen
		case domain.ApplicationExpired:
			opts.State = views.HandoffExpired
		case domain.ApplicationClaimed:
			opts.State = views.HandoffClaimed
		default:
			// Pending (never happens for a hand-off) and rejected both mean
			// the link cannot produce a credential.
			opts.State = views.HandoffUnknown
		}
	}

	// Same policy as the share page: nothing here should outlive its window or
	// be indexed.
	h := w.Header()
	h.Set("Cache-Control", "no-store, must-revalidate")
	h.Set("Content-Security-Policy", views.ContentSecurityPolicy)
	h.Set("Content-Type", "text/html; charset=utf-8")

	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, views.RenderKeyHandoffPage(opts))
}

// humanizeHours renders a granted lifetime the way the hand-off page words it.
func humanizeHours(hours float64) string {
	if hours == 0 {
		return "不过期"
	}
	return strconv.FormatFloat(hours, 'f', -1, 64) + " 小时"
}

func (s *Server) handleSharePage(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	token, err := s.shareToken(c.Param("token"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	resolution, err := s.store.ResolveShare(token)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to resolve share link").WithCause(err))
		return
	}
	if resolution.Status == domain.StatusMissing {
		httpx.WriteError(w, r, httpx.NotFound("Share link not found"))
		return
	}
	if resolution.Status == domain.StatusGone {
		httpx.WriteError(w, r, httpx.Gone("This share link has expired"))
		return
	}

	_ = s.store.RecordView(token)

	// Never cache a page whose lifetime is deliberately bounded.
	//
	// The policy lives in views, next to the HTML it constrains, so the two
	// cannot be edited independently.
	h := w.Header()
	h.Set("Cache-Control", "no-store, must-revalidate")
	h.Set("Content-Security-Policy", views.ContentSecurityPolicy)
	h.Set("Content-Type", "text/html; charset=utf-8")

	page := renderSharePage(s, resolution)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, page)
}

// handleShareHTML serves the body of a `format: "html"` report verbatim, for
// the share page's iframe (and for the "open on its own" link).
//
// Resolution semantics are identical to handleSharePage — unknown and revoked
// are 404, expired is 410 — because this is the same capability, just a
// different representation of it. A markdown report has no such
// representation, so it is 404 here as well rather than inventing a new state.
//
// Two deliberate differences from handleSharePage:
//
//  1. No view is recorded. The outer page owns the counter; counting the frame
//     as well would double every visit.
//  2. `X-Frame-Options: DENY` (set for every response by httpx.SecurityHeaders)
//     is removed, since the one legitimate consumer of this URL is a frame.
//     The CSP's `frame-ancestors 'self'` below is the replacement restriction —
//     same-origin frames only, which is exactly what DENY would have prevented.
func (s *Server) handleShareHTML(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	token, err := s.shareToken(c.Param("token"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	resolution, err := s.store.ResolveShare(token)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to resolve share link").WithCause(err))
		return
	}
	if resolution.Status == domain.StatusMissing {
		httpx.WriteError(w, r, httpx.NotFound("Share link not found"))
		return
	}
	if resolution.Status == domain.StatusGone {
		httpx.WriteError(w, r, httpx.Gone("This share link has expired"))
		return
	}
	if resolution.Report.Format != domain.FormatHTML {
		httpx.WriteError(w, r, httpx.NotFound("Share link not found"))
		return
	}

	h := w.Header()
	h.Del("X-Frame-Options")
	h.Set("Content-Security-Policy", views.HTMLReportContentSecurityPolicy)
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Same reasoning as the share page: a bounded-lifetime document must not be
	// cached anywhere.
	h.Set("Cache-Control", "no-store, must-revalidate")

	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, resolution.Report.Content)
}

// ---------------------------------------------------------------------------
// agent
// ---------------------------------------------------------------------------

func (s *Server) handleCreateReport(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	raw, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxContentBytes+1<<16))
	if err != nil {
		httpx.WriteError(w, r, httpx.BadRequest("Could not read request body", nil))
		return
	}
	body, err := validateJSON(raw)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	input, err := parseCreateReport(body, s.cfg.MaxContentBytes)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Authorship is taken from the authenticated principal, never from the body:
	// a caller must not be able to claim a report it did not publish. Root has
	// no key row, so its reports store "" as the owner and only root and the
	// dashboard may read them.
	if principal := httpx.PrincipalFrom(r.Context()); principal != nil {
		input.OwnerKeyID = principal.KeyID
	}

	report, share, err := s.store.CreateReport(input, s.cfg.DefaultShareHours)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to create report").WithCause(err))
		return
	}

	var shareBody any
	if share != nil {
		shareBody = map[string]any{
			"token":      share.Token,
			"expires_at": share.ExpiresAt,
			"is_active":  share.IsActive,
			"view_count": share.ViewCount,
			"created_at": share.CreatedAt,
			"url":        s.cfg.ShareURL(share.Token),
		}
	}

	httpx.OK(w, r, map[string]any{
		"id":         report.ID,
		"title":      report.Title,
		"category":   report.Category,
		"format":     report.Format,
		"created_at": report.CreatedAt,
		"updated_at": report.UpdatedAt,
		"share":      shareBody,
	}, http.StatusCreated)
}

func (s *Server) handleGetReport(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	report, err := s.store.GetReport(c.Param("id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load report").WithCause(err))
		return
	}
	if report == nil {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}
	// Ownership is the permission model: a key may read only what it published.
	// "Not yours" answers 404, not 403, so a caller cannot confirm that a report
	// it is not allowed to see exists — the same rule unknown share tokens use.
	if !ownerCanAccess(httpx.PrincipalFrom(r.Context()), report.OwnerKeyID) {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}
	httpx.OK(w, r, report)
}

func (s *Server) handleRevoke(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	token, err := s.shareToken(c.Param("token"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Resolve the owning report before mutating anything: revoking someone
	// else's link must be indistinguishable from revoking a link that does not
	// exist, so the ownership test has to happen before the write.
	owner, _, found, err := s.store.ShareTokenOwner(token)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to resolve share token").WithCause(err))
		return
	}
	if !found {
		httpx.WriteError(w, r, httpx.NotFound("Share token not found"))
		return
	}
	if !ownerCanAccess(httpx.PrincipalFrom(r.Context()), owner) {
		httpx.WriteError(w, r, httpx.NotFound("Share token not found"))
		return
	}

	found, err = s.store.RevokeToken(token)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to revoke share token").WithCause(err))
		return
	}
	if !found {
		httpx.WriteError(w, r, httpx.NotFound("Share token not found"))
		return
	}
	httpx.OK(w, r, map[string]any{"token": token, "is_active": false})
}

// ---------------------------------------------------------------------------
// update / delete
// ---------------------------------------------------------------------------

// applyReportPatch is the shared body of the agent and dashboard PATCH routes.
//
// The two routes differ only in who may touch the report, which is decided by
// the caller-supplied authorise function. Everything else — parsing, the
// ownership check happening before the write, the 404-not-403 rule, and the
// response shape — lives here once, so the dashboard cannot drift into being
// more permissive or more talkative than the agent API.
func (s *Server) applyReportPatch(w http.ResponseWriter, r *http.Request, id string, authorise func(*domain.Report) bool) {

	raw, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxContentBytes+1<<16))
	if err != nil {
		httpx.WriteError(w, r, httpx.BadRequest("Could not read request body", nil))
		return
	}
	body, err := validateJSON(raw)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	patch, err := parseUpdateReport(body, s.cfg.MaxContentBytes)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Resolve and authorise BEFORE writing. Doing it after would mean the write
	// already happened for a caller who turns out not to be allowed, and any
	// "0 rows changed" answer would have to double as both "no such report" and
	// "not yours" — the two must stay indistinguishable, so the decision has to
	// be made on a read that can return 404 for both.
	existing, err := s.store.GetReport(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load report").WithCause(err))
		return
	}
	if existing == nil || !authorise(existing) {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}

	report, found, err := s.store.UpdateReport(id, patch, time.Now().UnixMilli())
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to update report").WithCause(err))
		return
	}
	if !found {
		// Deleted between the check and the write. Report it as absent rather
		// than resurrecting a success for a row that no longer exists.
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}
	httpx.OK(w, r, report)
}

// applyReportDelete is the shared body of the agent and dashboard DELETE routes.
func (s *Server) applyReportDelete(w http.ResponseWriter, r *http.Request, id string, authorise func(*domain.Report) bool) {

	existing, err := s.store.GetReport(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load report").WithCause(err))
		return
	}
	// Same rule as PATCH and as every other ownership check in this package: a
	// caller who is not allowed to see the report must not be able to tell it
	// apart from one that does not exist.
	if existing == nil || !authorise(existing) {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}

	found, err := s.store.DeleteReport(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to delete report").WithCause(err))
		return
	}
	if !found {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}
	// The response reports what actually happened to the links, so a client can
	// say "and its N links are dead now" without guessing.
	httpx.OK(w, r, map[string]any{
		"id":      id,
		"deleted": true,
	})
}

func (s *Server) handleUpdateReport(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	principal := httpx.PrincipalFrom(r.Context())
	s.applyReportPatch(w, r, c.Param("id"), func(report *domain.Report) bool {
		return ownerCanAccess(principal, report.OwnerKeyID)
	})
}

func (s *Server) handleDeleteReport(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	principal := httpx.PrincipalFrom(r.Context())
	s.applyReportDelete(w, r, c.Param("id"), func(report *domain.Report) bool {
		return ownerCanAccess(principal, report.OwnerKeyID)
	})
}

// The dashboard routes deliberately apply no ownership predicate: the dashboard
// is already authenticated as the human owner of the deployment, and
// ownerCanAccess grants root (and therefore a session, which carries none of
// the agent principal's fields) access to everything. Passing "always true"
// keeps that in one visible place.
func (s *Server) handleAdminUpdateReport(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	s.applyReportPatch(w, r, c.Param("id"), func(*domain.Report) bool { return true })
}

func (s *Server) handleAdminDeleteReport(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	s.applyReportDelete(w, r, c.Param("id"), func(*domain.Report) bool { return true })
}

// ---------------------------------------------------------------------------
// dashboard
// ---------------------------------------------------------------------------

func (s *Server) handleLogin(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	body, err := readJSONObject(r, 4096)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	rawPassword, ok := body["password"]
	if !ok {
		httpx.WriteError(w, r, httpx.BadRequest("`password` is required and must be a string",
			map[string]any{"field": "password"}))
		return
	}
	plaintext, err := requireString(rawPassword, "password", 1, 512)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	if !password.Verify(plaintext, s.cfg.AdminPasswordHash) {
		// Deliberately vague: do not reveal whether the account or the password
		// was wrong.
		httpx.WriteError(w, r, httpx.Unauthorized("Invalid credentials"))
		return
	}

	session := httpx.BuildSession("admin", s.cfg.SessionTTL)
	cookieValue, err := httpx.SignSession(session, s.cfg.SessionSecret)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to sign session").WithCause(err))
		return
	}
	w.Header().Set("Set-Cookie", httpx.SessionCookie(
		cookieValue, s.cfg.SessionTTL, s.cfg.CookieSecure, s.cfg.CookiePath))

	httpx.OK(w, r, map[string]any{
		"authenticated": true,
		"expires_at":    session.Expires * 1000,
	})
}

func (s *Server) handleLogout(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	w.Header().Set("Set-Cookie", httpx.ClearSessionCookie(s.cfg.CookieSecure, s.cfg.CookiePath))
	httpx.OK(w, r, map[string]any{"authenticated": false})
}

func (s *Server) handleSession(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	subject := ""
	if session := httpx.SessionFrom(r.Context()); session != nil {
		subject = session.Subject
	}
	httpx.OK(w, r, map[string]any{"authenticated": true, "subject": subject})
}

// handleListOwnReports lists the reports the caller itself published.
//
// Root sees everything, because every ownership check in this package already
// grants root access to every report; a named key sees only rows whose
// owner_key_id is its own. That asymmetry is the ownership model, not a
// convenience: an agent must not be able to enumerate another agent's work, and
// it must not accidentally see the root-published reports either (owner_key_id
// ” is root's own).
func (s *Server) handleListOwnReports(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	principal := httpx.PrincipalFrom(r.Context())
	if principal == nil {
		httpx.WriteError(w, r, httpx.Unauthorized(agentAuthFailed))
		return
	}

	q := r.URL.Query()
	limit := clampInt(q.Get("limit"), 50, 1, 200)
	offset := clampInt(q.Get("offset"), 0, 0, 100_000)
	category := q.Get("category")

	var (
		reports []domain.Report
		err     error
	)
	if principal.Root {
		reports, err = s.store.ListReports(category, limit, offset)
	} else {
		reports, err = s.store.ListReportsByOwner(principal.KeyID, category, limit, offset)
	}
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list reports").WithCause(err))
		return
	}

	// Always emit a JSON array, never null, so clients can iterate directly.
	out := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		out = append(out, map[string]any{
			"id":           report.ID,
			"title":        report.Title,
			"category":     report.Category,
			"format":       report.Format,
			"metadata":     report.Metadata,
			"created_at":   report.CreatedAt,
			"updated_at":   report.UpdatedAt,
			"owner_key_id": report.OwnerKeyID,
		})
	}
	httpx.OK(w, r, out)
}

func (s *Server) handleListReports(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	q := r.URL.Query()
	limit := clampInt(q.Get("limit"), 50, 1, 200)
	offset := clampInt(q.Get("offset"), 0, 0, 100_000)
	category := q.Get("category")

	reports, err := s.store.ListReports(category, limit, offset)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list reports").WithCause(err))
		return
	}

	// Resolve owner ids to the names a human chose at approval time. The list
	// query already selected owner_key_id; dropping it here is what made the
	// dashboard unable to answer "who published this", so it is carried through
	// and labelled rather than shown as a raw uuid.
	ownerNames, err := s.ownerNameIndex()
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list reports").WithCause(err))
		return
	}

	// Always emit a JSON array, never null, so clients can iterate directly.
	out := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		out = append(out, map[string]any{
			"id":           report.ID,
			"title":        report.Title,
			"category":     report.Category,
			"format":       report.Format,
			"metadata":     report.Metadata,
			"created_at":   report.CreatedAt,
			"updated_at":   report.UpdatedAt,
			"owner_key_id": report.OwnerKeyID,
			"owner_name":   ownerNames[report.OwnerKeyID],
		})
	}
	httpx.OK(w, r, out)
}

// ownerNameIndex maps owner_key_id to a human label for the dashboard.
//
// The empty key is present on purpose: it is how root-published (and
// pre-ownership) reports are stored, and a blank cell there would read as "the
// dashboard does not know" rather than "nobody's named key".
func (s *Server) ownerNameIndex() (map[string]string, error) {
	keys, err := s.store.ListAgentKeys()
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(keys)+1)
	names[""] = "root (break-glass)"
	for _, k := range keys {
		// A key that was renamed keeps one row, so the latest name wins by
		// construction; revoked keys stay listed, because a report does not stop
		// having an author when the credential is withdrawn.
		names[k.ID] = k.Name
	}
	return names, nil
}

func (s *Server) handleAdminGetReport(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	id := c.Param("id")
	report, err := s.store.GetReport(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load report").WithCause(err))
		return
	}
	if report == nil {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}
	tokens, err := s.store.ListShareTokens(report.ID)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list share tokens").WithCause(err))
		return
	}
	// A report with no tokens must serialize an empty array, not null.
	pub := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		pub = append(pub, map[string]any{
			"token":      t.Token,
			"expires_at": t.ExpiresAt,
			"is_active":  t.IsActive,
			"view_count": t.ViewCount,
			"created_at": t.CreatedAt,
		})
	}
	httpx.OK(w, r, map[string]any{
		"id":           report.ID,
		"title":        report.Title,
		"category":     report.Category,
		"format":       report.Format,
		"content":      report.Content,
		"metadata":     report.Metadata,
		"created_at":   report.CreatedAt,
		"updated_at":   report.UpdatedAt,
		"share_tokens": pub,
	})
}

func (s *Server) handleCreateShare(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	reportID := c.Param("id")

	// An absent or empty body means "use the default duration".
	var body map[string]any
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		httpx.WriteError(w, r, httpx.BadRequest("Could not read request body", nil))
		return
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		parsed, err := validateJSON(raw)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		body, _ = parsed.(map[string]any)
	}

	hours, err := readHours(body)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	created, err := s.store.CreateShareToken(reportID, hours)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to create share token").WithCause(err))
		return
	}
	if created == nil {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}

	httpx.OK(w, r, map[string]any{
		"token":      created.Token,
		"expires_at": created.ExpiresAt,
		"is_active":  created.IsActive,
		"view_count": created.ViewCount,
		"created_at": created.CreatedAt,
		"url":        s.cfg.ShareURL(created.Token),
	}, http.StatusCreated)
}

func (s *Server) handlePatchShare(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	token, err := s.shareToken(c.Param("token"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	body, err := readJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	patch := store.SharePatch{}
	// expiresInHours wins when both are supplied, matching the original.
	if v, ok := body["expiresInHours"]; ok {
		f, ok := jsNumber(v)
		if !ok || f < 0 {
			httpx.WriteError(w, r, httpx.BadRequest("`expiresInHours` must be a non-negative number",
				map[string]any{"field": "expiresInHours"}))
			return
		}
		patch.ExpiresInHours = &f
	} else if v, ok := body["expiresAt"]; ok {
		f, ok := jsNumber(v)
		if !ok || f < 0 {
			httpx.WriteError(w, r, httpx.BadRequest("`expiresAt` must be epoch ms (0 = never expires)",
				map[string]any{"field": "expiresAt"}))
			return
		}
		// The original applied Math.round to the absolute value.
		rounded := int64(f + 0.5)
		patch.ExpiresAt = &rounded
	}

	if v, ok := body["isActive"]; ok {
		b, ok := v.(bool)
		if !ok {
			httpx.WriteError(w, r, httpx.BadRequest("`isActive` must be a boolean",
				map[string]any{"field": "isActive"}))
			return
		}
		patch.IsActive = &b
	}

	if patch.ExpiresInHours == nil && patch.ExpiresAt == nil && patch.IsActive == nil {
		httpx.WriteError(w, r, httpx.BadRequest(
			"Provide at least one of: expiresInHours, expiresAt, isActive", nil))
		return
	}

	updated, err := s.store.UpdateShareToken(token, patch)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to update share token").WithCause(err))
		return
	}
	if updated == nil {
		httpx.WriteError(w, r, httpx.NotFound("Share token not found"))
		return
	}
	httpx.OK(w, r, map[string]any{
		"token":      updated.Token,
		"expires_at": updated.ExpiresAt,
		"is_active":  updated.IsActive,
		"view_count": updated.ViewCount,
		"created_at": updated.CreatedAt,
		"url":        s.cfg.ShareURL(updated.Token),
	})
}

func (s *Server) handleDeleteShare(c *gin.Context) {
	// Bound so the body below stays byte-identical to the pre-Gin handler.
	w, r := c.Writer, c.Request
	token, err := s.shareToken(c.Param("token"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	found, err := s.store.RevokeToken(token)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to revoke share token").WithCause(err))
		return
	}
	if !found {
		httpx.WriteError(w, r, httpx.NotFound("Share token not found"))
		return
	}
	httpx.OK(w, r, map[string]any{"token": token, "is_active": false})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ownerCanAccess reports whether the authenticated principal may read or revoke
// a report owned by ownerKeyID.
//
// Root passes everything. A named key passes only for its own reports. The
// empty-KeyID guard is not decorative: root's reports store "" as their owner,
// so without it a principal that somehow carried an empty key id would match
// every root-published report. Callers turn a false into 404, never 403.
func ownerCanAccess(principal *domain.Principal, ownerKeyID string) bool {
	if principal == nil {
		return false
	}
	if principal.Root {
		return true
	}
	return principal.KeyID != "" && principal.KeyID == ownerKeyID
}

// shareToken validates the {token} path value, returning a 404 (not a 400) when
// the shape is wrong so a malformed token is indistinguishable from an unknown
// one.
//
// It takes the raw path value rather than the request so that the caller reads
// it from the router (c.Param) at the one place a router exists.
func (s *Server) shareToken(raw string) (string, error) {
	if raw == "" || !isShareTokenShape(raw) {
		return "", httpx.NotFound("Report or share link not found")
	}
	return raw, nil
}

func sharePayload(resolution *domain.Resolution) map[string]any {
	report := resolution.Report
	share := resolution.Share
	return map[string]any{
		"report": map[string]any{
			"id":         report.ID,
			"title":      report.Title,
			"category":   report.Category,
			"format":     report.Format,
			"content":    report.Content,
			"metadata":   report.Metadata,
			"created_at": report.CreatedAt,
			"updated_at": report.UpdatedAt,
		},
		"share": map[string]any{
			"token":      share.Token,
			"expires_at": share.ExpiresAt,
			"view_count": share.ViewCount,
		},
	}
}

func clampInt(value string, fallback, min, max int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	if parsed < min {
		return min
	}
	if parsed > max {
		return max
	}
	return parsed
}

func readJSONObject(r *http.Request, limit int64) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return nil, httpx.BadRequest("Could not read request body", nil)
	}
	body, err := validateJSON(raw)
	if err != nil {
		return nil, err
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return nil, httpx.BadRequest("Request body must be a JSON object", nil)
	}
	return obj, nil
}
