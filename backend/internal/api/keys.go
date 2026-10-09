package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/zeroicey/teleport/backend/internal/agentkey"
	"github.com/zeroicey/teleport/backend/internal/domain"
	"github.com/zeroicey/teleport/backend/internal/httpx"
	"github.com/zeroicey/teleport/backend/internal/store"
)

// claimHeader carries the secret an unauthenticated caller proves it owns an
// application (or renewal) with. Missing and wrong are answered identically.
const claimHeader = "X-Teleport-Claim"

// pollIntervalSeconds is the advertised polling cadence for a pending
// application. It is a hint, not a lease: the endpoint is cheap and idempotent.
const pollIntervalSeconds = 5

// renewalClaimLabel domain-separates renewal poll secrets from agent-key token
// derivation, so a value valid in one context can never be replayed in the other.
const renewalClaimLabel = "teleport/agent-key/renewal-claim/v1"

// agentAuthFailed mirrors httpx's single 401 message. Handlers only reach for it
// if the auth middleware was bypassed, which would be a wiring bug — the message
// must still be the same one so no response distinguishes the failure reasons.
const agentAuthFailed = "Invalid agent credentials"

// defaultApprovedKeyHours is the lifetime a human grants when they approve an
// application without naming one.
//
// It is deliberately NOT the applicant's requestedHours. That field arrives over
// the unauthenticated application endpoint, so inheriting it would let a caller
// choose the lifetime of the credential a human is about to issue — and
// requestedHours=0 ("unspecified") would silently mean "never expires". Absent
// means this default; 0 means never only when the admin types it explicitly.
//
// It is also deliberately not DefaultShareHours: share-link TTL and credential
// lifetime are different policies, and coupling them would make a change to one
// silently rewrite the other.
const defaultApprovedKeyHours = 24

// maxKeyLifetimeHours caps every hour count the API accepts (100 years). The
// point is to keep hours*3_600_000 inside int64 so a lifetime can never saturate
// into a negative timestamp.
const maxKeyLifetimeHours = 100 * 365 * 24

// keyResolver adapts the store to httpx.KeyResolver.
//
// It is the bridge between "a bearer token was presented" and "which key row is
// that": the middleware hashes the token and hands over only the digest, so the
// plaintext never reaches this layer.
type keyResolver struct {
	store *store.Store
}

// Resolve looks the hash up and turns a hit into a Principal.
//
// Failures are deliberately not distinguished: an unknown, revoked and expired
// key all yield ok=false, matching what the middleware must tell the caller
// (nothing).
func (r keyResolver) Resolve(ctx context.Context, tokenHash string) (domain.Principal, bool) {
	nowMS := store.NowMS()
	key, err := r.store.ResolveAgentKey(tokenHash, nowMS)
	if err != nil {
		// A database failure must fail closed *and* stay invisible on the wire:
		// the middleware answers with the same 401 as a bad token.
		slog.ErrorContext(ctx, "resolve agent key failed", "error", err)
		return domain.Principal{}, false
	}
	if key == nil {
		return domain.Principal{}, false
	}

	// Usage analytics, not authorization. A failed counter write must not turn a
	// valid credential into a 401 — being unable to record a use is not a reason
	// to reject the request — so it is logged and swallowed.
	if err := r.store.TouchAgentKey(key.ID, store.NowMS()); err != nil {
		slog.WarnContext(ctx, "touch agent key failed", "keyId", key.ID, "error", err)
	}
	return domain.Principal{KeyID: key.ID, Name: key.Name}, true
}

