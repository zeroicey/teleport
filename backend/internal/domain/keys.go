package domain

// Principal is the authenticated caller of an agent-facing endpoint.
//
// A request either presents the single break-glass AGENT_SECRET_KEY (Root) or a
// row from agent_keys. Root is the escape hatch that keeps the pre-existing
// deployment working and retains access to everything; a database-backed key is
// named and scoped, and may only touch the reports it published itself.
type Principal struct {
	// KeyID is the agent_keys.id of the presented key. It is empty for Root,
	// and empty is also what reports published by Root store as their owner —
	// so "no owner" and "root owns it" are deliberately the same value.
	KeyID string
	// Name is the human label given to the key at approval time. Empty for Root.
	Name string
	// Root is true when the caller presented AGENT_SECRET_KEY.
	Root bool
}

// AgentKey is an issued, named credential.
//
// TokenHash is the sha256 (hex) of the bearer token. The plaintext is never
// persisted: it exists only inside the response that first hands it over. This
// type is therefore safe to serialize except for TokenHash, which the JSON tag
// deliberately omits so it cannot leak into a response by accident.
type AgentKey struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	TokenPrefix  string `json:"token_prefix"`
	TokenHash    string `json:"-"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at"` // 0 = never expires
	RevokedAt    int64  `json:"revoked_at"` // 0 = active
	LastUsedAt   int64  `json:"last_used_at"`
	RequestCount int64  `json:"request_count"`
	Note         string `json:"note"`
}

// Active reports whether the key may be used at the given time.
//
// Expiry is compared against expires_at == 0 meaning "never"; revocation is
// absolute, so a revoked key stays revoked even if its expiry is in the future.
func (k *AgentKey) Active(nowMS int64) bool {
	if k == nil || k.RevokedAt != 0 {
		return false
	}
	return k.ExpiresAt == 0 || k.ExpiresAt > nowMS
}

// ApplicationStatus is the lifecycle of a key application.
type ApplicationStatus string

const (
	ApplicationPending  ApplicationStatus = "pending"
	ApplicationApproved ApplicationStatus = "approved"
	ApplicationRejected ApplicationStatus = "rejected"
	ApplicationClaimed  ApplicationStatus = "claimed"
	ApplicationExpired  ApplicationStatus = "expired"
)

// KeyApplication is an agent's request for a credential, awaiting a human.
//
// ClaimSecret is handed to the agent once at submission and stored only as
// ClaimHash; it is what lets an unauthenticated caller poll for the outcome
// without the application id alone being enough to read someone else's request.
type KeyApplication struct {
	ID             string            `json:"id"`
	ClaimHash      string            `json:"-"`
	Label          string            `json:"label"`
	Purpose        string            `json:"purpose"`
	RequestedHours float64           `json:"requested_hours"` // 0 = unspecified
	Status         ApplicationStatus `json:"status"`
	CreatedAt      int64             `json:"created_at"`
	ExpiresAt      int64             `json:"expires_at"`
	DecidedAt      int64             `json:"decided_at"`
	ClaimDeadline  int64             `json:"claim_deadline"`
	IssuedKeyID    string            `json:"issued_key_id"`
	RequesterIP    string            `json:"requester_ip"`
	UserAgent      string            `json:"user_agent"`

	// The approval decision is parked here rather than written into AgentKey,
	// because the credential cannot exist until the agent claims it: the token
	// is derived from the claim secret, and the server holds only its hash until
	// then. These become the new key's name, lifetime and note at claim time.
	ApprovedName  string  `json:"approved_name"`
	ApprovedHours float64 `json:"approved_hours"`
	ApprovedNote  string  `json:"approved_note"`
}

// Renewable reports whether the application is still awaiting a decision.
func (a *KeyApplication) Renewable() bool {
	return a != nil && a.Status == ApplicationPending
}

// RenewalStatus is the lifecycle of a renewal request.
type RenewalStatus string

const (
	RenewalPending  RenewalStatus = "pending"
	RenewalApproved RenewalStatus = "approved"
	RenewalRejected RenewalStatus = "rejected"
)

// KeyRenewal is a request to extend an existing key's expiry.
//
// Renewals deliberately share the same human-approval requirement as first
// issuance: a key that could extend itself forever would make expiry decorative.
type KeyRenewal struct {
	ID               string        `json:"id"`
	KeyID            string        `json:"key_id"`
	RequestedHours   float64       `json:"requested_hours"`
	Status           RenewalStatus `json:"status"`
	CreatedAt        int64         `json:"created_at"`
	DecidedAt        int64         `json:"decided_at"`
	GrantedExpiresAt int64         `json:"granted_expires_at"`
}
