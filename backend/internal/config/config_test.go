package config

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

// requiredEnv sets the minimum an environment must provide for Load to get as
// far as the value under test, and clears everything else so one test cannot
// leak settings into another.
func requiredEnv(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{"SESSION_SECRET", "test-session-secret"},
		{"AGENT_SECRET_KEY", "test-agent-secret"},
		{"ADMIN_PASSWORD_HASH", "pbkdf2$test"},
		// Required since the store moved to PostgreSQL (P2). It is a plain
		// string here, not a live DSN: Load validates presence and form, it does
		// not connect, and a config test must not need a database.
		{"DATABASE_URL", "postgres://teleport:test@127.0.0.1:5432/teleport_test?sslmode=disable"},
	} {
		t.Setenv(kv[0], kv[1])
	}
}

// TestKeyRenewalGraceDefaultsAndValidation covers the knob that decides whether
// letting a key lapse is recoverable. Its zero value is meaningful — it turns the
// window off — so the difference between "unset", "0" and "negative" has to be
// defined rather than left to Go's zero value.
func TestKeyRenewalGraceDefaultsAndValidation(t *testing.T) {
	t.Run("defaults to 90 days", func(t *testing.T) {
		requiredEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := 90 * 24 * time.Hour; cfg.KeyRenewalGrace != want {
			t.Errorf("KeyRenewalGrace = %s, want %s", cfg.KeyRenewalGrace, want)
		}
	})

	t.Run("zero is accepted and disables the window", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("KEY_RENEWAL_GRACE", "0")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.KeyRenewalGrace != 0 {
			t.Errorf("KeyRenewalGrace = %s, want 0", cfg.KeyRenewalGrace)
		}
	})

	t.Run("a negative value is rejected rather than widening the window", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("KEY_RENEWAL_GRACE", "-1h")
		_, err := Load()
		if err == nil {
			t.Fatal("negative KEY_RENEWAL_GRACE was accepted")
		}
		if !strings.Contains(err.Error(), "KEY_RENEWAL_GRACE") {
			t.Errorf("error does not name the variable: %v", err)
		}
	})

	t.Run("an absurd value is rejected", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("KEY_RENEWAL_GRACE", "100000h")
		if _, err := Load(); err == nil {
			t.Fatal("KEY_RENEWAL_GRACE beyond the cap was accepted")
		}
	})

	t.Run("an explicit window is honoured", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("KEY_RENEWAL_GRACE", "720h")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.KeyRenewalGrace != 30*24*time.Hour {
			t.Errorf("KeyRenewalGrace = %s, want 720h", cfg.KeyRenewalGrace)
		}
	})
}

// TestDatabaseConfigValidation covers the storage settings the rewrite added.
//
// DATABASE_URL is required with no default on purpose: a wrong-but-present
// default would point the service at another database, and the failure would
// surface as missing data rather than as a startup error. Both branches are
// asserted here because an unmatched validation branch is a branch nobody has
// seen work.
func TestDatabaseConfigValidation(t *testing.T) {
	t.Run("DATABASE_URL is required", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("DATABASE_URL", "")
		_, err := Load()
		if err == nil {
			t.Fatal("Load accepted an empty DATABASE_URL; the service would start with no database")
		}
		if !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Errorf("error does not name the variable: %v", err)
		}
	})

	t.Run("DB_MAX_CONNS defaults to a small pool", func(t *testing.T) {
		requiredEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// Not a magic number for its own sake: the PostgreSQL instance is shared
		// with clip / registry / serenique and max_connections=100 is their
		// combined budget, so the pool must be bounded by default rather than
		// taking the driver's larger default.
		if cfg.DBMaxConns != 8 {
			t.Errorf("DBMaxConns = %d, want 8", cfg.DBMaxConns)
		}
	})

	t.Run("DB_MAX_CONNS must be positive", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("DB_MAX_CONNS", "0")
		_, err := Load()
		if err == nil {
			t.Fatal("Load accepted DB_MAX_CONNS=0; the pool could never hand out a connection")
		}
		if !strings.Contains(err.Error(), "DB_MAX_CONNS") {
			t.Errorf("error does not name the variable: %v", err)
		}
	})
}

// --- security middleware ---------------------------------------------------

// TestSecurityMiddlewareDefaultsByEnvironment is the knob the whole feature hangs
// off, and its default is conditional — which is exactly the kind of thing that
// silently regresses if it is only ever tested one way.
//
// The two audiences want opposite defaults: production is public and wants every
// guard on; the test and contract harnesses drive the whole surface from one
// address in seconds and would read a working limiter as a regression.
func TestSecurityMiddlewareDefaultsByEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"production", true},
		{"development", false},
		{"test", false},
		{"", false},
	} {
		t.Run("ENVIRONMENT="+tc.env, func(t *testing.T) {
			requiredEnv(t)
			t.Setenv("ENVIRONMENT", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SecurityMiddleware != tc.want {
				t.Errorf("SecurityMiddleware = %v with ENVIRONMENT=%q, want %v",
					cfg.SecurityMiddleware, tc.env, tc.want)
			}
		})
	}
}