// limitKeyApplications throttles the public application endpoint per source IP.
//
// It is a middleware rather than an in-handler check so the quota is consumed
// before the body is read or parsed: a malformed flood is exactly the traffic
// this exists to bound.
//
// The bucket key comes from ClientIP, not RealIP: RealIP believes forwarded
// headers unconditionally, so with no CDN in front a caller could mint a fresh
// bucket per request by rotating X-Forwarded-For. ClientIP ignores every header
// unless the immediate peer is a known proxy.
func (s *Server) limitKeyApplications(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.applyLimiter.allow(httpx.ClientIP(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(s.applyLimiter.retryAfterSeconds()))
			httpx.Fail(w, r, http.StatusTooManyRequests, "rate_limited",
				"Too many key applications from this address; try again later", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// expireApplications closes stale rows before a read that would otherwise show
// them (the approval queue and the public poll endpoints).
//
// There is no scheduler: doing it opportunistically keeps the binary free of
// background goroutines, and a stale row left in place would occupy a
// KEY_MAX_PENDING slot and sit in the queue forever.
func (s *Server) expireApplications(nowMS int64) {
	if _, err := s.store.ExpireApplications(nowMS); err != nil {
		slog.Warn("expire key applications failed", "error", err)
	}
}

// ---------------------------------------------------------------------------
// public: application + renewal polling
// ---------------------------------------------------------------------------

// handleCreateKeyApplication is the open entry point: any agent may ask for a
// credential, and a human decides whether to grant it.
func (s *Server) handleCreateKeyApplication(w http.ResponseWriter, r *http.Request) {
	body, err := readJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	label, err := requireVisibleString(body["label"], "label", 1, 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	purpose := ""
	if v, ok := body["purpose"]; ok && v != nil {
		if purpose, err = requireVisibleText(v, "purpose", 0, 500); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	hours, err := readOptionalHours(body, "requestedHours")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// The server owns the entropy: the caller never proposes the secret, so it
	// cannot weaken the derived token.
	claimSecret, err := agentkey.NewClaimSecret()
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to generate claim secret").WithCause(err))
		return
	}

	nowMS := store.NowMS()
	s.expireApplications(nowMS)

	app, err := s.store.CreateApplication(store.ApplicationInput{
		Label:          label,
		Purpose:        purpose,
		RequestedHours: hours,
		// The audit field must come from the same source as the rate-limit
		// bucket: an approver reading "requester_ip" has to be able to trust it,
		// and with RealIP this column would have recorded whatever the caller
		// typed into a header — usable to frame an innocent address.
		RequesterIP: httpx.ClientIP(r),
		UserAgent:   truncate(r.UserAgent(), 512),
		ClaimHash:   agentkey.HashToken(claimSecret),
		TTL:         s.cfg.KeyApplicationTTL.Milliseconds(),
	}, s.cfg.KeyMaxPending, nowMS)
	if err != nil {
		writeKeyLifecycleError(w, r, err)
		return
	}

	// claim_secret is emitted here and nowhere else. It is not recoverable: only
	// its hash is stored, so a lost secret means an abandoned application.
	httpx.OK(w, r, map[string]any{
		"id":                    app.ID,
		"claim_secret":          claimSecret,
		"status":                string(app.Status),
		"created_at":            app.CreatedAt,
		"expires_at":            app.ExpiresAt,
		"poll_interval_seconds": pollIntervalSeconds,
	}, http.StatusCreated)
}

// handlePollKeyApplication is how an agent learns the decision, and the one
// moment a plaintext credential exists.
func (s *Server) handlePollKeyApplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	provided := r.Header.Get(claimHeader)
	// No id and no secret are the same answer as a secret that does not match:
	// 404. Anything else would confirm that an application id exists.
	if id == "" || provided == "" {
		httpx.WriteError(w, r, httpx.NotFound("Key application not found"))
		return
	}

	nowMS := store.NowMS()
	s.expireApplications(nowMS)

	app, err := s.store.GetApplication(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load key application").WithCause(err))
		return
	}
	if app == nil {
		httpx.WriteError(w, r, httpx.NotFound("Key application not found"))
		return
	}
	// Constant-time comparison of two hashes: a plain == would leak the stored
	// hash byte by byte through response timing.
	if !agentkey.EqualHash(app.ClaimHash, agentkey.HashToken(provided)) {
		httpx.WriteError(w, r, httpx.NotFound("Key application not found"))
		return
	}

	switch app.Status {
	case domain.ApplicationPending, domain.ApplicationRejected:
		httpx.OK(w, r, publicApplication(app))
	case domain.ApplicationExpired:
		httpx.WriteError(w, r, httpx.Gone("This key application has expired"))
	case domain.ApplicationClaimed:
		// Already collected. Saying so would confirm the holder's guess of an
		// application id, so it is folded into "not found".
		httpx.WriteError(w, r, httpx.NotFound("Key application not found"))
	case domain.ApplicationApproved:
		// KEY_MAX_ACTIVE is enforced inside the store's claim transaction, so a
		// concurrent burst cannot slip past the cap, and a claim refused for that
		// reason rolls the status flip back — the application is not consumed and
		// the same claim secret can retry once a slot frees up.
		key, token, err := s.store.ClaimApplication(id, provided, s.kDerive, s.cfg.KeyMaxActive, nowMS)
		if err != nil {
			writeKeyLifecycleError(w, r, err)
			return
		}
		payload := publicApplication(app)
		payload["status"] = string(domain.ApplicationApproved)
		payload["key"] = claimedKeyPayload(key, token)
		httpx.OK(w, r, payload)
	default:
		httpx.WriteError(w, r, httpx.NotFound("Key application not found"))
	}
}

// handlePollRenewal exposes a renewal's outcome to the agent that filed it.
func (s *Server) handlePollRenewal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	provided := r.Header.Get(claimHeader)
	if id == "" || provided == "" {
		httpx.WriteError(w, r, httpx.NotFound("Renewal not found"))
		return
	}

	renewal, err := s.store.GetRenewal(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load renewal").WithCause(err))
		return
	}
	if renewal == nil {
		httpx.WriteError(w, r, httpx.NotFound("Renewal not found"))
		return
	}
	expected := s.renewalClaimSecret(renewal.ID, renewal.KeyID)
	if !agentkey.EqualHash(agentkey.HashToken(provided), agentkey.HashToken(expected)) {
		httpx.WriteError(w, r, httpx.NotFound("Renewal not found"))
		return
	}
	httpx.OK(w, r, renewal)
}

// renewalClaimSecret derives the poll secret for a renewal request.
//
// key_renewals has no claim-secret column on purpose: the schema is frozen, and
// storing one would reintroduce exactly the at-rest secret the derivation design
// exists to avoid. The value is therefore recomputable from the same K_derive
// subkey the tokens use — the server never has to persist it, and the agent only
// ever holds the result.
//
// This is a deliberate design choice, not a missing column. The secret depends
// on K_derive, which exists only on the server, so learning a renewal id and its
// key id is not enough to reconstruct it; and filing a renewal already requires
// the agent's bearer token. Adding claim_hash to key_renewals would buy nothing
// the derivation does not already give, while putting a secret back at rest.
func (s *Server) renewalClaimSecret(renewalID, keyID string) string {
	mac := hmac.New(sha256.New, s.kDerive)
	mac.Write([]byte(renewalClaimLabel))
	mac.Write([]byte("|"))
	mac.Write([]byte(renewalID))
	mac.Write([]byte("|"))
	mac.Write([]byte(keyID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// agent-owned
// ---------------------------------------------------------------------------

// keyMeResponse is the agent's view of its own credential.
//
// It is an explicit projection rather than an embedded AgentKey: `note` is the
// administrator's internal annotation ("who this key was issued to and why") and
// must not be handed to the key holder, and `revoked_at` cannot be non-zero on a
// request that authenticated (the resolver refuses revoked keys), so exposing it
// would only invite a caller to infer state it can never observe. Spelling the
// fields out makes the exclusion visible instead of relying on a future tag.
type keyMeResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	TokenPrefix  string `json:"token_prefix"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at"` // 0 = never expires
	LastUsedAt   int64  `json:"last_used_at"`
	RequestCount int64  `json:"request_count"`
	Root         bool   `json:"root"`
}

// handleKeyMe reports the caller's own credential. Root has no key row, so it
// gets a synthetic identity instead of a 404.
func (s *Server) handleKeyMe(w http.ResponseWriter, r *http.Request) {
	principal := httpx.PrincipalFrom(r.Context())
	if principal == nil {
		httpx.WriteError(w, r, httpx.Unauthorized(agentAuthFailed))
		return
	}
	if principal.Root {
		httpx.OK(w, r, keyMeResponse{
			Name: "root (break-glass)",
			Root: true,
		})
		return
	}

	key, err := s.store.GetAgentKey(principal.KeyID)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load key").WithCause(err))
		return
	}
	if key == nil {
		// Revoked or deleted between authentication and this read.
		httpx.WriteError(w, r, httpx.NotFound("Key not found"))
		return
	}
	httpx.OK(w, r, keyMeResponse{
		ID:           key.ID,
		Name:         key.Name,
		TokenPrefix:  key.TokenPrefix,
		CreatedAt:    key.CreatedAt,
		ExpiresAt:    key.ExpiresAt,
		LastUsedAt:   key.LastUsedAt,
		RequestCount: key.RequestCount,
	})
}

// handleCreateRenewal lets a key ask for more time. It stays pending until a
// human approves it — a key that could extend itself would make expiry
// decorative.
func (s *Server) handleCreateRenewal(w http.ResponseWriter, r *http.Request) {
	principal := httpx.PrincipalFrom(r.Context())
	if principal == nil {
		httpx.WriteError(w, r, httpx.Unauthorized(agentAuthFailed))
		return
	}
	if principal.Root {
		// Root is a static environment credential: there is nothing to renew.
		httpx.WriteError(w, r, httpx.NotFound("No key to renew"))
		return
	}

	body, err := readOptionalJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	hours, err := readOptionalHours(body, "requestedHours")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	renewal, err := s.store.CreateRenewal(principal.KeyID, hours, store.NowMS())
	if err != nil {
		writeKeyLifecycleError(w, r, err)
		return
	}
	httpx.OK(w, r, s.renewalCreatedPayload(renewal), http.StatusCreated)
}

// ---------------------------------------------------------------------------
// dashboard
// ---------------------------------------------------------------------------

// handleAdminListKeyApplications drives the approval queue. Expiry runs first so
// the queue never shows a row that is already dead.
func (s *Server) handleAdminListKeyApplications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := strings.TrimSpace(q.Get("status"))
	if status == "" {
		status = string(domain.ApplicationPending)
	}
	if !validApplicationStatus(status) {
		httpx.WriteError(w, r, httpx.BadRequest(
			"`status` must be one of: pending, approved, rejected, claimed, expired",
			map[string]any{"field": "status"}))
		return
	}
	limit := clampInt(q.Get("limit"), 100, 1, 500)

	nowMS := store.NowMS()
	s.expireApplications(nowMS)

	apps, err := s.store.ListApplications(status, limit)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list key applications").WithCause(err))
		return
	}
	if apps == nil {
		apps = []domain.KeyApplication{}
	}
	httpx.OK(w, r, apps)
}

// handleAdminApproveKeyApplication parks the granted name/lifetime on the
// application. It deliberately does not create the key: the token is derived
// from the claim secret, which the server only holds a hash of until the agent
// claims it. So "approving adds no key row yet" is the contract, not a bug.
func (s *Server) handleAdminApproveKeyApplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, err := readOptionalJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	name := ""
	if v, ok := body["name"]; ok && v != nil {
		if name, err = requireVisibleString(v, "name", 1, 100); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	note := ""
	if v, ok := body["note"]; ok && v != nil {
		if note, err = requireVisibleText(v, "note", 0, 500); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	// Absent `expiresInHours` means the server default; an explicit 0 means
	// "never expires". The two must not collapse into one — and the default must
	// not be the applicant's requestedHours, which arrives unauthenticated and
	// would let a caller pick the lifetime of the credential (with 0 meaning
	// "forever") simply by leaving the admin's field empty.
	var hours float64
	if v, ok := body["expiresInHours"]; ok && v != nil {
		if hours, err = nonNegativeHours(v, "expiresInHours"); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	} else {
		hours = defaultApprovedKeyHours
	}

	nowMS := store.NowMS()
	s.expireApplications(nowMS)
	deadline := nowMS + s.cfg.KeyClaimWindow.Milliseconds()
	if err := s.store.DecideApplication(id, true, name, hours, note, deadline, nowMS); err != nil {
		s.writeDecisionError(w, r, id, err)
		return
	}
	s.writeApplicationResult(w, r, id)
}

// handleAdminRejectKeyApplication transitions a pending application to rejected.
func (s *Server) handleAdminRejectKeyApplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, err := readOptionalJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	reason := ""
	if v, ok := body["reason"]; ok && v != nil {
		if reason, err = requireVisibleText(v, "reason", 0, 500); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	nowMS := store.NowMS()
	s.expireApplications(nowMS)
	if err := s.store.DecideApplication(id, false, "", 0, reason, 0, nowMS); err != nil {
		s.writeDecisionError(w, r, id, err)
		return
	}
	s.writeApplicationResult(w, r, id)
}

// handleAdminListKeys returns every key, active or not. TokenHash is json:"-"
// on the domain type, so it cannot be serialized even by accident, and the
// plaintext does not exist server-side at all.
func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAgentKeys()
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list keys").WithCause(err))
		return
	}
	if keys == nil {
		keys = []domain.AgentKey{}
	}
	httpx.OK(w, r, keys)
}

