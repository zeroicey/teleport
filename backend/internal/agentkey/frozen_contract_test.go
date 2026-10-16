package agentkey

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// Every value below is a WIRE CONTRACT, not an implementation detail.
//
// Agent tokens are *derived*, not stored:
//
//	token = base64url(HMAC-SHA256(K_derive, "teleport/agent-key/v1" + "|" + appID + "|" + claimSecret))
//	K_derive = HMAC-SHA256(SESSION_SECRET, "teleport/derive/agent-key/v1")
//
// The server keeps only sha256(token), so it cannot look a token up — it can only
// recompute one. That means the moment the label strings or the byte layout
// change, **every agent key already in the wild stops authenticating**, and
// nothing else in the system notices: the code still compiles, the other 243
// tests still pass, and the failure appears as a wave of 401s from agents nobody
// is watching.
//
// This test existed as a gap until 2026-10-16 (during the Gin/Postgres rewrite
// planning, an audit found the three derivation labels were referenced by no test
// at all), which is exactly why it now pins both the labels and the resulting
// bytes.
//
// If a value here must change, it is not a test fix: it is a migration. Rotating
// every issued key and re-issuing it to its owner is the only safe way through,
// and the decision record has to say so first.
const (
	frozenSessionSecret = "frozen-contract-check"
	frozenAppID         = "00000000-0000-4000-8000-000000000000"
	frozenClaimSecret   = "frozen-claim-secret"
)

// TestDerivationLabelsAreFrozen pins the three domain-separation strings.
func TestDerivationLabelsAreFrozen(t *testing.T) {
	// The literal on the right is the contract. Do not derive it from the
	// constant on the left — that would make the test tautological.
	if derivationLabel != "teleport/derive/agent-key/v1" {
		t.Errorf("derivationLabel = %q, want %q — rotating this invalidates every issued agent key",
			derivationLabel, "teleport/derive/agent-key/v1")
	}
	if tokenLabel != "teleport/agent-key/v1" {
		t.Errorf("tokenLabel = %q, want %q — rotating this invalidates every issued agent key",
			tokenLabel, "teleport/agent-key/v1")
	}
}

// TestDerivedValuesAreFrozen pins the exact bytes for fixed inputs.
//
// A golden value catches what a scheme test cannot: a changed label is caught by
// the test above, but a changed *order* of the HMAC inputs, a different digest, or
// a different base64 alphabet would all still satisfy the documented formula
// while producing different tokens.
func TestDerivedValuesAreFrozen(t *testing.T) {
	k := KDerive(frozenSessionSecret)
	if got, want := hex.EncodeToString(k),
		"2a1a9228d77483189ce5f77c14ced10bfabe934fb10ac56072e3e3262df190ff"; got != want {
		t.Errorf("KDerive(%q) = %s, want %s", frozenSessionSecret, got, want)
	}

	token := DeriveToken(k, frozenAppID, frozenClaimSecret)
	if got, want := token, "4Zss6umKcSvZRe5oifz0pgasptX1xKOxwwdK9wZOWBY"; got != want {
		t.Errorf("DeriveToken(...) = %s, want %s", got, want)
	}
	if got, want := Prefix(token), "4Zss6umK"; got != want {
		t.Errorf("Prefix(token) = %q, want %q", got, want)
	}
	if got, want := HashToken(token),
		"781a4f7f53d605f512b1b4ee994654d5cf2da6e2d96d33f6360eb98ccf8d277a"; got != want {
		t.Errorf("HashToken(token) = %s, want %s", got, want)
	}
}

// TestDerivationSchemeMatchesTheDocumentedFormula recomputes the token from the
// documented formula using nothing but the standard library.
//
// It is deliberately an independent reimplementation: it shares no code with the
// package, so it fails if the shipped implementation and the comment at the top
// of agentkey.go ever disagree about what the scheme is.
func TestDerivationSchemeMatchesTheDocumentedFormula(t *testing.T) {
	wantK := indepHash([]byte(frozenSessionSecret), "teleport/derive/agent-key/v1")
	if got := KDerive(frozenSessionSecret); !hmac.Equal(got, wantK) {
		t.Errorf("KDerive does not match HMAC-SHA256(SESSION_SECRET, %q)", "teleport/derive/agent-key/v1")
	}

	mac := hmac.New(sha256.New, wantK)
	mac.Write([]byte("teleport/agent-key/v1"))
	mac.Write([]byte("|"))
	mac.Write([]byte(frozenAppID))
	mac.Write([]byte("|"))
	mac.Write([]byte(frozenClaimSecret))
	wantToken := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if got := DeriveToken(wantK, frozenAppID, frozenClaimSecret); got != wantToken {
		t.Errorf("DeriveToken = %s, want %s (the documented formula)", got, wantToken)
	}
	sum := sha256.Sum256([]byte(wantToken))
	if got := HashToken(wantToken); got != hex.EncodeToString(sum[:]) {
		t.Errorf("HashToken is not hex(sha256(token))")
	}
	// The label separation must actually separate: the same secret under two
	// different labels must not produce the same subkey.
	if hmac.Equal(indepHash([]byte(frozenSessionSecret), "teleport/derive/agent-key/v1"),
		indepHash([]byte(frozenSessionSecret), "teleport/agent-key/v1")) {
		t.Error("two different labels produced the same subkey")
	}
}

func indepHash(key []byte, label string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}
