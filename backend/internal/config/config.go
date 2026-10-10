// Package config reads and validates all runtime configuration.
//
// Configuration comes from environment variables, optionally seeded from a
// `.env` file next to the binary (developer convenience only — production uses
// systemd's EnvironmentFile). Reading everything through this package means a
// missing secret fails loudly at startup instead of producing an empty string
// deep inside a request handler.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved, validated runtime configuration.
type Config struct {
	// Addr is the TCP listen address, e.g. "172.17.0.1:8788".
	Addr string
	// RoutePrefix is the path prefix every route is mounted under, including
	// the frontend. It must match the prefix Caddy routes on, because this
	// process is the single source of truth for the whole app surface: the same
	// prefix appears in API routes, in the SPA's asset URLs and client routes,
	// in the server-rendered share page, and in the share links built from
	// AppBaseURL.
	RoutePrefix string
	// PublicBaseURL is the public ORIGIN only — scheme and host, with no path.
	// Example: https://api.hcyj.xyz
	//
	// The application itself is not served at this origin's root: everything
	// (SPA, API, share pages) lives under RoutePrefix. Use AppBaseURL for links.
	PublicBaseURL string
	// AssetBaseURL is the absolute base the share page loads its JS bundle from.
	// The share page's HTML is rendered by this backend while the bundle belongs
	// to the frontend's static build; both are served under AppBaseURL, so this
	// defaults to it. It stays overridable for split-origin setups.
	AssetBaseURL string
	// FrontendOrigins is the CORS allow-list. This deployment is entirely
	// same-origin (SPA, API and share pages all sit under RoutePrefix), so CORS
	// is a belt-and-braces control rather than the primary path: it only
	// matters if someone deliberately points a cross-origin client at /api/*.
	FrontendOrigins []string

	// StaticDir is the directory holding the built frontend (index.html plus
	// assets/). It is served by this process under RoutePrefix.
	//
	// Serving the SPA from Go rather than from an edge/CDN is what makes the
	// deployment reachable from mainland China: Cloudflare's assigned anycast
	// IPs are TCP-blocked there (see README "已知陷阱"), while this host is not.
	// Empty disables static serving entirely, which is what tests want.
	StaticDir string

	// DBPath is the SQLite file path (or ":memory:" for tests).
	DBPath string

	DefaultShareHours int
	MaxContentBytes   int64

	// Secrets.
	AgentSecretKey    string
	SessionSecret     string
	AdminPasswordHash string

	// SessionTTL is how long a dashboard login stays valid.
	SessionTTL time.Duration
	// CookieSecure sets the `Secure` attribute. Must be true in production
	// (the public entry point is HTTPS); false only for plain-HTTP local dev.
	CookieSecure bool
	// CookiePath scopes the session cookie. Defaults to RoutePrefix, which is
	// what keeps the cookie off the other services sharing this public host
	// (see Load). Set explicitly only for an unusual deployment shape.
	CookiePath string
	// TrustedProxies is currently informational: the service only ever listens
	// on a private interface behind Caddy, so all remote addresses are proxies.
	TrustedProxies []string

	// Environment is echoed by /api/health.
	Environment string

	// -- agent self-service keys -------------------------------------------
	//
	// These bound the public application endpoint. Anyone on the internet can
	// submit an application (there is no shared secret to hand over first), so
	// the limits are what stand between the service and a flooded approval
	// queue. The counters are in-process, so they reset on restart: a known and
	// accepted trade-off for a single-binary personal deployment.
	//
	// KeyApplyPerHour caps applications per source IP per hour.
	KeyApplyPerHour int
	// KeyApplicationTTL is how long an unapproved application stays open.
	KeyApplicationTTL time.Duration
	// KeyClaimWindow is how long an approved application can still be claimed.
	// After it lapses the credential is never issued at all.
	KeyClaimWindow time.Duration
	// KeyMaxPending caps outstanding applications, approved or not.
	KeyMaxPending int
	// KeyMaxActive caps live credentials, so a compromised approver or a runaway
	// agent cannot mint unbounded keys.
	KeyMaxActive int
	// KeyRenewalGrace is how long past its expiry a key may still be presented
	// to ask for a renewal.
	//
	// Expiry used to be unrecoverable by the agent itself: the renewal request
	// needs the key to authenticate, so a key that lapsed without a pending
	// request could never be renewed, and re-applying mints a *new* key id —
	// orphaning every report the old one had published, since ownership is
	// compared by key id. The grace window closes that: an expired key can still
	// file the request, and a human still has to approve it. Zero disables the
	// window entirely, restoring the old behaviour.
	KeyRenewalGrace time.Duration
}

