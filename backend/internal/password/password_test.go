package password

import (
	"strings"
	"testing"
)

// TestHashFormatMatchesLegacy pins the stored format, which must stay
// byte-compatible with hashes produced by the previous Workers implementation.
func TestHashFormatMatchesLegacy(t *testing.T) {
	hash, err := Hash("hunter2", 100_000)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 4 {
		t.Fatalf("expected 4 $-separated parts, got %d: %s", len(parts), hash)
	}
	if parts[0] != "pbkdf2" {
		t.Errorf("algorithm = %q, want pbkdf2", parts[0])
	}
	if parts[1] != "100000" {
		t.Errorf("iterations = %q, want 100000", parts[1])
	}
	if strings.ContainsAny(parts[2], "+/=") || strings.ContainsAny(parts[3], "+/=") {
		t.Errorf("salt/key must be base64url without padding: %s", hash)
	}
}

// TestVerifyRoundTrip covers the happy path across iteration counts, including
// the 100 000 value the old deployment was capped at.
func TestVerifyRoundTrip(t *testing.T) {
	for _, iterations := range []int{1, 1_000, 100_000, DefaultIterations} {
		t.Run("iterations", func(t *testing.T) {
			hash, err := Hash("correct horse battery staple", iterations)
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			if !Verify("correct horse battery staple", hash) {
				t.Error("Verify rejected the correct password")
			}
			if Verify("wrong password", hash) {
				t.Error("Verify accepted a wrong password")
			}
			if Verify("", hash) {
				t.Error("Verify accepted an empty password")
			}
			if Verify("correct horse battery staple ", hash) {
				t.Error("Verify accepted a password with trailing whitespace")
			}
		})
	}
}

// TestSaltsAreRandom ensures two hashes of the same password differ.
func TestSaltsAreRandom(t *testing.T) {
	first, err := Hash("same", 1_000)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := Hash("same", 1_000)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if first == second {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
}

// TestVerifyRejectsMalformedHashes ensures a corrupt configuration cannot be
// distinguished from a wrong password, and never panics.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	malformed := []string{
		"",
		"not-a-hash",
		"pbkdf2$1000$onlythree",
		"pbkdf2$abc$c2FsdA$a2V5",
		"pbkdf2$0$c2FsdA$a2V5",
		"pbkdf2$-5$c2FsdA$a2V5",
		"bcrypt$1000$c2FsdA$a2V5",
		"pbkdf2$1000$!!!$a2V5",
		"pbkdf2$1000$c2FsdA$!!!",
		"pbkdf2$1000$$",
	}
	for _, stored := range malformed {
		if Verify("anything", stored) {
			t.Errorf("Verify accepted malformed hash %q", stored)
		}
	}
}

// TestIterationsOf covers the reporting helper used by the startup check.
func TestIterationsOf(t *testing.T) {
	hash, err := Hash("x", 42_000)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if got := IterationsOf(hash); got != 42_000 {
		t.Errorf("IterationsOf = %d, want 42000", got)
	}
	if got := IterationsOf("garbage"); got != 0 {
		t.Errorf("IterationsOf(garbage) = %d, want 0", got)
	}
}

// TestUnicodePasswords covers multi-byte input reaching the KDF intact.
func TestUnicodePasswords(t *testing.T) {
	for _, pw := range []string{"密码测试", "emoji-🔐-pass", "ünïcödé"} {
		hash, err := Hash(pw, 1_000)
		if err != nil {
			t.Fatalf("Hash(%q): %v", pw, err)
		}
		if !Verify(pw, hash) {
			t.Errorf("Verify failed for %q", pw)
		}
	}
}

// TestDefaultIterationsIsStrong guards against a future accidental reduction.
func TestDefaultIterationsIsStrong(t *testing.T) {
	if DefaultIterations < 600_000 {
		t.Errorf("DefaultIterations = %d; OWASP guidance for PBKDF2-HMAC-SHA256 is at least 600000",
			DefaultIterations)
	}
}
