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
	"os"
	"strconv"
	"strings"

	"github.com/zeroicey/teleport/backend/internal/aidoc"
	"github.com/zeroicey/teleport/backend/internal/config"
	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/httpx"
	"github.com/zeroicey/teleport/backend/internal/markdown"
	"github.com/zeroicey/teleport/backend/internal/password"
	"github.com/zeroicey/teleport/backend/internal/spa"
	"github.com/zeroicey/teleport/backend/internal/store"
	"github.com/zeroicey/teleport/backend/internal/webui"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	cfg    *config.Config
	store  *store.Store
	render *markdown.Renderer
}

// New builds the HTTP handler for the whole API.
//
// It returns an error rather than exiting so the caller controls startup
// failure: a misconfigured StaticDir should stop the process with a clear
// message, not silently serve an API with no frontend.
func New(cfg *config.Config, st *store.Store) (http.Handler, error) {
	s := &Server{cfg: cfg, store: st, render: markdown.New()}

	authCfg := httpx.AuthConfig{
		AgentSecretKey: cfg.AgentSecretKey,
		SessionSecret:  cfg.SessionSecret,
		CookieSecure:   cfg.CookieSecure,
		CookiePath:     cfg.CookiePath,
		SessionTTL:     cfg.SessionTTL,
	}
	requireAgent := httpx.RequireAgent(authCfg)
	requireSession := httpx.RequireSession(authCfg)

	mux := http.NewServeMux()
	p := cfg.RoutePrefix

	// -- public ---------------------------------------------------------------
	mux.HandleFunc("GET "+p+"/api/health", s.handleHealth)
	mux.HandleFunc("GET "+p+"/api/share/{token}", s.handleReadShare)
	mux.HandleFunc("GET "+p+"/s/{token}", s.handleSharePage)

	// -- machine-readable usage guide -----------------------------------------
	//
	// Served by this process, not the SPA, because an agent fetching a URL gets
	// only what the server sends: the SPA renders client-side and would hand
	// back an empty shell.
	//
	// These sit at stable paths rather than behind a share link on purpose. A
	// share link expires, so a guide distributed as one would eventually 410 on
	// every agent that had cached it — the opposite of what this is for.
	mux.HandleFunc("GET "+p+"/ai", s.handleAIGuideHTML)
	mux.HandleFunc("GET "+p+"/ai.md", s.handleAIGuideMarkdown)
	mux.HandleFunc("GET "+p+"/llms.txt", s.handleLLMsTxt)

	// -- agent ----------------------------------------------------------------
	mux.Handle("POST "+p+"/api/reports", requireAgent(http.HandlerFunc(s.handleCreateReport)))
	mux.Handle("GET "+p+"/api/reports/{id}", requireAgent(http.HandlerFunc(s.handleGetReport)))
	mux.Handle("POST "+p+"/api/share/{token}/revoke", requireAgent(http.HandlerFunc(s.handleRevoke)))

	// -- dashboard ------------------------------------------------------------
	mux.HandleFunc("POST "+p+"/api/admin/login", s.handleLogin)
	mux.HandleFunc("POST "+p+"/api/admin/logout", s.handleLogout)
	mux.Handle("GET "+p+"/api/admin/session", requireSession(http.HandlerFunc(s.handleSession)))
	mux.Handle("GET "+p+"/api/admin/reports", requireSession(http.HandlerFunc(s.handleListReports)))
	mux.Handle("GET "+p+"/api/admin/reports/{id}", requireSession(http.HandlerFunc(s.handleAdminGetReport)))
	mux.Handle("POST "+p+"/api/admin/reports/{id}/shares", requireSession(http.HandlerFunc(s.handleCreateShare)))
	mux.Handle("PATCH "+p+"/api/admin/shares/{token}", requireSession(http.HandlerFunc(s.handlePatchShare)))
	mux.Handle("DELETE "+p+"/api/admin/shares/{token}", requireSession(http.HandlerFunc(s.handleDeleteShare)))

	// Anything else under the prefix is a JSON 404 rather than Go's plain-text
	// "404 page not found", so clients can rely on the envelope everywhere.
	//
	// This is registered as the prefix catch-all, and net/http gives the more
	// specific patterns above precedence over it — that is what keeps unknown
	// /api/... paths returning JSON instead of falling through to the SPA.
	var root http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "Not found", nil)
	})

	// API and share paths get their own catch-alls so they keep the JSON
	// envelope even when the SPA owns the prefix root.
	//
	// Without these, an unknown /api/... path matches the SPA catch-all and the
	// client receives an HTML 404 — which is worse than it sounds: an XHR caller
	// doing response.json() on it fails with a parse error and reports "bad
	// response" instead of the actual 404. Both are more specific than
	// `p+"/"`, so the SPA never sees these.
	mux.Handle(p+"/api/", root)
	mux.Handle(p+"/s/", root)

	// Serve the frontend from this same process when one is available. It is
	// mounted at the prefix root, so the SPA shell, its hashed assets and the
	// API all share one origin — which is what makes the session cookie
	// first-party and keeps the share page's `script-src 'self'` meaningful.
	//
	// Two sources, in order of preference:
	//
	//   1. Assets embedded in the binary (the normal deployment: one file to
	//      copy, and the assets can never drift from the code).
	//   2. STATIC_DIR on disk, which makes `pnpm run build` + restart enough to
	//      ship a frontend change without recompiling.
	//
	// The JSON 404 handler stays as the fallback for /api/* and /s/* misses
	// (spa.Handler deliberately refuses those paths too, so either layer alone
	// is safe), and is what answers when no frontend is present at all.
	build, buildSource, err := resolveFrontend(cfg)
	if err != nil {
		return nil, err
	}
	if build != nil {
		static, err := spa.New(build, p, buildSource)
		if err != nil {
			return nil, fmt.Errorf("frontend build at %s is unusable: %w", buildSource, err)
		}
		slog.Info("serving frontend", "source", buildSource, "prefix", p)
		mux.Handle(p+"/", static)
	} else {
		slog.Warn("no frontend build available; serving API only")
		mux.Handle(p+"/", root)
	}

	return httpx.Chain(mux,
		httpx.Recover,
		httpx.RequestID,
		httpx.LogRequests,
		httpx.SecurityHeaders,
		httpx.CORS(cfg.FrontendOrigins),
	), nil
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
func (s *Server) handleAIGuideMarkdown(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) handleAIGuideHTML(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, aidoc.LLMSTxt(s.aiFacts()))
}