// Load builds a Config from the environment, after optionally loading a .env
// file. Values already present in the real environment always win.
func Load() (*Config, error) {
	loadDotEnv(findDotEnv())

	cfg := &Config{
		Addr:              env("LISTEN_ADDR", "127.0.0.1:8788"),
		RoutePrefix:       normalizePrefix(env("ROUTE_PREFIX", "/yeciorez/teleport")),
		PublicBaseURL:     strings.TrimRight(env("PUBLIC_BASE_URL", "http://127.0.0.1:8788"), "/"),
		DBPath:            env("DB_PATH", "./data/teleport.db"),
		Environment:       env("ENVIRONMENT", "development"),
		AgentSecretKey:    os.Getenv("AGENT_SECRET_KEY"),
		SessionSecret:     os.Getenv("SESSION_SECRET"),
		AdminPasswordHash: os.Getenv("ADMIN_PASSWORD_HASH"),
		StaticDir:         env("STATIC_DIR", ""),
		CookiePath:        env("COOKIE_PATH", ""),
		TrustedProxies:    splitList(env("TRUSTED_PROXIES", "")),
	}

	// Scope the session cookie to the application prefix by default.
	//
	// This is not cosmetic: PublicBaseURL is a shared host (api.hcyj.xyz also
	// fronts an unrelated service on :3000). With the default `Path=/` the
	// browser would attach our session cookie to every request for that other
	// service too — needless exposure of a credential, and a confusing thing to
	// debug. Restricting it to the prefix means the cookie is only ever sent to
	// routes this process owns.
	if cfg.CookiePath == "" {
		cfg.CookiePath = cfg.RoutePrefix
		if cfg.CookiePath == "" {
			cfg.CookiePath = "/"
		}
	}

	// The share page's JS is served by this same process under the route
	// prefix, so the natural default is the application base (origin + prefix),
	// not the bare origin.
	if cfg.AssetBaseURL = strings.TrimRight(env("ASSET_BASE_URL", ""), "/"); cfg.AssetBaseURL == "" {
		cfg.AssetBaseURL = cfg.AppBaseURL()
	}

	if cfg.PublicBaseURL == "" {
		return nil, fmt.Errorf("PUBLIC_BASE_URL must not be empty")
	}
	if cfg.SessionSecret == "" {
		return nil, fmt.Errorf("SESSION_SECRET is required (generate: openssl rand -base64 48)")
	}
	if cfg.AgentSecretKey == "" {
		return nil, fmt.Errorf("AGENT_SECRET_KEY is required (generate: openssl rand -base64 32)")
	}
	if cfg.AdminPasswordHash == "" {
		return nil, fmt.Errorf("ADMIN_PASSWORD_HASH is required (generate: teleport hash-password 'your password')")
	}

	// Default the CORS allow-list to the public origin. This deployment is
	// same-origin, so this only fires for deliberate cross-origin clients.
	origins := splitList(os.Getenv("FRONTEND_ORIGINS"))
	if len(origins) == 0 {
		origins = []string{cfg.PublicBaseURL}
	}
	cfg.FrontendOrigins = origins

	var err error
	if cfg.DefaultShareHours, err = envInt("DEFAULT_SHARE_HOURS", 168); err != nil {
		return nil, err
	}
	if cfg.MaxContentBytes, err = envInt64("MAX_CONTENT_BYTES", 1<<20); err != nil {
		return nil, err
	}
	if cfg.SessionTTL, err = envDuration("SESSION_TTL", 12*time.Hour); err != nil {
		return nil, err
	}
	if cfg.KeyApplyPerHour, err = envInt("KEY_APPLY_PER_HOUR", 5); err != nil {
		return nil, err
	}
	if cfg.KeyApplicationTTL, err = envDuration("KEY_APPLICATION_TTL", 24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.KeyClaimWindow, err = envDuration("KEY_CLAIM_WINDOW", 30*time.Minute); err != nil {
		return nil, err
	}
	if cfg.KeyMaxPending, err = envInt("KEY_MAX_PENDING", 50); err != nil {
		return nil, err
	}
	if cfg.KeyMaxActive, err = envInt("KEY_MAX_ACTIVE", 100); err != nil {
		return nil, err
	}
	if cfg.KeyRenewalGrace, err = envDurationAllowZero("KEY_RENEWAL_GRACE", 90*24*time.Hour); err != nil {
		return nil, err
	}
	// A zero or negative limit would reject every request, which reads as a
	// broken deployment rather than a strict one. Reject it at startup instead.
	if cfg.KeyApplyPerHour <= 0 {
		return nil, fmt.Errorf("KEY_APPLY_PER_HOUR must be positive (got %d)", cfg.KeyApplyPerHour)
	}
	if cfg.KeyMaxPending <= 0 {
		return nil, fmt.Errorf("KEY_MAX_PENDING must be positive (got %d)", cfg.KeyMaxPending)
	}
	if cfg.KeyMaxActive <= 0 {
		return nil, fmt.Errorf("KEY_MAX_ACTIVE must be positive (got %d)", cfg.KeyMaxActive)
	}
	if cfg.KeyApplicationTTL <= 0 {
		return nil, fmt.Errorf("KEY_APPLICATION_TTL must be positive (got %s)", cfg.KeyApplicationTTL)
	}
	if cfg.KeyClaimWindow <= 0 {
		return nil, fmt.Errorf("KEY_CLAIM_WINDOW must be positive (got %s)", cfg.KeyClaimWindow)
	}
	// Zero is meaningful here — it turns the grace window off and restores strict
	// expiry — so it is accepted, unlike the other KEY_* durations. A negative
	// value would silently widen the window instead of closing it.
	if cfg.KeyRenewalGrace < 0 {
		return nil, fmt.Errorf("KEY_RENEWAL_GRACE must not be negative (got %s)", cfg.KeyRenewalGrace)
	}
	// Bound it so a typo cannot widen the window to effectively forever, and so
	// `now - grace` in the resolver's SQL stays far from underflowing.
	if cfg.KeyRenewalGrace > 10*365*24*time.Hour {
		return nil, fmt.Errorf("KEY_RENEWAL_GRACE must be at most 10 years (got %s)", cfg.KeyRenewalGrace)
	}
	if cfg.CookieSecure, err = envBool("COOKIE_SECURE", true); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Prefix returns the normalized route prefix, always starting with "/" and
// never ending with one, so callers can concatenate paths safely.
func normalizePrefix(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

// AppBaseURL is the absolute base of the whole application: the public origin
// plus the route prefix, e.g. "https://api.hcyj.xyz/yeciorez/teleport".
//
// Every browser-facing URL hangs off this: the SPA shell, its hashed assets and
// the server-rendered share pages. Keeping it derived from two configured
// values (instead of being a third independent one) means a prefix change
// cannot leave the share links pointing somewhere the SPA is not served.
func (c *Config) AppBaseURL() string {
	return c.PublicBaseURL + c.RoutePrefix
}

// Route builds a full URL path by joining the configured prefix and a
// suffix that must start with "/".
func (c *Config) Route(suffix string) string {
	return c.RoutePrefix + suffix
}

// ShareURL is the absolute, publicly reachable URL of a share link.
//
// Share pages are rendered by this backend under the route prefix, so the link
// is AppBaseURL-based rather than origin-based: the same process serves the
// page and its assets, which keeps `script-src 'self'` meaningful.
func (c *Config) ShareURL(token string) string {
	return c.AppBaseURL() + "/s/" + token
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, raw)
	}
	return n, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, raw)
	}
	return n, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", key, raw)
	}
	return b, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration (e.g. 12h), got %q", key, raw)
	}
	return d, nil
}

