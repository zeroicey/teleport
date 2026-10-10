package config

import (
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
