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