// handleAdminCreateKey mints a credential outright, for handing one to an agent
// that cannot poll. This response is one of only two places a plaintext token
// ever appears.
func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	body, err := readJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	name, err := requireVisibleString(body["name"], "name", 1, 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	hours, err := readOptionalHours(body, "expiresInHours")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	note := ""
	if v, ok := body["note"]; ok && v != nil {
		if note, err = requireVisibleText(v, "note", 0, 500); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	key, token, err := s.store.CreateManualKey(name, hours, note, s.cfg.KeyMaxActive, store.NowMS())
	if err != nil {
		writeKeyLifecycleError(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"token": token, "key": key}, http.StatusCreated)
}

// handleAdminPatchKey renames a key, changes its expiry (which is the human's
// direct renewal path) or revokes it.
func (s *Server) handleAdminPatchKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, err := readJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	patch := store.KeyPatch{}
	if v, ok := body["name"]; ok && v != nil {
		name, err := requireVisibleString(v, "name", 1, 100)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		patch.Name = &name
	}
	if v, ok := body["expiresInHours"]; ok && v != nil {
		hours, err := nonNegativeHours(v, "expiresInHours")
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		// 0 means "never expires", which is an absolute timestamp of 0.
		expiresAt := store.HoursFromNow(hours, 0)
		patch.ExpiresAt = &expiresAt
	}
	if v, ok := body["revoked"]; ok && v != nil {
		revoked, ok := v.(bool)
		if !ok {
			httpx.WriteError(w, r, httpx.BadRequest("`revoked` must be a boolean",
				map[string]any{"field": "revoked"}))
			return
		}
		patch.Revoked = &revoked
	}
	if v, ok := body["note"]; ok && v != nil {
		note, err := requireVisibleText(v, "note", 0, 500)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		patch.Note = &note
	}
	if patch.Name == nil && patch.ExpiresAt == nil && patch.Revoked == nil && patch.Note == nil {
		httpx.WriteError(w, r, httpx.BadRequest(
			"Provide at least one of: name, expiresInHours, revoked, note", nil))
		return
	}

	key, err := s.store.UpdateAgentKey(id, patch, store.NowMS())
	if err != nil {
		writeKeyLifecycleError(w, r, err)
		return
	}
	if key == nil {
		httpx.WriteError(w, r, httpx.NotFound("Key not found"))
		return
	}
	httpx.OK(w, r, key)
}