// TestSecurityMiddlewareExplicitSettingWins proves the default is a default and
// not a policy: an operator must be able to force it either way.
func TestSecurityMiddlewareExplicitSettingWins(t *testing.T) {
	for _, tc := range []struct {
		env   string
		value string
		want  bool
	}{
		{"production", "false", false},
		{"development", "true", true},
	} {
		t.Run(tc.env+"+"+tc.value, func(t *testing.T) {
			requiredEnv(t)
			t.Setenv("ENVIRONMENT", tc.env)
			t.Setenv("SECURITY_MIDDLEWARE", tc.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SecurityMiddleware != tc.want {
				t.Errorf("SecurityMiddleware = %v, want %v", cfg.SecurityMiddleware, tc.want)
			}
		})
	}
}

// TestBlockedIPsAreParsedAndValidated covers the one control in this set that is
// not self-healing.
//
// Both directions matter and they fail differently: an entry that is accepted but
// never matches is a blocklist that silently blocks nothing, and an entry that is
// rejected at startup is merely annoying. The first is the dangerous one.
func TestBlockedIPsAreParsedAndValidated(t *testing.T) {
	t.Run("plain addresses and CIDRs both match", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("BLOCKED_IPS", "203.0.113.7, 198.51.100.0/24, 2001:db8::/32")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.BlockedPrefixes) != 3 {
			t.Fatalf("BlockedPrefixes = %v, want 3 entries", cfg.BlockedPrefixes)
		}

		inPrefix := func(raw string) bool {
			addr := netip.MustParseAddr(raw)
			for _, p := range cfg.BlockedPrefixes {
				if p.Contains(addr) {
					return true
				}
			}
			return false
		}
		for _, raw := range []string{"203.0.113.7", "198.51.100.99", "2001:db8::1"} {
			if !inPrefix(raw) {
				t.Errorf("%s is not blocked; the entry was parsed but never matches", raw)
			}
		}
		if inPrefix("203.0.113.8") {
			t.Error("203.0.113.8 blocked by a 203.0.113.7/32 entry")
		}
	})

	t.Run("a non-address refuses to start", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("BLOCKED_IPS", "not-an-address")
		if _, err := Load(); err == nil {
			t.Fatal("Load accepted a BLOCKED_IPS entry that is not an address; " +
				"a blocklist that silently blocks nothing is the failure mode to avoid")
		}
	})

	t.Run("a masked prefix matches what the human meant", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("BLOCKED_IPS", "10.1.2.3/8")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// Masked to 10.0.0.0/8, so a sibling address matches.
		if !cfg.BlockedPrefixes[0].Contains(netip.MustParseAddr("10.9.9.9")) {
			t.Errorf("10.1.2.3/8 parsed as %s; expected it masked to 10.0.0.0/8",
				cfg.BlockedPrefixes[0])
		}
	})
}

// TestSecurityLimitsMustBePositive: a zero or negative budget would reject every
// request, which reads as a broken deployment rather than a strict one.
func TestSecurityLimitsMustBePositive(t *testing.T) {
	for _, name := range []string{
		"LOGIN_RATE_LIMIT",
		"PUBLIC_READ_RATE_LIMIT",
		"WRITE_RATE_LIMIT",
		"GLOBAL_RATE_LIMIT",
	} {
		t.Run(name, func(t *testing.T) {
			requiredEnv(t)
			t.Setenv("ENVIRONMENT", "production")
			t.Setenv(name, "0")
			if _, err := Load(); err == nil {
				t.Errorf("Load accepted %s=0; a zero budget rejects every request", name)
			}
		})
	}
}

// TestBanAfterRefusalsZeroIsMeaningful: zero disables automatic banning while
// leaving the permanent blocklist enforced, so it must be accepted.
func TestBanAfterRefusalsZeroIsMeaningful(t *testing.T) {
	requiredEnv(t)
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("BAN_AFTER_REFUSALS", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BanAfterRefusals != 0 {
		t.Errorf("BanAfterRefusals = %d, want 0", cfg.BanAfterRefusals)
	}
}

// TestDashboardCSPStartsOutNotEnforcing pins the deliberate first phase of the SPA
// policy: it must not enforce until it has been verified on real traffic.
//
// The field is spelled for its safe state (DashboardCSPEnforce, default false) so
// that a Config nobody configured also gets report-only. This test covers the
// Load path; the zero-value case is covered by the API-level test that builds a
// Config by hand.
func TestDashboardCSPStartsOutNotEnforcing(t *testing.T) {
	requiredEnv(t)
	t.Setenv("ENVIRONMENT", "production")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DashboardCSPEnforce {
		t.Error("the dashboard CSP enforces by default; it must be verified against real traffic first")
	}

	t.Run("it can be turned on deliberately", func(t *testing.T) {
		requiredEnv(t)
		t.Setenv("DASHBOARD_CSP_ENFORCE", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.DashboardCSPEnforce {
			t.Error("DASHBOARD_CSP_ENFORCE=true did not enable enforcement")
		}
	})
}
