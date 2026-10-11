package store

import (
	"testing"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/domain"
)

// TestCreateHandoffApplicationIsApprovedAndClaimable pins the contract the
// one-step hand-off relies on: the row exists as *approved* immediately, with
// the granted lifetime parked on it, and it is claimable without any further
// write.
func TestCreateHandoffApplicationIsApprovedAndClaimable(t *testing.T) {
	s := newTestStore(t)
	const nowMS = int64(1_700_000_000_000)

	secret, err := agentkey.NewClaimSecret()
	if err != nil {
		t.Fatalf("NewClaimSecret: %v", err)
	}

	app, err := s.CreateHandoffApplication(HandoffInput{
		Label:       "A姐",
		Note:        "月报发布用",
		Hours:       24,
		ClaimHash:   agentkey.HashToken(secret),
		RequesterIP: "203.0.113.7",
		UserAgent:   "test-agent",
		LinkTTL:     int64(24 * 60 * 60 * 1000),
	}, nowMS)
	if err != nil {
		t.Fatalf("CreateHandoffApplication: %v", err)
	}

	if app.Status != domain.ApplicationApproved {
		t.Errorf("status = %q, want %q (a hand-off must not sit in the pending queue)",
			app.Status, domain.ApplicationApproved)
	}
	if app.ExpiresAt != nowMS+24*60*60*1000 {
		t.Errorf("expires_at = %d, want %d (now + LinkTTL)", app.ExpiresAt, nowMS+24*60*60*1000)
	}
	if app.ClaimDeadline != app.ExpiresAt {
		t.Errorf("claim_deadline = %d, want %d (the link's clock is the only one that matters)",
			app.ClaimDeadline, app.ExpiresAt)
	}
	if app.DecidedAt != nowMS {
		t.Errorf("decided_at = %d, want %d (the admin decided at generation time)", app.DecidedAt, nowMS)
	}
	if app.ApprovedName != "A姐" || app.ApprovedHours != 24 || app.ApprovedNote != "月报发布用" {
		t.Errorf("approval metadata not parked on the row: %+v", app)
	}

	// The hand-off path must not be gated by the *public* queue cap: an admin
	// minting a link for a colleague is not the flood that cap bounds.
	if _, err := s.CreateApplication(ApplicationInput{
		Label: "flood", ClaimHash: agentkey.HashToken("x"), TTL: 1000,
	}, 1, nowMS); err != nil {
		t.Fatalf("first public application should fit under a cap of 1: %v", err)
	}
	if _, err := s.CreateApplication(ApplicationInput{
		Label: "flood2", ClaimHash: agentkey.HashToken("x"), TTL: 1000,
	}, 1, nowMS); err != ErrMaxPending {
		t.Fatalf("expected the pending cap to bite for the public path, got %v", err)
	}
	if _, err := s.CreateHandoffApplication(HandoffInput{
		Label: "B姐", Hours: 0, ClaimHash: agentkey.HashToken("y"), LinkTTL: 1000,
	}, nowMS); err != nil {
		t.Errorf("hand-off was refused by the pending cap: %v", err)
	}

	// And the credential is derivable straight from the stored hash's secret.
	key, token, err := s.ClaimApplication(app.ID, secret, agentkey.KDerive("session"), 100, nowMS)
	if err != nil {
		t.Fatalf("ClaimApplication: %v", err)
	}
	if token == "" || key == nil {
		t.Fatalf("claim returned no credential: key=%v token=%q", key, token)
	}
	if key.Name != "A姐" {
		t.Errorf("claimed key name = %q, want the hand-off label", key.Name)
	}
	if key.ExpiresAt != nowMS+24*60*60*1000 {
		t.Errorf("claimed key expires_at = %d, want the granted lifetime", key.ExpiresAt)
	}
	if got := agentkey.DeriveToken(agentkey.KDerive("session"), app.ID, secret); got != token {
		t.Errorf("claimed token is not the derived one (derived %q, got %q)", got, token)
	}

	// A second claim must not hand the credential out twice.
	if _, _, err := s.ClaimApplication(app.ID, secret, agentkey.KDerive("session"), 100, nowMS); err == nil {
		t.Error("a hand-off credential was claimed twice")
	}
}

// TestCreateHandoffApplicationNeverStoresTheSecret is the guard for the whole
// design: only the hash may reach the database.
func TestCreateHandoffApplicationNeverStoresTheSecret(t *testing.T) {
	s := newTestStore(t)
	secret := "super-secret-claim-value"

	app, err := s.CreateHandoffApplication(HandoffInput{
		Label: "A姐", ClaimHash: agentkey.HashToken(secret), LinkTTL: 1000,
	}, 1_700_000_000_000)
	if err != nil {
		t.Fatalf("CreateHandoffApplication: %v", err)
	}

	var stored string
	if err := s.db.QueryRow(
		`SELECT claim_hash FROM key_applications WHERE id = ?`, app.ID,
	).Scan(&stored); err != nil {
		t.Fatalf("read back claim_hash: %v", err)
	}
	if stored == secret {
		t.Fatal("the claim secret itself was persisted")
	}
	if stored != agentkey.HashToken(secret) {
		t.Errorf("claim_hash = %q, want sha256 of the secret", stored)
	}

	// No other column may carry it either — a stray copy would be just as bad
	// as storing it in claim_hash.
	var hits int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM key_applications WHERE label = ? OR purpose = ? OR approved_note = ?`,
		secret, secret, secret,
	).Scan(&hits); err != nil {
		t.Fatalf("scan for the secret: %v", err)
	}
	if hits != 0 {
		t.Errorf("the claim secret leaked into %d metadata column(s)", hits)
	}
}