// handleAdminListRenewals drives the renewal approval queue.
func (s *Server) handleAdminListRenewals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := strings.TrimSpace(q.Get("status"))
	if status == "" {
		status = string(domain.RenewalPending)
	}
	if !validRenewalStatus(status) {
		httpx.WriteError(w, r, httpx.BadRequest(
			"`status` must be one of: pending, approved, rejected",
			map[string]any{"field": "status"}))
		return
	}
	limit := clampInt(q.Get("limit"), 100, 1, 500)

	renewals, err := s.store.ListRenewals(status, limit)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to list renewals").WithCause(err))
		return
	}
	if renewals == nil {
		renewals = []domain.KeyRenewal{}
	}
	httpx.OK(w, r, renewals)
}

// handleAdminApproveRenewal extends the key. With no `expiresInHours` the
// agent's requested lifetime is granted.
func (s *Server) handleAdminApproveRenewal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, err := readOptionalJSONObject(r, 1<<16)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var hours float64
	if v, ok := body["expiresInHours"]; ok && v != nil {
		if hours, err = nonNegativeHours(v, "expiresInHours"); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	} else {
		renewal, err := s.store.GetRenewal(id)
		if err != nil {
			httpx.WriteError(w, r, httpx.Internal("Failed to load renewal").WithCause(err))
			return
		}
		if renewal == nil {
			httpx.WriteError(w, r, httpx.NotFound("Renewal not found"))
			return
		}
		// Documented contract: an absent expiresInHours honours the requested
		// amount. But 0 on a renewal means "never expires", which is the highest
		// privilege the system can grant, and it must not be reachable by a
		// requester-supplied field passing through an empty approval — so a
		// non-positive request falls back to the server default instead.
		// Requesters never send 0 on purpose (the dashboard maps 0 to 24h), so
		// this only closes the bare-API path.
		if renewal.RequestedHours > 0 {
			hours = renewal.RequestedHours
		} else {
			hours = defaultApprovedKeyHours
		}
	}

	if _, err := s.store.DecideRenewal(id, true, hours, store.NowMS()); err != nil {
		writeKeyLifecycleError(w, r, err)
		return
	}
	s.writeRenewalResult(w, r, id)
}