func (s *Server) aiFacts() aidoc.Facts {
	return aidoc.FactsFrom(s.cfg)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	// A cheap liveness probe for the database. A failure is reported as 503 so
	// the systemd/Caddy health checks can act on it.
	if err := s.store.DB().PingContext(r.Context()); err != nil {
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

func (s *Server) handleReadShare(w http.ResponseWriter, r *http.Request) {
	token, err := s.shareToken(r)
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

func (s *Server) handleSharePage(w http.ResponseWriter, r *http.Request) {
	token, err := s.shareToken(r)
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
	h := w.Header()
	h.Set("Cache-Control", "no-store, must-revalidate")
	h.Set("Content-Security-Policy", "default-src 'none'; "+
		"script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
		"font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; "+
		"frame-ancestors 'none'; upgrade-insecure-requests")
	h.Set("Content-Type", "text/html; charset=utf-8")

	page := renderSharePage(s, resolution)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, page)
}

// ---------------------------------------------------------------------------
// agent
// ---------------------------------------------------------------------------

func (s *Server) handleCreateReport(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	report, err := s.store.GetReport(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load report").WithCause(err))
		return
	}
	if report == nil {
		httpx.WriteError(w, r, httpx.NotFound("Report not found"))
		return
	}
	httpx.OK(w, r, report)
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	token, err := s.shareToken(r)
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
// dashboard
// ---------------------------------------------------------------------------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Set-Cookie", httpx.ClearSessionCookie(s.cfg.CookieSecure, s.cfg.CookiePath))
	httpx.OK(w, r, map[string]any{"authenticated": false})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	subject := ""
	if session := httpx.SessionFrom(r.Context()); session != nil {
		subject = session.Subject
	}
	httpx.OK(w, r, map[string]any{"authenticated": true, "subject": subject})
}

func (s *Server) handleListReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := clampInt(q.Get("limit"), 50, 1, 200)
	offset := clampInt(q.Get("offset"), 0, 0, 100_000)
	category := q.Get("category")

	reports, err := s.store.ListReports(category, limit, offset)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list reports").WithCause(err))
		return
	}
	// Always emit a JSON array, never null, so clients can iterate directly.
	out := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		out = append(out, map[string]any{
			"id":         report.ID,
			"title":      report.Title,
			"category":   report.Category,
			"format":     report.Format,
			"metadata":   report.Metadata,
			"created_at": report.CreatedAt,
			"updated_at": report.UpdatedAt,
		})
	}
	httpx.OK(w, r, out)
}

func (s *Server) handleAdminGetReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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

func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	reportID := r.PathValue("id")

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

func (s *Server) handlePatchShare(w http.ResponseWriter, r *http.Request) {
	token, err := s.shareToken(r)
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

func (s *Server) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	token, err := s.shareToken(r)
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

// shareToken validates the {token} path value, returning a 404 (not a 400) when
// the shape is wrong so a malformed token is indistinguishable from an unknown
// one.
func (s *Server) shareToken(r *http.Request) (string, error) {
	raw := r.PathValue("token")
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
