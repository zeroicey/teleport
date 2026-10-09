package store

import (
	"errors"
	"sync"
	"testing"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/domain"
)

const testSessionSecret = "test-session-secret-for-derivation"

func testKDerive() []byte { return agentkey.KDerive(testSessionSecret) }

// apply submits an application and returns it along with the claim secret the
// agent would have been handed.
func apply(t *testing.T, s *Store, label string, nowMS int64) (*domain.KeyApplication, string) {
	t.Helper()
	claim, err := agentkey.NewClaimSecret()
	if err != nil {
		t.Fatalf("NewClaimSecret: %v", err)
	}
	app, err := s.CreateApplication(ApplicationInput{
		Label: label, Purpose: "testing",
		ClaimHash: agentkey.HashToken(claim), TTL: 24 * 3600 * 1000,
	}, 50, nowMS)
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	return app, claim
}

func TestApplicationApprovalAndClaim(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	app, claim := apply(t, s, "reporter", now)
	if app.Status != domain.ApplicationPending {
		t.Fatalf("status = %q, want pending", app.Status)
	}

	if err := s.DecideApplication(app.ID, true, "nightly-agent", 24, "ok", now+30*60*1000, now); err != nil {
		t.Fatalf("DecideApplication: %v", err)
	}

	key, token, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now)
	if err != nil {
		t.Fatalf("ClaimApplication: %v", err)
	}
	if token == "" || key.ID == "" {
		t.Fatal("claim returned an empty key or token")
	}
	if key.Name != "nightly-agent" {
		t.Errorf("key name = %q, want the name the human set", key.Name)
	}
	if key.ExpiresAt != now+24*3600*1000 {
		t.Errorf("expires_at = %d, want the approved 24h from claim time", key.ExpiresAt)
	}

	// The derived token must be the one that actually authenticates, and the
	// database must hold only its hash.
	got, err := s.ResolveAgentKey(agentkey.HashToken(token), now)
	if err != nil {
		t.Fatalf("ResolveAgentKey: %v", err)
	}
	if got == nil || got.ID != key.ID {
		t.Fatalf("the claim token does not resolve to the issued key")
	}

	var stored string
	if err := s.DB().QueryRow(`SELECT token_hash FROM agent_keys WHERE id = ?`, key.ID).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if stored == token {
		t.Fatal("the plaintext token was persisted")
	}
	if stored != agentkey.HashToken(token) {
		t.Errorf("stored hash is not sha256(token)")
	}
}

// The whole point of hashing is that the secret is not in the database. Assert it
// at the storage layer, where it actually matters, across every table.
func TestNoPlaintextCollected(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	app, claim := apply(t, s, "reporter", now)
	if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+60000, now); err != nil {
		t.Fatalf("DecideApplication: %v", err)
	}
	_, token, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now)
	if err != nil {
		t.Fatalf("ClaimApplication: %v", err)
	}
	manualKey, manualToken, err := s.CreateManualKey("manual", 1, "", 100, now)
	if err != nil {
		t.Fatalf("CreateManualKey: %v", err)
	}

	// Dump every text column of every table and look for the two live secrets.
	rows, err := s.DB().Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()

	for _, table := range tables {
		cols, err := s.DB().Query(`SELECT * FROM ` + table)
		if err != nil {
			continue // virtual/system table with no scannable columns
		}
		names, _ := cols.Columns()
		for cols.Next() {
			vals := make([]any, len(names))
			ptrs := make([]any, len(names))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := cols.Scan(ptrs...); err != nil {
				continue
			}
			for i, v := range vals {
				s, ok := v.(string)
				if !ok {
					continue
				}
				if s == token || s == manualToken || s == claim {
					t.Fatalf("%s.%s holds a plaintext secret", table, names[i])
				}
			}
		}
		cols.Close()
	}
	_ = manualKey
}