// handleAdminRejectRenewal refuses a renewal without touching the key.
func (s *Server) handleAdminRejectRenewal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.DecideRenewal(id, false, 0, store.NowMS()); err != nil {
		writeKeyLifecycleError(w, r, err)
		return
	}
	s.writeRenewalResult(w, r, id)
}

// ---------------------------------------------------------------------------
// payloads and error mapping
// ---------------------------------------------------------------------------

// publicApplication is the subset of an application an unauthenticated poller
// may see. It is a curated object, not the domain type: the row also carries the
// approver's note, the requester's IP and the parked decision, none of which the
// contract exposes.
func publicApplication(a *domain.KeyApplication) map[string]any {
	if a == nil {
		return map[string]any{}
	}
	return map[string]any{
		"id":         a.ID,
		"status":     string(a.Status),
		"label":      a.Label,
		"created_at": a.CreatedAt,
		"expires_at": a.ExpiresAt,
		"decided_at": a.DecidedAt,
	}
}

// claimedKeyPayload is the one-time hand-over of a derived credential.
func claimedKeyPayload(key *domain.AgentKey, token string) map[string]any {
	if key == nil {
		return map[string]any{"token": token}
	}
	return map[string]any{
		"token":        token,
		"id":           key.ID,
		"name":         key.Name,
		"token_prefix": key.TokenPrefix,
		"expires_at":   key.ExpiresAt,
	}
}