// envDurationAllowZero is envDuration for a knob where zero carries meaning
// rather than being an unusable value.
//
// KEY_RENEWAL_GRACE is the case: 0 is how an operator turns the renewal grace
// window off and restores strict expiry, so rejecting it as "not positive" would
// make the feature unremovable through configuration. A negative duration stays
// an error — it would *widen* the window instead of closing it, which is the
// opposite of what someone typing a minus sign intends.
func envDurationAllowZero(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		// Go durations have no day unit, and the natural way to write a 90-day
		// window is "90d" — which fails. Say what to write instead of only
		// saying no: this is the one value most likely to be set by hand.
		return 0, fmt.Errorf("%s must be a duration of at least 0 (0 disables it), got %q "+
			"(Go durations have no day unit; write 90 days as 2160h)", key, raw)
	}
	return d, nil
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, strings.TrimRight(p, "/"))
		}
	}
	return out
}

// findDotEnv locates a .env file next to the executable or in the working
// directory, so `go run ./backend` and an installed binary both pick it up.
func findDotEnv() string {
	if explicit := os.Getenv("ENV_FILE"); explicit != "" {
		return explicit
	}
	candidates := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, candidate := range candidates {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return ""
}

// loadDotEnv reads a minimal KEY=VALUE file. It never overrides variables that
// are already set, so systemd/EnvironmentFile always takes precedence.
func loadDotEnv(path string) {
	if path == "" {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// Strip a single layer of matching quotes.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}