func TestClaimRejectsWrongSecretAndPreservesTheRealOne(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	app, claim := apply(t, s, "reporter", now)
	if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+60000, now); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ClaimApplication(app.ID, "not-the-secret", testKDerive(), 100, now); !errors.Is(err, ErrClaimMismatch) {
		t.Fatalf("wrong secret gave %v, want ErrClaimMismatch", err)
	}

	// The failed attempt must not have consumed the application: a wrong guess
	// must not be a way to deny the legitimate claimant their credential.
	if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now); err != nil {
		t.Fatalf("legitimate claim after a wrong guess failed: %v", err)
	}
}

func TestClaimIsSingleUse(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	app, claim := apply(t, s, "reporter", now)
	if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+60000, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now); !errors.Is(err, ErrApplicationClaimed) {
		t.Fatalf("second claim gave %v, want ErrApplicationClaimed", err)
	}
}

// The credential is handed out exactly once, under concurrency. This is the test
// that would catch a check-then-act implementation: it must be the conditional
// UPDATE, not a prior SELECT, that decides the winner.
func TestConcurrentClaimHasExactlyOneWinner(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	app, claim := apply(t, s, "reporter", now)
	if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+60000, now); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		wins   int
		tokens []string
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, token, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
				tokens = append(tokens, token)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d concurrent claims succeeded, want exactly 1", wins)
	}

	// And exactly one key row exists, so a losing goroutine cannot have leaked a
	// half-created credential.
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM agent_keys`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("agent_keys has %d rows, want 1", count)
	}
	if resolved, err := s.ResolveAgentKey(agentkey.HashToken(tokens[0]), now); err != nil || resolved == nil {
		t.Fatalf("the winning token does not authenticate: key=%v err=%v", resolved, err)
	}
}

func TestClaimStateMachine(t *testing.T) {
	now := NowMS()

	t.Run("pending cannot be claimed", func(t *testing.T) {
		s := newTestStore(t)
		app, claim := apply(t, s, "r", now)
		// A pending application is indistinguishable from a missing one to a
		// caller, so it must not be claimable and must not leak its state.
		if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now); !errors.Is(err, ErrApplicationMissing) {
			t.Fatalf("got %v, want ErrApplicationMissing", err)
		}
	})

	t.Run("rejected reports rejection", func(t *testing.T) {
		s := newTestStore(t)
		app, claim := apply(t, s, "r", now)
		if err := s.DecideApplication(app.ID, false, "", 0, "", 0, now); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now); !errors.Is(err, ErrApplicationRejected) {
			t.Fatalf("got %v, want ErrApplicationRejected", err)
		}
	})

	t.Run("past the claim window", func(t *testing.T) {
		s := newTestStore(t)
		app, claim := apply(t, s, "r", now)
		if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+1000, now); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 100, now+2000); !errors.Is(err, ErrClaimWindowClosed) {
			t.Fatalf("got %v, want ErrClaimWindowClosed", err)
		}
	})

	t.Run("unknown application", func(t *testing.T) {
		s := newTestStore(t)
		if _, _, err := s.ClaimApplication("nope", "x", testKDerive(), 100, now); !errors.Is(err, ErrApplicationMissing) {
			t.Fatalf("got %v, want ErrApplicationMissing", err)
		}
	})

	t.Run("a decided application cannot be re-decided", func(t *testing.T) {
		s := newTestStore(t)
		app, _ := apply(t, s, "r", now)
		if err := s.DecideApplication(app.ID, false, "", 0, "", 0, now); err != nil {
			t.Fatal(err)
		}
		// Re-approving a rejection would let a second click undo a security decision.
		if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+1000, now); !errors.Is(err, ErrApplicationDecided) {
			t.Fatalf("got %v, want ErrApplicationDecided", err)
		}
	})
}

func TestExpireApplications(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	claim, _ := agentkey.NewClaimSecret()
	if _, err := s.CreateApplication(ApplicationInput{
		Label: "stale", ClaimHash: agentkey.HashToken(claim), TTL: 1000,
	}, 50, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpireApplications(now + 5000); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountApplications("expired"); err != nil || n != 1 {
		t.Fatalf("expired count = %d (err %v), want 1", n, err)
	}
	if n, err := s.CountApplications("pending"); err != nil || n != 0 {
		t.Fatalf("pending count = %d (err %v), want 0", n, err)
	}
}

func TestPendingCap(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	for i := 0; i < 3; i++ {
		if _, err := s.CreateApplication(ApplicationInput{
			Label: "x", ClaimHash: agentkey.HashToken("h"), TTL: 1000,
		}, 3, now); err != nil {
			t.Fatalf("application %d: %v", i, err)
		}
	}
	if _, err := s.CreateApplication(ApplicationInput{
		Label: "x", ClaimHash: agentkey.HashToken("h"), TTL: 1000,
	}, 3, now); !errors.Is(err, ErrMaxPending) {
		t.Fatalf("got %v, want ErrMaxPending", err)
	}
}

func TestResolveAgentKeyHonoursExpiryAndRevocation(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	key, token, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	hash := agentkey.HashToken(token)

	if got, err := s.ResolveAgentKey(hash, now); err != nil || got == nil {
		t.Fatalf("fresh key did not resolve: %v %v", got, err)
	}
	// One hour + one millisecond later, the 1-hour key is gone.
	if got, err := s.ResolveAgentKey(hash, now+3_600_001); err != nil || got != nil {
		t.Fatalf("expired key resolved: %v %v", got, err)
	}

	// Revocation is absolute: it must hold even well before the expiry.
	if _, err := s.UpdateAgentKey(key.ID, KeyPatch{Revoked: boolPtr(true)}, now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ResolveAgentKey(hash, now); err != nil || got != nil {
		t.Fatalf("revoked key resolved: %v %v", got, err)
	}

	// Un-revoke restores it, because a misclick should be recoverable.
	if _, err := s.UpdateAgentKey(key.ID, KeyPatch{Revoked: boolPtr(false)}, now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ResolveAgentKey(hash, now); err != nil || got == nil {
		t.Fatalf("un-revoked key did not resolve: %v %v", got, err)
	}

	// A never-expiring key stays valid arbitrarily far out.
	forever, foreverToken, err := s.CreateManualKey("forever", 0, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if forever.ExpiresAt != 0 {
		t.Errorf("expires_at = %d, want 0 for a never-expiring key", forever.ExpiresAt)
	}
	if got, err := s.ResolveAgentKey(agentkey.HashToken(foreverToken), now+100*365*24*3600*1000); err != nil || got == nil {
		t.Fatalf("never-expiring key did not resolve in the far future: %v %v", got, err)
	}
}

func TestTouchAgentKeyCounts(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.TouchAgentKey(key.ID, now+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetAgentKey(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestCount != 3 {
		t.Errorf("request_count = %d, want 3", got.RequestCount)
	}
	if got.LastUsedAt != now+2 {
		t.Errorf("last_used_at = %d, want %d", got.LastUsedAt, now+2)
	}
}

func TestMaxActiveKeys(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	if _, _, err := s.CreateManualKey("a", 1, "", 1, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateManualKey("b", 1, "", 1, now); !errors.Is(err, ErrMaxActive) {
		t.Fatalf("got %v, want ErrMaxActive", err)
	}
}

func TestOwnership(t *testing.T) {
	s := newTestStore(t)

	saved, share, err := s.CreateReport(domain.CreateReportInput{
		Title: "owned", Format: domain.FormatMarkdown, Content: "body",
		OwnerKeyID: "key-a",
	}, 1)
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	owner, found, err := s.ReportOwner(saved.ID)
	if err != nil || !found {
		t.Fatalf("ReportOwner: owner=%q found=%v err=%v", owner, found, err)
	}
	if owner != "key-a" {
		t.Errorf("owner = %q, want key-a", owner)
	}

	// A report with no owner is what root publishes, and it must round-trip as
	// the empty string rather than being mistaken for a missing report.
	rootOwned, _, err := s.CreateReport(domain.CreateReportInput{
		Title: "root", Format: domain.FormatMarkdown, Content: "body",
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	owner, found, err = s.ReportOwner(rootOwned.ID)
	if err != nil || !found || owner != "" {
		t.Fatalf("root report: owner=%q found=%v err=%v", owner, found, err)
	}

	if _, found, _ := s.ReportOwner("does-not-exist"); found {
		t.Error("ReportOwner claimed a missing report exists")
	}

	tokenOwner, active, found, err := s.ShareTokenOwner(share.Token)
	if err != nil || !found {
		t.Fatalf("ShareTokenOwner: found=%v err=%v", found, err)
	}
	if tokenOwner != "key-a" || !active {
		t.Errorf("share owner = %q active=%v, want key-a active", tokenOwner, active)
	}
	if _, _, found, _ := s.ShareTokenOwner("nope"); found {
		t.Error("ShareTokenOwner claimed a missing token exists")
	}
}

func TestRenewalLifecycle(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}

	renewal, err := s.CreateRenewal(key.ID, 48, now)
	if err != nil {
		t.Fatalf("CreateRenewal: %v", err)
	}
	// A second pending renewal would bury the approval queue and force a human to
	// reconcile competing extensions of the same key.
	if _, err := s.CreateRenewal(key.ID, 48, now); !errors.Is(err, ErrRenewalExists) {
		t.Fatalf("got %v, want ErrRenewalExists", err)
	}

	got, err := s.DecideRenewal(renewal.ID, true, 48, now)
	if err != nil {
		t.Fatalf("DecideRenewal: %v", err)
	}
	// The key was created with 1 hour, and renewing grants 48 more measured from
	// the later of (current expiry, now) — so it ends at now + 1h + 48h. A
	// renewal that reset the expiry to now+48h would silently delete the hour the
	// key already had, punishing exactly the early renewal the guide recommends.
	want := now + 49*3600*1000
	if got.GrantedExpiresAt != want {
		t.Errorf("granted = %d, want %d (now + remaining 1h + granted 48h)", got.GrantedExpiresAt, want)
	}
	after, err := s.GetAgentKey(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExpiresAt != want {
		t.Errorf("key expires_at = %d, want %d", after.ExpiresAt, want)
	}
	// And a renewal must never shorten a key that still has time left.
	if after.ExpiresAt <= now+48*3600*1000 {
		t.Error("renewal produced an expiry no better than a plain reset from now")
	}

	if _, err := s.DecideRenewal(renewal.ID, true, 1, now); !errors.Is(err, ErrApplicationDecided) {
		t.Fatalf("re-deciding gave %v, want ErrApplicationDecided", err)
	}
	if _, err := s.DecideRenewal("nope", true, 1, now); !errors.Is(err, ErrRenewalMissing) {
		t.Fatalf("got %v, want ErrRenewalMissing", err)
	}
}

func TestRenewalRejectionLeavesKeyAlone(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetAgentKey(key.ID)

	renewal, err := s.CreateRenewal(key.ID, 48, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideRenewal(renewal.ID, false, 0, now); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetAgentKey(key.ID)
	if after.ExpiresAt != before.ExpiresAt {
		t.Errorf("a rejected renewal changed the expiry: %d -> %d", before.ExpiresAt, after.ExpiresAt)
	}

	// A rejected renewal must not block a future attempt.
	if _, err := s.CreateRenewal(key.ID, 48, now); err != nil {
		t.Fatalf("a new renewal after rejection failed: %v", err)
	}
}

func TestRevokedKeyCannotBeRenewed(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAgentKey(key.ID, KeyPatch{Revoked: boolPtr(true)}, now); err != nil {
		t.Fatal(err)
	}
	renewal, err := s.CreateRenewal(key.ID, 48, now)
	if err != nil {
		t.Fatal(err)
	}
	// Revocation is a deliberate human act; a renewal must not quietly undo it.
	if _, err := s.DecideRenewal(renewal.ID, true, 48, now); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("got %v, want ErrKeyMissing", err)
	}
}

func TestNeverExpiringRenewalGrant(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	key, _, err := s.CreateManualKey("k", 1, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := s.CreateRenewal(key.ID, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DecideRenewal(renewal.ID, true, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	// 0 hours means "never", which must be representable as a grant, not ignored.
	if got.GrantedExpiresAt != 0 {
		t.Errorf("granted = %d, want 0 (never expires)", got.GrantedExpiresAt)
	}
	after, _ := s.GetAgentKey(key.ID)
	if after.ExpiresAt != 0 {
		t.Errorf("key expires_at = %d, want 0", after.ExpiresAt)
	}
}

// The active-key ceiling must be enforced inside the claim transaction. A caller
// that counts first and claims second would let N concurrent claims each observe
// a count below the cap and all succeed.
func TestClaimEnforcesActiveCapAtomically(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	// Occupy the only slot.
	holder, _, err := s.CreateManualKey("holder", 0, "", 100, now)
	if err != nil {
		t.Fatal(err)
	}

	app, claim := apply(t, s, "reporter", now)
	if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+60000, now); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 1, now); !errors.Is(err, ErrMaxActive) {
		t.Fatalf("claim at the cap gave %v, want ErrMaxActive", err)
	}

	// Being turned away must not burn the credential: the same application is
	// still claimable once a slot frees up.
	if _, err := s.UpdateAgentKey(holder.ID, KeyPatch{Revoked: boolPtr(true)}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimApplication(app.ID, claim, testKDerive(), 1, now); err != nil {
		t.Fatalf("claim after freeing a slot failed: %v", err)
	}
}

// Concurrent claimers must not all slip past the cap. With one slot free and
// several approved applications racing, exactly one may succeed.
func TestConcurrentClaimsRespectActiveCap(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()

	const racers = 6
	type pending struct {
		id    string
		claim string
	}
	var apps []pending
	for i := 0; i < racers; i++ {
		app, claim := apply(t, s, "reporter", now)
		if err := s.DecideApplication(app.ID, true, "agent", 1, "", now+60000, now); err != nil {
			t.Fatal(err)
		}
		apps = append(apps, pending{app.ID, claim})
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		other []error
	)
	start := make(chan struct{})
	for _, a := range apps {
		wg.Add(1)
		go func(a pending) {
			defer wg.Done()
			<-start
			_, _, err := s.ClaimApplication(a.id, a.claim, testKDerive(), 1, now)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				other = append(other, err)
			}
		}(a)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d of %d concurrent claims succeeded, want exactly 1 (cap=1)", wins, racers)
	}
	for _, err := range other {
		if !errors.Is(err, ErrMaxActive) {
			t.Errorf("a losing claim failed with %v, want ErrMaxActive", err)
		}
	}
	var active int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM agent_keys WHERE revoked_at = 0`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("%d active keys exist, want 1", active)
	}
}