func (s *Server) renewalCreatedPayload(renewal *domain.KeyRenewal) map[string]any {
	return map[string]any{
		"id":              renewal.ID,
		"key_id":          renewal.KeyID,
		"status":          string(renewal.Status),
		"requested_hours": renewal.RequestedHours,
		"created_at":      renewal.CreatedAt,
		"claim_secret":    s.renewalClaimSecret(renewal.ID, renewal.KeyID),
	}
}

// writeApplicationResult re-reads the decided application so the dashboard sees
// the stored row rather than an echo of its own request.
func (s *Server) writeApplicationResult(w http.ResponseWriter, r *http.Request, id string) {
	app, err := s.store.GetApplication(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load key application").WithCause(err))
		return
	}
	if app == nil {
		httpx.WriteError(w, r, httpx.NotFound("Key application not found"))
		return
	}
	httpx.OK(w, r, app)
}

func (s *Server) writeRenewalResult(w http.ResponseWriter, r *http.Request, id string) {
	renewal, err := s.store.GetRenewal(id)
	if err != nil {
		httpx.WriteError(w, r, httpx.Internal("Failed to load renewal").WithCause(err))
		return
	}
	if renewal == nil {
		httpx.WriteError(w, r, httpx.NotFound("Renewal not found"))
		return
	}
	httpx.OK(w, r, renewal)
}

