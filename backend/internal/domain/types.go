// Package domain holds the business types shared by the store, services and
// HTTP handlers.
package domain

// ReportFormat is the stored content format.
type ReportFormat string

const (
	FormatMarkdown ReportFormat = "markdown"
	FormatHTML     ReportFormat = "html"
)

// Report is a single published report.
type Report struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Category  string         `json:"category"`
	Format    ReportFormat   `json:"format"`
	Content   string         `json:"content,omitempty"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt int64          `json:"created_at"`
	UpdatedAt int64          `json:"updated_at"`
	// OwnerKeyID is the agent_keys.id that published this report, or "" when it
	// came from the break-glass AGENT_SECRET_KEY (or from a deployment that
	// predates keys). Ownership is what scopes an agent's read and revoke rights.
	OwnerKeyID string `json:"owner_key_id,omitempty"`
}

// ShareToken is one independently-expiring link to a report.
type ShareToken struct {
	Token     string `json:"token"`
	ReportID  string `json:"report_id"`
	CreatedAt int64  `json:"created_at"`
	// ExpiresAt is epoch milliseconds; 0 means "never expires".
	ExpiresAt int64 `json:"expires_at"`
	IsActive  bool  `json:"is_active"`
	ViewCount int64 `json:"view_count"`
	// URL is the absolute public share URL, filled in by the API layer.
	URL string `json:"url,omitempty"`
}

// CreateReportInput is the validated payload for POST /api/reports.
type CreateReportInput struct {
	Title    string
	Category string
	Format   ReportFormat
	Content  string
	Metadata map[string]any
	// AutoShareHours is a pointer so "field absent" is distinguishable from an
	// explicit 0 (= never expires). Fractional hours are accepted, matching the
	// original Number() coercion.
	AutoShareHours *float64
	// OwnerKeyID is the authenticated principal's key id, or "" for root. Set by
	// the API layer from the request context; never from the request body, or a
	// caller could claim authorship of a report it did not publish.
	OwnerKeyID string
}

// ReportPatch is a partial update to a report. A nil field means "not provided
// and therefore unchanged", which is why every field is a pointer or a nil-able
// map: PATCH must be able to distinguish "set this to X" from "leave it alone",
// and for metadata "set it to {}" from "leave it alone".
//
// There is deliberately no OwnerKeyID field. Ownership is a security boundary,
// not data: a caller must not be able to hand a report to another key, nor
// claim one, by patching it. A request body carrying owner_key_id is rejected
// outright rather than ignored.
type ReportPatch struct {
	Title    *string
	Category *string
	Format   *ReportFormat
	Content  *string
	// Metadata replaces the stored object wholesale. Merging cannot express
	// "delete this key" (is null a deletion or a null value?), so replacement is
	// the only unambiguous choice; send {} to clear it.
	Metadata map[string]any
}

// Provided reports whether the patch carries at least one field to change.
// An empty patch is refused by the API layer: it would bump updated_at and make
// the share page advertise an update that never happened.
func (p ReportPatch) Provided() bool {
	return p.Title != nil || p.Category != nil || p.Format != nil ||
		p.Content != nil || p.Metadata != nil
}

// Resolution is the outcome of resolving a share token.
type Resolution struct {
	Status ResolutionStatus
	Report *Report
	Share  *ShareToken
}

type ResolutionStatus int

const (
	StatusOK ResolutionStatus = iota
	StatusMissing
	StatusGone
)