// A flood of concurrent applications must not overshoot the pending cap, and the
// losers must get a clean ErrMaxPending — not a raw SQLite error, which the API
// would surface as a 500 under exactly the attack the cap exists to absorb.
//
// CreateApplication counts and inserts inside one transaction, but that
// transaction is deferred, so it upgrades from a read snapshot to a write lock.
// In WAL mode such an upgrade can fail with SQLITE_BUSY_SNAPSHOT when another
// writer commits in between (busy_timeout does not cover that case), which would
// turn a rejected application into a 500. Measured at 24 goroutines over 20
// rounds, every loser received ErrMaxPending, so the window is not reachable
// here. This test pins that property: if the behaviour ever regresses to a raw
// error, it fails rather than silently becoming a 500 in production.
func TestConcurrentApplicationsRespectPendingCap(t *testing.T) {
	s := newTestStore(t)
	now := NowMS()
	const cap = 3
	const racers = 24

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		unclean []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.CreateApplication(ApplicationInput{
				Label: "flood", ClaimHash: agentkey.HashToken("h"), TTL: 3600 * 1000,
			}, cap, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, ErrMaxPending):
				unclean = append(unclean, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != cap {
		t.Errorf("%d applications succeeded, want exactly %d", wins, cap)
	}
	for _, err := range unclean {
		t.Errorf("a rejected application returned %v, want ErrMaxPending "+
			"(a raw error here becomes a 500 in production)", err)
	}
	var stored int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM key_applications`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != cap {
		t.Errorf("%d applications stored, want %d", stored, cap)
	}
}

// Renewal semantics deserve their own cases: "extends, never shortens" is only
// meaningful if the never-expires and late-renewal ends are pinned too.
func TestRenewalOnlyExtends(t *testing.T) {
	now := NowMS()

	t.Run("early renewal keeps the remaining time", func(t *testing.T) {
		s := newTestStore(t)
		// A key with 10 days left, renewed for 10 more, must end at 20 days —
		// not at 10, which is what granting from `now` would produce.
		key, _, err := s.CreateManualKey("k", 240, "", 100, now)
		if err != nil {
			t.Fatal(err)
		}
		renewal, err := s.CreateRenewal(key.ID, 240, now)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.DecideRenewal(renewal.ID, true, 240, now)
		if err != nil {
			t.Fatal(err)
		}
		want := now + 480*3600*1000
		if got.GrantedExpiresAt != want {
			t.Errorf("granted = %d, want %d (10d remaining + 10d granted)", got.GrantedExpiresAt, want)
		}
	})

	t.Run("late renewal yields a fully future window", func(t *testing.T) {
		s := newTestStore(t)
		key, _, err := s.CreateManualKey("k", 1, "", 100, now)
		if err != nil {
			t.Fatal(err)
		}
		renewal, err := s.CreateRenewal(key.ID, 5, now)
		if err != nil {
			t.Fatal(err)
		}
		// Already past the old expiry: the grant must be now + 5h, not old + 5h.
		late := now + 10*3600*1000
		got, err := s.DecideRenewal(renewal.ID, true, 5, late)
		if err != nil {
			t.Fatal(err)
		}
		if want := late + 5*3600*1000; got.GrantedExpiresAt != want {
			t.Errorf("granted = %d, want %d", got.GrantedExpiresAt, want)
		}
	})

	t.Run("a never-expiring key is never downgraded by a renewal", func(t *testing.T) {
		s := newTestStore(t)
		key, _, err := s.CreateManualKey("forever", 0, "", 100, now)
		if err != nil {
			t.Fatal(err)
		}
		renewal, err := s.CreateRenewal(key.ID, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		// 0 means unbounded, not "expired at the epoch". A renewal must not turn a
		// permanent key into one that dies in an hour.
		got, err := s.DecideRenewal(renewal.ID, true, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.GrantedExpiresAt != 0 {
			t.Errorf("granted = %d, want 0 (still never expires)", got.GrantedExpiresAt)
		}
		after, _ := s.GetAgentKey(key.ID)
		if after.ExpiresAt != 0 {
			t.Errorf("key expires_at = %d, want 0", after.ExpiresAt)
		}
	})

	t.Run("a finite key can be granted permanence", func(t *testing.T) {
		s := newTestStore(t)
		key, _, err := s.CreateManualKey("k", 1, "", 100, now)
		if err != nil {
			t.Fatal(err)
		}
		renewal, err := s.CreateRenewal(key.ID, 0, now)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.DecideRenewal(renewal.ID, true, 0, now)
		if err != nil {
			t.Fatal(err)
		}
		// The human chose "never", which is an extension, not a downgrade.
		if got.GrantedExpiresAt != 0 {
			t.Errorf("granted = %d, want 0", got.GrantedExpiresAt)
		}
		after, _ := s.GetAgentKey(key.ID)
		if after.ExpiresAt != 0 {
			t.Errorf("key expires_at = %d, want 0", after.ExpiresAt)
		}
	})
}