// writeDecisionError maps a failed approve/reject.
//
// A decision that matched no row is usually "already decided", but it is also
// exactly what an expired row produces once the sweep has run: the row is no
// longer pending, yet nobody ever decided anything. Answering "already decided"
// there is misleading — the agent polling the same application is told 410 gone
// — so the row is re-read and the two cases are separated.
func (s *Server) writeDecisionError(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, store.ErrApplicationDecided) {
		app, readErr := s.store.GetApplication(id)
		if readErr != nil {
			httpx.WriteError(w, r, httpx.Internal("Failed to load key application").WithCause(readErr))
			return
		}
		if app != nil && app.Status == domain.ApplicationExpired {
			httpx.WriteError(w, r, httpx.Gone("This key application has expired"))
			return
		}
	}
	writeKeyLifecycleError(w, r, err)
}

// writeKeyLifecycleError maps store sentinels onto the frozen status codes.
//
// The distinctions matter to the caller: 404 is "you may not see this" (absent
// and not-yours are indistinguishable by design), 409 is "your request
// conflicts with the row's state", 403 means a human rejected it, and 410 means
// it timed out.
func writeKeyLifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrApplicationMissing),
		errors.Is(err, store.ErrClaimMismatch),
		errors.Is(err, store.ErrApplicationClaimed),
		errors.Is(err, store.ErrKeyMissing),
		errors.Is(err, store.ErrRenewalMissing):
		httpx.WriteError(w, r, httpx.NotFound("Not found"))

	case errors.Is(err, store.ErrApplicationDecided):
		httpx.Fail(w, r, http.StatusConflict, "already_decided",
			"This application or renewal was already decided", nil)
	case errors.Is(err, store.ErrRenewalExists):
		httpx.Fail(w, r, http.StatusConflict, "renewal_exists",
			"A renewal is already pending for this key", nil)
	case errors.Is(err, store.ErrMaxPending):
		httpx.Fail(w, r, http.StatusConflict, "too_many_pending",
			"Too many applications are already awaiting review", nil)
	case errors.Is(err, store.ErrMaxActive):
		httpx.Fail(w, r, http.StatusConflict, "too_many_active_keys",
			"Too many active keys already exist", nil)

	case errors.Is(err, store.ErrApplicationRejected):
		httpx.Fail(w, r, http.StatusForbidden, "rejected",
			"This key application was rejected", nil)

	case errors.Is(err, store.ErrApplicationExpired):
		httpx.WriteError(w, r, httpx.Gone("This key application has expired"))
	case errors.Is(err, store.ErrClaimWindowClosed):
		httpx.WriteError(w, r, httpx.Gone("The claim window for this application has closed"))

	default:
		httpx.WriteError(w, r, httpx.Internal("Key operation failed").WithCause(err))
	}
}

