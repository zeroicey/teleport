package agentkey

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// The derivation is the only thing standing between a claim secret and a
// credential, so its determinism and its separation properties are the contract.
func TestDeriveTokenIsDeterministicAndDomainSeparated(t *testing.T) {
	k := KDerive("session-secret")

	base := DeriveToken(k, "app-1", "claim-1")
	if again := DeriveToken(k, "app-1", "claim-1"); again != base {
		t.Fatalf("derivation is not deterministic: %q vs %q", base, again)
	}

	// Every input must actually matter. If any of these collide, one application
	// could claim another's credential.
	for _, tc := range []struct {
		name string
		got  string
	}{
		{"different application", DeriveToken(k, "app-2", "claim-1")},
		{"different claim secret", DeriveToken(k, "app-1", "claim-2")},
		{"different derive key", DeriveToken(KDerive("other-secret"), "app-1", "claim-1")},
	} {
		if tc.got == base {
			t.Errorf("%s produced the same token", tc.name)
		}
	}

	// A single secret must never be replayable across the two usages: K_derive
	// is a subkey precisely so that session signing material cannot be reused.
	if bytes.Equal(KDerive("x"), KDerive("y")) {
		t.Error("KDerive ignores the session secret")
	}
}

func TestDeriveTokenIsURLSafeAndHighEntropy(t *testing.T) {
	tok := DeriveToken(KDerive("s"), "app", "claim")
	if len(tok) != 43 {
		t.Errorf("token length = %d, want 43 (32 bytes base64url, unpadded)", len(tok))
	}
	for _, r := range tok {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r) {
			t.Fatalf("token contains a non-URL-safe character %q", r)
		}
	}
}

func TestHashTokenIsHexSHA256(t *testing.T) {
	got := HashToken("token")
	if len(got) != 64 {
		t.Fatalf("hash length = %d, want 64", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("hash is not valid hex: %v", err)
	}
	if HashToken("token") != got {
		t.Error("hash is not deterministic")
	}
	if HashToken("other") == got {
		t.Error("hash collides on different input")
	}
}

func TestPrefixIsShortAndStable(t *testing.T) {
	tok := DeriveToken(KDerive("s"), "app", "claim")
	p := Prefix(tok)
	if len(p) != 8 {
		t.Errorf("prefix length = %d, want 8", len(p))
	}
	if !strings.HasPrefix(tok, p) {
		t.Error("prefix is not a prefix of the token")
	}
	// A prefix must be an identifier, never the whole secret.
	if p == tok {
		t.Error("prefix exposed the entire token")
	}
	if got := Prefix("short"); got != "short" {
		t.Errorf("Prefix on a short value = %q, want it returned unchanged", got)
	}
}

func TestSecretsAreUniqueAndHighEntropy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		s, err := NewClaimSecret()
		if err != nil {
			t.Fatalf("NewClaimSecret: %v", err)
		}
		if len(s) != 43 {
			t.Fatalf("claim secret length = %d, want 43", len(s))
		}
		if seen[s] {
			t.Fatal("NewClaimSecret repeated a value")
		}
		seen[s] = true

		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[tok] {
			t.Fatal("NewToken repeated a value")
		}
		seen[tok] = true
	}
}

func TestNewIDIsUUIDv4AndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if len(id) != 36 {
			t.Fatalf("id length = %d, want 36: %q", len(id), id)
		}
		if id[14] != '4' {
			t.Errorf("id is not version 4: %q", id)
		}
		if seen[id] {
			t.Fatal("NewID repeated a value")
		}
		seen[id] = true
	}
}

func TestEqualHash(t *testing.T) {
	a := HashToken("one")
	if !EqualHash(a, a) {
		t.Error("EqualHash rejected identical hashes")
	}
	if EqualHash(a, HashToken("two")) {
		t.Error("EqualHash accepted different hashes")
	}
	if EqualHash(a, "") || EqualHash("", a) {
		t.Error("EqualHash accepted an empty hash")
	}
}
