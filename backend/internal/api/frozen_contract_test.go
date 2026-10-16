package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
)

// The renewal claim secret is derived, not stored: key_renewals deliberately has
// no secret column, so the server recomputes
//
//	HMAC-SHA256(K_derive, "teleport/agent-key/renewal-claim/v1" + "|" + renewalID + "|" + keyID)
//
// instead of keeping the value at rest. Like the token derivation, this is a wire
// contract with a silent failure mode: change the label or the byte layout and
// every renewal link already handed to an agent stops resolving — with no compile
// error and no other failing test.
//
// Until 2026-10-16 no test referenced this label at all.
func TestRenewalClaimDerivationIsFrozen(t *testing.T) {
	const wantLabel = "teleport/agent-key/renewal-claim/v1"
	if renewalClaimLabel != wantLabel {
		t.Errorf("renewalClaimLabel = %q, want %q — rotating this invalidates every outstanding renewal link",
			renewalClaimLabel, wantLabel)
	}

	const (
		kDeriveSeed = "frozen-contract-check"
		renewalID   = "00000000-0000-4000-8000-000000000001"
		keyID       = "00000000-0000-4000-8000-000000000002"
	)
	kDerive := agentkey.KDerive(kDeriveSeed)

	// The exported method, on a server carrying just the derived subkey.
	s := &Server{kDerive: kDerive}
	if got, want := s.renewalClaimSecret(renewalID, keyID),
		"rzqnS_5HH4bnsjrwozfs1Pph-m-jtnQEidz-jQkLZ30"; got != want {
		t.Errorf("renewalClaimSecret = %s, want %s", got, want)
	}

	// And an independent recomputation from the documented formula, sharing no
	// code with the implementation.
	mac := hmac.New(sha256.New, kDerive)
	mac.Write([]byte(wantLabel))
	mac.Write([]byte("|"))
	mac.Write([]byte(renewalID))
	mac.Write([]byte("|"))
	mac.Write([]byte(keyID))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if got := s.renewalClaimSecret(renewalID, keyID); got != want {
		t.Errorf("renewalClaimSecret = %s, want %s (the documented formula)", got, want)
	}

	// The label must actually separate this use from the token use: the same
	// key id under the two labels must not collide.
	if hmac.Equal([]byte(s.renewalClaimSecret(renewalID, keyID)),
		[]byte(agentkey.DeriveToken(kDerive, renewalID, keyID))) {
		t.Error("renewal claim and agent token derivations collided")
	}
}