func validApplicationStatus(status string) bool {
	switch domain.ApplicationStatus(status) {
	case domain.ApplicationPending, domain.ApplicationApproved, domain.ApplicationRejected,
		domain.ApplicationClaimed, domain.ApplicationExpired:
		return true
	}
	return false
}

func validRenewalStatus(status string) bool {
	switch domain.RenewalStatus(status) {
	case domain.RenewalPending, domain.RenewalApproved, domain.RenewalRejected:
		return true
	}
	return false
}

// readOptionalJSONObject tolerates an absent body, which several action
// endpoints allow ("approve with defaults", "reject with no reason").
func readOptionalJSONObject(r *http.Request, limit int64) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return nil, httpx.BadRequest("Could not read request body", nil)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	parsed, err := validateJSON(raw)
	if err != nil {
		return nil, err
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, httpx.BadRequest("Request body must be a JSON object", nil)
	}
	return obj, nil
}

// readOptionalHours reads an optional non-negative hour count from a body.
func readOptionalHours(body map[string]any, field string) (float64, error) {
	v, ok := body[field]
	if !ok || v == nil {
		return 0, nil
	}
	return nonNegativeHours(v, field)
}

// nonNegativeHours applies the same JS Number() coercion the rest of the API
// uses, and rejects values outside the range a key lifetime can meaningfully
// take rather than silently clamping them.
//
// The upper bound exists because float64 -> int64 conversion saturates: a body
// of {"expiresInHours":1e300} used to round-trip 200 with a negative expires_at
// written to the database, i.e. the API claimed success while storing a value
// outside the contract. NaN and ±Inf never get this far — jsNumber rejects them.
func nonNegativeHours(v any, field string) (float64, error) {
	hours, ok := jsNumber(v)
	if !ok || hours < 0 {
		return 0, httpx.BadRequest("`"+field+"` must be a non-negative number",
			map[string]any{"field": field})
	}
	if hours > maxKeyLifetimeHours {
		return 0, httpx.BadRequest(
			"`"+field+"` must be at most "+strconv.Itoa(maxKeyLifetimeHours)+" hours (100 years)",
			map[string]any{"field": field})
	}
	return hours, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// requireVisibleString validates a single-line, human-facing identifier
// (label, name): length bounds as usual, plus no control characters and no
// invisible formatting characters, and at least one visible character when a
// minimum length is required.
//
// The invisible-character rule matters because these values are shown to a
// human. A label of "\u200b" renders as nothing at all, and NUL truncates
// display in some tooling while still being stored; either lets a caller put a
// row in the approval queue that an operator cannot actually read.
func requireVisibleString(value any, field string, min, max int) (string, error) {
	return requireVisible(value, field, min, max, false)
}

// requireVisibleText is the multi-line variant for prose fields (purpose, note,
// reason): tabs and newlines are legitimate there, every other control
// character and every invisible formatting character is not.
func requireVisibleText(value any, field string, min, max int) (string, error) {
	return requireVisible(value, field, min, max, true)
}

func requireVisible(value any, field string, min, max int, multiline bool) (string, error) {
	s, err := requireString(value, field, min, max)
	if err != nil {
		return "", err
	}

	visible := 0
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			if !multiline {
				return "", httpx.BadRequest("`"+field+"` must be a single line",
					map[string]any{"field": field})
			}
		case unicode.IsControl(r):
			// Covers NUL and the rest of Cc.
			return "", httpx.BadRequest("`"+field+"` must not contain control characters",
				map[string]any{"field": field})
		case unicode.Is(unicode.Cf, r):
			// Zero-width spaces/joiners, BOM, soft hyphen and the bidi overrides:
			// invisible, and the bidi ones can reorder what a reader sees.
			return "", httpx.BadRequest("`"+field+"` must not contain invisible formatting characters",
				map[string]any{"field": field})
		case !unicode.IsSpace(r):
			visible++
		}
	}
	if min > 0 && visible == 0 {
		return "", httpx.BadRequest("`"+field+"` must contain at least one visible character",
			map[string]any{"field": field})
	}
	return s, nil
}
